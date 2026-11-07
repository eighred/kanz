package provision

import (
	"context"
	"errors"
	"strconv"

	"github.com/segmentio/kafka-go"
)

var (
	ErrBroker   = errors.New("broker observation unavailable")
	ErrConflict = errors.New("existing resource differs")
	ErrMissing  = errors.New("required resource absent")
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
	ClusterID        string `json:"expected_cluster_id"`
	Mode             string `json:"mode"`
	ManifestSHA      string `json:"manifest_sha256"`
	VerifiedTopics   int    `json:"verified_topics"`
	MissingTopics    int    `json:"missing_topics"`
	VerifiedACLs     int    `json:"verified_acls"`
	TopicAttempts    int    `json:"topic_attempts"`
	ACLAttempts      int    `json:"acl_attempts"`
	PartialOrUnknown bool   `json:"partial_or_unknown"`
	Complete         bool   `json:"complete"`
}

func Run(ctx context.Context, api Admin, p Plan, mode string) (Receipt, error) {
	r := Receipt{Mode: mode, ManifestSHA: p.ManifestSHA, ClusterID: p.ClusterID}
	if (mode != "plan" && mode != "apply" && mode != "verify") || !clusterIdentity.MatchString(p.ClusterID) || api == nil || len(p.Topics) == 0 || len(p.Topics) > 256 || len(p.ACLs) != 6 {
		return r, ErrInput
	}
	var missing []Topic
	// Observe EVERY existing topic and all target-principal ACLs before the
	// first mutation. Unknown observations cannot be converted into absence.
	for _, t := range p.Topics {
		exists, err := checkTopic(ctx, api, t, p.ClusterID)
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
	for _, t := range missing {
		if ctx.Err() != nil {
			return r, ErrBroker
		}
		exists, err := checkTopic(ctx, api, t, p.ClusterID)
		if err != nil {
			return r, err
		}
		if exists {
			continue
		}
		r.TopicAttempts++
		r.PartialOrUnknown = true
		result, err := api.CreateTopics(ctx, &kafka.CreateTopicsRequest{Topics: []kafka.TopicConfig{{Topic: t.Name, NumPartitions: t.Partitions, ReplicationFactor: 3, ConfigEntries: []kafka.ConfigEntry{{ConfigName: "cleanup.policy", ConfigValue: t.Cleanup}, {ConfigName: "retention.ms", ConfigValue: strconv.FormatInt(t.Retention, 10)}, {ConfigName: "min.insync.replicas", ConfigValue: "2"}}}}})
		if err != nil || result == nil || len(result.Errors) != 1 {
			return r, ErrBroker
		}
		resultErr, ok := result.Errors[t.Name]
		if !ok || (resultErr != nil && !errors.Is(resultErr, kafka.TopicAlreadyExists)) {
			return r, ErrBroker
		}
	}
	// CreateTopics acknowledgement is not readiness. A missing or mismatched
	// readback refuses grants; a later explicit invocation can retry safely.
	r.VerifiedTopics = 0
	for _, t := range p.Topics {
		exists, err := checkTopic(ctx, api, t, p.ClusterID)
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
			return r, ErrBroker
		}
		r.ACLAttempts = len(acls)
		r.PartialOrUnknown = true
		result, err := api.CreateACLs(ctx, &kafka.CreateACLsRequest{ACLs: acls})
		if err != nil || result == nil || len(result.Errors) != len(acls) {
			return r, ErrBroker
		}
		for _, e := range result.Errors {
			if e != nil {
				return r, ErrBroker
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

func checkTopic(ctx context.Context, api Admin, want Topic, clusterID string) (bool, error) {
	m, err := api.Metadata(ctx, &kafka.MetadataRequest{Topics: []string{want.Name}})
	if err != nil || m == nil || len(m.Topics) != 1 || m.Topics[0].Name != want.Name {
		return false, ErrBroker
	}
	if m.ClusterID != clusterID || m.ClusterID == "" {
		return false, ErrConflict
	}
	// kafka-go resolves partition IDs through the response broker map; an
	// unknown leader/replica becomes a zero Broker. Require full known entries,
	// not counts or ID zero alone (zero is a legitimate broker ID).
	if len(m.Brokers) < 3 || len(m.Brokers) > 1024 {
		return false, ErrConflict
	}
	brokers := make(map[int]kafka.Broker, len(m.Brokers))
	for _, broker := range m.Brokers {
		if broker.ID < 0 || broker.Host == "" || len(broker.Host) > 512 || broker.Port < 1 || broker.Port > 65535 {
			return false, ErrConflict
		}
		if _, exists := brokers[broker.ID]; exists {
			return false, ErrConflict
		}
		brokers[broker.ID] = broker
	}
	t := m.Topics[0]
	if errors.Is(t.Error, kafka.UnknownTopicOrPartition) {
		return false, nil
	}
	if t.Error != nil {
		return false, ErrBroker
	}
	if t.Internal || len(t.Partitions) != want.Partitions {
		return false, ErrConflict
	}
	seen := make(map[int]bool)
	for _, p := range t.Partitions {
		if p.Error != nil || p.ID < 0 || p.ID >= want.Partitions || seen[p.ID] || len(p.Replicas) != 3 || len(p.Isr) < 2 || len(p.Isr) > 3 {
			return false, ErrConflict
		}
		replicas, isr := make(map[int]bool), make(map[int]bool)
		for _, broker := range p.Replicas {
			known, exists := brokers[broker.ID]
			if !exists || known != broker || replicas[broker.ID] {
				return false, ErrConflict
			}
			replicas[broker.ID] = true
		}
		for _, broker := range p.Isr {
			known, exists := brokers[broker.ID]
			if !exists || known != broker || !replicas[broker.ID] || isr[broker.ID] {
				return false, ErrConflict
			}
			isr[broker.ID] = true
		}
		leader, exists := brokers[p.Leader.ID]
		if !exists || leader != p.Leader || !replicas[p.Leader.ID] || !isr[p.Leader.ID] {
			return false, ErrConflict
		}
		seen[p.ID] = true
	}
	c, err := api.DescribeConfigs(ctx, &kafka.DescribeConfigsRequest{Resources: []kafka.DescribeConfigRequestResource{{ResourceType: kafka.ResourceTypeTopic, ResourceName: want.Name, ConfigNames: []string{"cleanup.policy", "retention.ms", "min.insync.replicas"}}}})
	if err != nil || c == nil || len(c.Resources) != 1 || c.Resources[0].Error != nil || c.Resources[0].ResourceName != want.Name || c.Resources[0].ResourceType != int8(kafka.ResourceTypeTopic) {
		return false, ErrBroker
	}
	expected := map[string]string{"cleanup.policy": want.Cleanup, "retention.ms": strconv.FormatInt(want.Retention, 10), "min.insync.replicas": "2"}
	if len(c.Resources[0].ConfigEntries) != len(expected) {
		return false, ErrConflict
	}
	for _, e := range c.Resources[0].ConfigEntries {
		v, ok := expected[e.ConfigName]
		if !ok || e.IsSensitive || e.ConfigValue != v {
			return false, ErrConflict
		}
		delete(expected, e.ConfigName)
	}
	return len(expected) == 0, nil
}

func missingACLs(ctx context.Context, api Admin, p Plan) ([]kafka.ACLEntry, error) {
	r, err := api.DescribeACLs(ctx, &kafka.DescribeACLsRequest{Filter: kafka.ACLFilter{ResourceTypeFilter: kafka.ResourceTypeAny, ResourcePatternTypeFilter: kafka.PatternTypeAny, PrincipalFilter: p.Principal, Operation: kafka.ACLOperationTypeAny, PermissionType: kafka.ACLPermissionTypeAny}})
	if err != nil || r == nil || r.Error != nil || len(r.Resources) > 6 {
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
