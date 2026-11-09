package provision

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/eighred/kanz/internal/topic"
	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/protocol"
)

// Keep the underlying observation available for closed classification without
// making a broker's free-form error text part of the public error or receipt.
type brokerCause struct{ cause error }

type conflictCause struct{ code string }

func (e conflictCause) Error() string { return ErrConflict.Error() }
func (e conflictCause) Unwrap() error { return ErrConflict }

func (e brokerCause) Error() string   { return ErrBroker.Error() }
func (e brokerCause) Unwrap() []error { return []error{ErrBroker, e.cause} }

// FailureCode projects only a bounded protocol code or fixed transport class.
func FailureCode(err error) string {
	if errors.Is(err, errTopicNotReady) {
		return "topic-not-ready"
	}
	var conflict conflictCause
	if errors.As(err, &conflict) {
		return conflict.code
	}
	var cause brokerCause
	if !errors.As(err, &cause) {
		return ""
	}
	var code kafka.Error
	if errors.As(cause.cause, &code) && code >= -1 && code <= 127 {
		return "kafka-" + strconv.Itoa(int(code))
	}
	if errors.Is(cause.cause, context.DeadlineExceeded) {
		return "deadline"
	}
	if errors.Is(cause.cause, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(cause.cause, io.EOF) {
		return "eof"
	}
	if errors.Is(cause.cause, io.ErrUnexpectedEOF) {
		return "unexpected-eof"
	}
	var invalid x509.CertificateInvalidError
	if errors.As(cause.cause, &invalid) {
		if invalid.Reason == x509.Expired {
			return "tls-expired"
		}
		return "tls-invalid"
	}
	var authority x509.UnknownAuthorityError
	if errors.As(cause.cause, &authority) {
		return "tls-authority"
	}
	var verification *tls.CertificateVerificationError
	if errors.As(cause.cause, &verification) {
		return "tls-certificate"
	}
	var protocolError protocol.Error
	if errors.As(cause.cause, &protocolError) {
		return "protocol"
	}
	var network net.Error
	if errors.As(cause.cause, &network) {
		if network.Timeout() {
			return "timeout"
		}
		return "network"
	}
	return "unknown"
}

var (
	ErrBroker        = errors.New("broker observation unavailable")
	ErrConflict      = errors.New("existing resource differs")
	ErrMissing       = errors.New("required resource absent")
	errTopicNotReady = errors.New("topic readiness not yet observed")
)

type Admin interface {
	Metadata(context.Context, *kafka.MetadataRequest) (*kafka.MetadataResponse, error)
	DescribeConfigs(context.Context, *kafka.DescribeConfigsRequest) (*kafka.DescribeConfigsResponse, error)
	DescribeACLs(context.Context, *kafka.DescribeACLsRequest) (*kafka.DescribeACLsResponse, error)
	CreateTopics(context.Context, *kafka.CreateTopicsRequest) (*kafka.CreateTopicsResponse, error)
	CreateACLs(context.Context, *kafka.CreateACLsRequest) (*kafka.CreateACLsResponse, error)
}

// Receipt counts observations and write attempts, not atomic commits. Any
// failed write/readback leaves PartialOrUnknown true, including response loss.
// No automatic delete or retention rollback can safely undo broker operations.
type Receipt struct {
	DurabilityProfile string `json:"durability_profile"`
	ClusterID         string `json:"expected_cluster_id"`
	Mode              string `json:"mode"`
	ManifestSHA       string `json:"manifest_sha256"`
	VerifiedTopics    int    `json:"verified_topics"`
	MissingTopics     int    `json:"missing_topics"`
	VerifiedACLs      int    `json:"verified_acls"`
	TopicAttempts     int    `json:"topic_attempts"`
	ACLAttempts       int    `json:"acl_attempts"`
	PartialOrUnknown  bool   `json:"partial_or_unknown"`
	Complete          bool   `json:"complete"`
}

func Run(ctx context.Context, api Admin, p Plan, mode string) (Receipt, error) {
	r := Receipt{Mode: mode, ManifestSHA: p.ManifestSHA, ClusterID: p.ClusterID, DurabilityProfile: p.DurabilityProfile}
	system := p.Tenant == topic.SystemTenant
	validScope := (system && p.Principal == "" && p.BrokerPrincipal == "" && len(p.ACLs) == 0) ||
		(!system && p.BrokerPrincipal != "" && len(p.ACLs) == 6)
	if (mode != "plan" && mode != "apply" && mode != "verify") || (p.DurabilityProfile != "kafka-min-isr" && p.DurabilityProfile != "redpanda-raft") || !ValidClusterID(p.ClusterID) || !validScope || api == nil || len(p.Topics) == 0 || len(p.Topics) > 256 {
		return r, ErrInput
	}
	var missing []Topic
	// Observe EVERY existing topic and all target-principal ACLs before the
	// first mutation. Unknown observations cannot be converted into absence.
	for _, t := range p.Topics {
		exists, err := checkTopic(ctx, api, t, p.ClusterID, p.DurabilityProfile)
		if err != nil {
			return r, err
		}
		if exists {
			r.VerifiedTopics++
		} else {
			missing = append(missing, t)
		}
	}
	r.MissingTopics = len(missing)
	acls, err := missingACLs(ctx, api, p)
	if err != nil {
		return r, err
	}
	r.VerifiedACLs = len(p.ACLs) - len(acls)
	if mode == "plan" {
		return r, nil
	}
	if mode == "verify" && (len(missing) != 0 || len(acls) != 0) {
		return r, ErrMissing
	}
	created := make(map[string]bool)
	for _, t := range missing {
		if ctx.Err() != nil {
			return r, brokerCause{ctx.Err()}
		}
		exists, err := checkTopic(ctx, api, t, p.ClusterID, p.DurabilityProfile)
		if err != nil {
			return r, err
		}
		if exists {
			continue
		}
		r.TopicAttempts++
		r.PartialOrUnknown = true
		result, err := api.CreateTopics(ctx, &kafka.CreateTopicsRequest{Topics: []kafka.TopicConfig{{Topic: t.Name, NumPartitions: t.Partitions, ReplicationFactor: 3, ConfigEntries: topicConfigs(t, p.DurabilityProfile)}}})
		if err != nil {
			return r, brokerCause{err}
		}
		if result == nil || len(result.Errors) != 1 {
			return r, ErrBroker
		}
		resultErr, ok := result.Errors[t.Name]
		if resultErr != nil && !errors.Is(resultErr, kafka.TopicAlreadyExists) {
			return r, brokerCause{resultErr}
		}
		if !ok {
			return r, ErrBroker
		}
		created[t.Name] = true
	}
	// A create acknowledgement is not readiness. Only topics acknowledged in
	// this invocation receive bounded read-only readiness observation. Existing
	// conflicts and configuration mismatches still refuse immediately; no write
	// is retried and the original total context deadline always bounds the wait.
	r.VerifiedTopics = 0
	for _, t := range p.Topics {
		var exists bool
		var err error
		if created[t.Name] {
			err = waitCreatedTopic(ctx, api, t, p.ClusterID, p.DurabilityProfile)
			exists = err == nil
		} else {
			exists, err = checkTopic(ctx, api, t, p.ClusterID, p.DurabilityProfile)
		}
		if err != nil {
			return r, err
		}
		if !exists {
			return r, ErrMissing
		}
		r.VerifiedTopics++
	}
	if len(acls) != 0 {
		if ctx.Err() != nil {
			return r, brokerCause{ctx.Err()}
		}
		r.ACLAttempts = len(acls)
		r.PartialOrUnknown = true
		result, err := api.CreateACLs(ctx, &kafka.CreateACLsRequest{ACLs: acls})
		if err != nil {
			return r, brokerCause{err}
		}
		if result == nil || len(result.Errors) != len(acls) {
			return r, ErrBroker
		}
		for _, e := range result.Errors {
			if e != nil {
				return r, brokerCause{e}
			}
		}
	}
	remaining, err := missingACLs(ctx, api, p)
	if err != nil {
		return r, err
	}
	if len(remaining) != 0 {
		return r, ErrMissing
	}
	r.VerifiedACLs, r.MissingTopics = len(p.ACLs), 0
	r.Complete, r.PartialOrUnknown = true, false
	return r, nil
}

func waitCreatedTopic(ctx context.Context, api Admin, want Topic, clusterID, profile string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		exists, err := checkTopic(ctx, api, want, clusterID, profile)
		if err == nil && exists {
			return nil
		}
		if err != nil && !errors.Is(err, errTopicNotReady) {
			return err
		}
		select {
		case <-ctx.Done():
			return brokerCause{ctx.Err()}
		case <-tick.C:
		}
	}
}

func checkTopic(ctx context.Context, api Admin, want Topic, clusterID, profile string) (bool, error) {
	m, err := api.Metadata(ctx, &kafka.MetadataRequest{Topics: []string{want.Name}})
	if err != nil {
		return false, brokerCause{err}
	}
	if m == nil || len(m.Topics) != 1 || m.Topics[0].Name != want.Name {
		return false, ErrBroker
	}
	if m.ClusterID != clusterID || m.ClusterID == "" {
		return false, conflictCause{"estate"}
	}
	// kafka-go resolves partition IDs through the response broker map; an
	// unknown leader/replica becomes a zero Broker. Require full known entries,
	// not counts or ID zero alone (zero is a legitimate broker ID).
	if len(m.Brokers) < 3 || len(m.Brokers) > 1024 {
		return false, conflictCause{"broker-count"}
	}
	brokers := make(map[int]kafka.Broker, len(m.Brokers))
	for _, broker := range m.Brokers {
		if broker.ID < 0 || broker.Host == "" || len(broker.Host) > 512 || broker.Port < 1 || broker.Port > 65535 {
			return false, conflictCause{"broker-shape"}
		}
		if _, exists := brokers[broker.ID]; exists {
			return false, conflictCause{"broker-duplicate"}
		}
		brokers[broker.ID] = broker
	}
	t := m.Topics[0]
	if errors.Is(t.Error, kafka.UnknownTopicOrPartition) {
		return false, nil
	}
	if errors.Is(t.Error, kafka.LeaderNotAvailable) {
		return false, errors.Join(ErrConflict, errTopicNotReady)
	}
	if t.Error != nil {
		return false, brokerCause{t.Error}
	}
	if t.Internal || len(t.Partitions) != want.Partitions {
		return false, conflictCause{"topic-shape"}
	}
	seen := make(map[int]bool)
	for _, p := range t.Partitions {
		if p.Error != nil && !errors.Is(p.Error, kafka.LeaderNotAvailable) {
			return false, brokerCause{p.Error}
		}
		if p.ID < 0 || p.ID >= want.Partitions || seen[p.ID] {
			return false, conflictCause{"partition-id"}
		}
		if len(p.Replicas) != 3 {
			return false, conflictCause{"replication-factor"}
		}
		if len(p.Isr) > 3 {
			return false, conflictCause{"isr-count"}
		}
		replicas, isr := make(map[int]bool), make(map[int]bool)
		for _, broker := range p.Replicas {
			known, exists := brokers[broker.ID]
			if !exists || known != broker || replicas[broker.ID] {
				return false, conflictCause{"replica-shape"}
			}
			replicas[broker.ID] = true
		}
		for _, broker := range p.Isr {
			known, exists := brokers[broker.ID]
			if !exists || known != broker || !replicas[broker.ID] || isr[broker.ID] {
				return false, conflictCause{"isr-shape"}
			}
			isr[broker.ID] = true
		}
		leader, exists := brokers[p.Leader.ID]
		if p.Error != nil || len(isr) < 2 || p.Leader.Host == "" {
			return false, errors.Join(ErrConflict, errTopicNotReady)
		}
		if !exists || leader != p.Leader || !replicas[p.Leader.ID] {
			return false, conflictCause{"leader-shape"}
		}
		if !isr[p.Leader.ID] {
			return false, errors.Join(ErrConflict, errTopicNotReady)
		}
		seen[p.ID] = true
	}
	expected := make(map[string]string)
	var names []string
	for _, entry := range topicConfigs(want, profile) {
		expected[entry.ConfigName] = entry.ConfigValue
		names = append(names, entry.ConfigName)
	}
	c, err := api.DescribeConfigs(ctx, &kafka.DescribeConfigsRequest{Resources: []kafka.DescribeConfigRequestResource{{ResourceType: kafka.ResourceTypeTopic, ResourceName: want.Name, ConfigNames: names}}})
	if err != nil {
		return false, brokerCause{err}
	}
	if c != nil && len(c.Resources) == 1 && c.Resources[0].Error != nil {
		return false, brokerCause{c.Resources[0].Error}
	}
	if c == nil || len(c.Resources) != 1 || c.Resources[0].ResourceName != want.Name || c.Resources[0].ResourceType != int8(kafka.ResourceTypeTopic) {
		return false, ErrBroker
	}
	if len(c.Resources[0].ConfigEntries) != len(expected) {
		return false, conflictCause{"config-count"}
	}
	for _, e := range c.Resources[0].ConfigEntries {
		v, ok := expected[e.ConfigName]
		if !ok || e.IsSensitive || e.ConfigValue != v {
			code := "config-key"
			switch e.ConfigName {
			case "cleanup.policy":
				code = "config-cleanup"
			case "retention.ms":
				code = "config-retention"
			case "min.insync.replicas":
				code = "config-min-isr"
			case "write.caching":
				code = "config-write-caching"
			}
			return false, conflictCause{code}
		}
		delete(expected, e.ConfigName)
	}
	return len(expected) == 0, nil
}

func missingACLs(ctx context.Context, api Admin, p Plan) ([]kafka.ACLEntry, error) {
	if p.Tenant == topic.SystemTenant {
		return nil, nil
	}
	r, err := api.DescribeACLs(ctx, &kafka.DescribeACLsRequest{Filter: kafka.ACLFilter{ResourceTypeFilter: kafka.ResourceTypeAny, ResourcePatternTypeFilter: kafka.PatternTypeAny, PrincipalFilter: p.BrokerPrincipal, Operation: kafka.ACLOperationTypeAny, PermissionType: kafka.ACLPermissionTypeAny}})
	if err != nil {
		return nil, brokerCause{err}
	}
	if r != nil && r.Error != nil {
		return nil, brokerCause{r.Error}
	}
	if r == nil || len(r.Resources) > 6 {
		return nil, ErrBroker
	}
	want := make(map[kafka.ACLEntry]bool)
	for _, a := range p.ACLs {
		want[a] = true
	}
	for _, resource := range r.Resources {
		if len(resource.ACLs) > 6 {
			return nil, ErrConflict
		}
		for _, a := range resource.ACLs {
			key := kafka.ACLEntry{ResourceType: resource.ResourceType, ResourceName: resource.ResourceName, ResourcePatternType: resource.PatternType, Principal: a.Principal, Host: a.Host, Operation: a.Operation, PermissionType: a.PermissionType}
			if !want[key] {
				return nil, ErrConflict
			}
			delete(want, key)
		}
	}
	var missing []kafka.ACLEntry
	for _, a := range p.ACLs {
		if want[a] {
			missing = append(missing, a)
		}
	}
	return missing, nil
}
