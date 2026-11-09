// This test executable runs only in the disposable, source-bound SPIRE fixture.
// It has no production fault flags and is not the admin image entrypoint.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/cmd/kanz-kafka-provision/internal/provision"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/segmentio/kafka-go"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

const nativeAddress = "native-redpanda-0.kanz-messaging.svc:9094"
const nativePeer = "spiffe://kanz.internal/ns/kanz-messaging/sa/redpanda"

type nativeObservation struct {
	Version   string `json:"version"`
	Mode      string `json:"mode"`
	ClusterID string `json:"cluster_id"`
	Identity  string `json:"identity"`
	UniqueID  bool   `json:"unique_id"`
	Passed    bool   `json:"passed"`
	Renewed   bool   `json:"renewed"`
}

func TestNativeBrokerQualification(t *testing.T) {
	mode := os.Getenv("KANZ_NATIVE_FIXTURE_MODE")
	if mode == "" {
		t.Skip("actual disposable SPIRE/broker fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	diagnostic := nativeDiagnostic{Version: version.String(), Mode: mode, Phase: "workload_api"}
	observation, err := nativeQualification(ctx, mode, &diagnostic)
	diagnostic.Category = nativeErrorCategory(err)
	closed, encodingErr := json.Marshal(diagnostic)
	if encodingErr != nil {
		t.Fatal("closed diagnostic encoding")
	}
	fmt.Printf("NATIVE_DIAGNOSTIC:%s\n", closed)
	if err != nil {
		t.Fatal("native broker qualification refused")
	}
	raw, err := json.Marshal(observation)
	if err != nil {
		t.Fatal("closed observation encoding")
	}
	fmt.Printf("NATIVE_PROOF:%s\n", raw)
}

type nativeDiagnostic struct {
	Version          string `json:"version"`
	Mode             string `json:"mode"`
	ExpectedIdentity string `json:"expected_identity"`
	Phase            string `json:"phase"`
	Category         string `json:"category"`
}

func nativeErrorCategory(err error) string {
	var code kafka.Error
	if errors.As(err, &code) && code >= -1 && code <= 127 {
		return "kafka-" + strconv.Itoa(int(code))
	}
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, provision.ErrInput):
		return "input"
	case errors.Is(err, provision.ErrBroker):
		return "broker"
	case errors.Is(err, provision.ErrConflict):
		return "conflict"
	default:
		return "unknown"
	}
}

func TestNativeDiagnosticCategories(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{{nil, "ok"}, {context.DeadlineExceeded, "deadline"}, {context.Canceled, "cancelled"},
		{kafka.RequestTimedOut, "kafka-7"},
		{provision.ErrInput, "input"}, {provision.ErrBroker, "broker"}, {provision.ErrConflict, "conflict"},
		{errors.New("secret-error-sentinel"), "unknown"}} {
		if got := nativeErrorCategory(tc.err); got != tc.want {
			t.Fatalf("closed diagnostic category: got %s want %s", got, tc.want)
		}
	}
}

func nativeQualification(ctx context.Context, mode string, diagnostic *nativeDiagnostic) (nativeObservation, error) {
	out := nativeObservation{Version: version.String(), Mode: mode}
	identities := map[string]string{
		"discover":         "spiffe://kanz.internal/ns/kanz-messaging/sa/kafka-provisioner",
		"partial":          "spiffe://kanz.internal/ns/kanz-messaging/sa/kafka-provisioner",
		"history-before":   "spiffe://kanz.internal/ns/kanz-messaging/sa/kafka-provisioner",
		"history-active":   "spiffe://kanz.internal/ns/kanz-messaging/sa/kafka-provisioner",
		"history-rollback": "spiffe://kanz.internal/ns/kanz-messaging/sa/kafka-provisioner",
		"tenant-a":         "spiffe://kanz.internal/ns/tenant-acme/sa/risk-engine",
		"tenant-b":         "spiffe://kanz.internal/ns/tenant-bravo/sa/risk-engine",
		"compute-a":        "spiffe://kanz.internal/ns/kanz-services/sa/archiver-acme",
		"ungranted":        "spiffe://kanz.internal/ns/tenant-acme/sa/ungranted",
	}
	for _, name := range []string{"quorum-healthy", "quorum-majority", "quorum-minority", "quorum-restored"} {
		identities[name] = "spiffe://kanz.internal/ns/kanz-messaging/sa/kafka-provisioner"
	}
	id, ok := identities[mode]
	if !ok {
		return out, provision.ErrInput
	}
	diagnostic.ExpectedIdentity = id
	source, err := transport.NewSource(ctx, "unix:///run/spiffe/spire-agent.sock")
	if err != nil {
		return out, err
	}
	defer source.Close()
	diagnostic.Phase = "identity"
	svid, err := source.GetX509SVID()
	if err != nil || svid.ID.String() != id || len(svid.Certificates) == 0 {
		return out, provision.ErrInput
	}
	diagnostic.Phase = "uniqueid"
	// Require the actual uniqueid composer attribute, never synthesize a CN leaf.
	expected := sha256.Sum256([]byte(id))
	unique := false
	for _, name := range svid.Certificates[0].Subject.Names {
		if name.Type.String() == "2.5.4.45" && name.Value == hex.EncodeToString(expected[:16]) {
			unique = true
		}
	}
	if !unique {
		return out, provision.ErrInput
	}
	out.Identity, out.UniqueID = id, true
	serial := svid.Certificates[0].SerialNumber.String()
	peer := spiffeid.RequireFromString(nativePeer)
	config := transport.ClientTLSConfig(source, transport.AuthorizeServices(peer))
	tr := &kafka.Transport{TLS: config}
	defer tr.CloseIdleConnections()
	client := &kafka.Client{Addr: kafka.TCP(nativeAddress), Transport: tr}
	diagnostic.Phase = "metadata"
	var meta *kafka.MetadataResponse
	if strings.HasPrefix(mode, "quorum-") {
		meta, err = nativeQuorumMetadata(ctx, client, mode, os.Getenv("KANZ_NATIVE_EXPECTED_CLUSTER"))
	} else {
		meta, err = client.Metadata(ctx, &kafka.MetadataRequest{Topics: []string{}})
	}
	if err != nil {
		return out, err
	}
	if meta == nil || !provision.ValidClusterID(meta.ClusterID) || (!strings.HasPrefix(mode, "quorum-") && len(meta.Brokers) != 3) {
		return out, provision.ErrBroker
	}
	out.ClusterID = meta.ClusterID
	if mode == "discover" {
		out.Passed = true
		diagnostic.Phase = "complete"
		return out, nil
	}
	diagnostic.Phase = "estate"
	if meta.ClusterID != os.Getenv("KANZ_NATIVE_EXPECTED_CLUSTER") {
		return out, provision.ErrConflict
	}
	if strings.HasPrefix(mode, "quorum-") {
		diagnostic.Phase = "quorum"
		if err = nativeQuorum(ctx, client, mode); err != nil {
			return out, err
		}
		out.Passed = true
		diagnostic.Phase = "complete"
		return out, nil
	}
	if strings.HasPrefix(mode, "history-") {
		diagnostic.Phase = "quorum"
		if err = nativeSystemHistory(ctx, client, mode, meta.ClusterID, out.Version); err != nil {
			return out, err
		}
		out.Passed = true
		diagnostic.Phase = "complete"
		return out, nil
	}
	if mode == "partial" {
		diagnostic.Phase = "partial"
		if err = nativePartial(ctx, client, meta.ClusterID); err != nil {
			return out, err
		}
		out.Passed = true
		diagnostic.Phase = "complete"
		return out, nil
	}
	diagnostic.Phase = "acl"
	tenant, foreign := "acme", "bravo"
	if mode == "tenant-b" {
		tenant, foreign = foreign, tenant
	}
	topic := tenant + ".order.order"
	if mode == "ungranted" {
		if err = nativeDeniedTopic(ctx, config, topic); err != nil {
			return out, err
		}
		out.Passed = true
		diagnostic.Phase = "complete"
		return out, nil
	}
	value := "native-transport-" + mode
	diagnostic.Phase = "own-topic"
	if err = nativeProduce(ctx, client, topic, value); err != nil {
		return out, err
	}
	if err = nativeReadContains(ctx, client, topic, value); err != nil {
		return out, err
	}
	diagnostic.Phase = "foreign-topic"
	if err = nativeDeniedTopic(ctx, config, foreign+".order.order"); err != nil {
		return out, err
	}
	diagnostic.Phase = "own-group"
	if err = nativeGroup(ctx, client, tenant+".fixture", topic, false); err != nil {
		return out, err
	}
	diagnostic.Phase = "foreign-group"
	if err = nativeGroup(ctx, client, foreign+".fixture", topic, true); err != nil {
		return out, err
	}
	diagnostic.Phase = "admin-denial"
	denied, err := client.CreateACLs(ctx, &kafka.CreateACLsRequest{ACLs: []kafka.ACLEntry{{ResourceType: kafka.ResourceTypeTopic, ResourceName: foreign + ".", ResourcePatternType: kafka.PatternTypePrefixed, Principal: "User:" + id, Host: "*", Operation: kafka.ACLOperationTypeRead, PermissionType: kafka.ACLPermissionTypeAllow}}})
	if err != nil {
		if !errors.Is(err, kafka.ClusterAuthorizationFailed) {
			return out, provision.ErrBroker
		}
	} else if denied == nil || len(denied.Errors) != 1 || !errors.Is(denied.Errors[0], kafka.ClusterAuthorizationFailed) {
		return out, provision.ErrConflict
	}
	wrong := transport.ClientTLSConfig(source, transport.AuthorizeServices(spiffeid.RequireFromString("spiffe://kanz.internal/ns/kanz-messaging/sa/kafka")))
	raw, err := (&tls.Dialer{Config: wrong}).DialContext(ctx, "tcp", nativeAddress)
	if err == nil {
		raw.Close()
		return out, provision.ErrConflict
	}
	// The real short-lived SPIRE leaf must rotate; close pooled sockets to require
	// a new authenticated connection and repeat positive and negative operations.
	diagnostic.Phase = "renewal"
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, e := source.GetX509SVID()
		if e != nil || current.ID.String() != id {
			return out, provision.ErrInput
		}
		if current.Certificates[0].SerialNumber.String() != serial {
			break
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-ticker.C:
		}
	}
	diagnostic.Phase = "reconnect"
	tr.CloseIdleConnections()
	if err = nativeProduce(ctx, client, topic, value+"-renewed"); err != nil {
		return out, err
	}
	if err = nativeReadContains(ctx, client, topic, value+"-renewed"); err != nil {
		return out, err
	}
	if err = nativeDeniedTopic(ctx, config, foreign+".order.order"); err != nil {
		return out, err
	}
	out.Renewed, out.Passed = true, true
	diagnostic.Phase = "complete"
	return out, nil
}

// The fixture writes only synthetic markers. Rollback must read both
// acknowledged records without producing a replacement.
func nativeSystemHistory(ctx context.Context, c *kafka.Client, mode, cluster, source string) error {
	const topic = "order.order"
	wait, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := nativeWaitForQuorumLeader(wait, c, "quorum-healthy", topic, cluster); err != nil {
		return err
	}
	before := "native-history-before:" + source
	active := "native-history-active:" + source
	if mode == "history-before" {
		if err := nativeProduce(ctx, c, topic, before); err != nil {
			return err
		}
	}
	if err := nativeQuorumReadContains(ctx, c, topic, before); err != nil {
		return err
	}
	if mode == "history-before" {
		return nil
	}
	if mode == "history-active" {
		if err := nativeProduce(ctx, c, topic, active); err != nil {
			return err
		}
	}
	return nativeQuorumReadContains(ctx, c, topic, active)
}

func nativeQuorum(ctx context.Context, c *kafka.Client, mode string) error {
	const topic = "acme.order.order"
	if mode != "quorum-minority" {
		wait, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		if err := nativeWaitForQuorumLeader(wait, c, mode, topic, os.Getenv("KANZ_NATIVE_EXPECTED_CLUSTER")); err != nil {
			return err
		}
	}
	if mode == "quorum-minority" {
		// Metadata/TLS above must still work. No quorum must prevent an
		// acks=all success; a timeout remains unknown effects, never rollback.
		probe, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		err := nativeProduce(probe, c, topic, "quorum-unacknowledged-unknown")
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, kafka.RequestTimedOut) ||
			errors.Is(err, kafka.NotEnoughReplicas) || errors.Is(err, kafka.NotEnoughReplicasAfterAppend) ||
			errors.Is(err, kafka.NotLeaderForPartition) || errors.Is(err, kafka.LeaderNotAvailable) {
			return nil
		}
		if err != nil {
			return err
		}
		return provision.ErrConflict
	}
	if mode != "quorum-healthy" {
		if err := nativeQuorumReadContains(ctx, c, topic, "quorum-healthy"); err != nil {
			return err
		}
	}
	if mode == "quorum-restored" {
		if err := nativeQuorumReadContains(ctx, c, topic, "quorum-majority"); err != nil {
			return err
		}
	}
	if err := nativeProduce(ctx, c, topic, mode); err != nil {
		return err
	}
	return nativeQuorumReadContains(ctx, c, topic, mode)
}

// Broker membership can converge before partition leadership. Validate both
// in the same observation and refuse to treat kafka-go's zero Broker as ID 0.
func nativeWaitForQuorumLeader(ctx context.Context, c nativeMetadataReader, mode, topic, cluster string) error {
	return nativeWaitForObservation(ctx, func() error {
		m, err := c.Metadata(ctx, &kafka.MetadataRequest{Topics: []string{topic}})
		if err != nil {
			return err
		}
		if m == nil {
			return provision.ErrBroker
		}
		if !provision.ValidClusterID(m.ClusterID) || m.ClusterID != cluster {
			return provision.ErrConflict
		}
		ready, err := nativeBrokerView(m.Brokers, mode)
		if err != nil {
			return err
		}
		if ready && len(m.Topics) == 1 && m.Topics[0].Name == topic && m.Topics[0].Error == nil {
			for _, p := range m.Topics[0].Partitions {
				if p.ID != 0 || p.Error != nil || p.Leader.Host == "" {
					continue
				}
				for _, broker := range m.Brokers {
					if broker == p.Leader {
						return nil
					}
				}
			}
		}
		return kafka.LeaderNotAvailable
	})
}

func nativeQuorumReadContains(ctx context.Context, c *kafka.Client, topic, value string) error {
	wait, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	return nativeWaitForObservation(wait, func() error {
		return nativeReadContains(wait, c, topic, value)
	})
}

// Reobserve reads and metadata only. A missing marker must actually become
// visible, and an ambiguous produce is never replayed by this helper.
func nativeWaitForObservation(ctx context.Context, observe func() error) error {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		err := observe()
		switch {
		case err == nil:
			return nil
		case errors.Is(err, kafka.LeaderNotAvailable), errors.Is(err, kafka.NotLeaderForPartition),
			errors.Is(err, kafka.BrokerNotAvailable), errors.Is(err, kafka.RequestTimedOut),
			errors.Is(err, kafka.OffsetNotAvailable), errors.Is(err, provision.ErrMissing):
		default:
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("quorum observation did not converge: %w (last result: %w)", ctx.Err(), err)
		case <-tick.C:
		}
	}
}

type nativeMetadataReader interface {
	Metadata(context.Context, *kafka.MetadataRequest) (*kafka.MetadataResponse, error)
}

// Redpanda advertises alive brokers, excluding itself when isolated. Observe
// the expected fault view before writes; all three brokers remain mandatory
// for healthy/restored cases. No unknown broker or changed estate is accepted.
func nativeQuorumMetadata(ctx context.Context, c nativeMetadataReader, mode, cluster string) (*kafka.MetadataResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		m, err := c.Metadata(ctx, &kafka.MetadataRequest{Topics: []string{}})
		if err != nil {
			return nil, err
		}
		if m == nil {
			return nil, provision.ErrBroker
		}
		if !provision.ValidClusterID(m.ClusterID) || m.ClusterID != cluster {
			return nil, provision.ErrConflict
		}
		ready, err := nativeBrokerView(m.Brokers, mode)
		if err != nil {
			return nil, err
		}
		if ready {
			return m, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
}

func nativeBrokerView(brokers []kafka.Broker, mode string) (bool, error) {
	seen := map[int]bool{}
	for _, b := range brokers {
		if b.ID < 0 || b.ID > 2 || seen[b.ID] || b.Host != fmt.Sprintf("native-redpanda-%d.kanz-messaging.svc", b.ID) || b.Port != 9094 {
			return false, provision.ErrConflict
		}
		seen[b.ID] = true
	}
	switch mode {
	case "quorum-healthy", "quorum-restored":
		return len(seen) == 3, nil
	case "quorum-majority":
		return len(seen) == 2 && seen[0] && seen[1], nil
	case "quorum-minority":
		return len(seen) == 2 && seen[1] && seen[2], nil
	default:
		return false, provision.ErrInput
	}
}

func nativeProduce(ctx context.Context, c *kafka.Client, topic, value string) error {
	r, e := c.Produce(ctx, &kafka.ProduceRequest{Topic: topic, Partition: 0, RequiredAcks: kafka.RequireAll, Records: kafka.NewRecordReader(kafka.Record{Value: kafka.NewBytes([]byte(value))})})
	if e != nil {
		return e
	}
	if r == nil {
		return provision.ErrBroker
	}
	return r.Error
}
func nativeReadContains(ctx context.Context, c *kafka.Client, topic, value string) error {
	r, e := c.Fetch(ctx, &kafka.FetchRequest{Topic: topic, Partition: 0, Offset: 0, MinBytes: 1, MaxBytes: 8192, MaxWait: time.Second})
	if e != nil {
		return e
	}
	if r == nil {
		return provision.ErrBroker
	}
	if r.Error != nil {
		return r.Error
	}
	if closer, ok := r.Records.(io.Closer); ok {
		defer closer.Close()
	}
	found := false
	for count := 0; ; count++ {
		record, e := r.Records.ReadRecord()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil || count >= 16 {
			return provision.ErrBroker
		}
		b, e := kafka.ReadAll(record.Value)
		if e != nil || len(b) > 256 {
			return provision.ErrBroker
		}
		if string(b) == value {
			found = true
		}
	}
	if !found {
		return provision.ErrMissing
	}
	return nil
}
func nativeDeniedTopic(ctx context.Context, config *tls.Config, topic string) error {
	for _, write := range []bool{true, false} {
		raw, e := (&tls.Dialer{Config: config, NetDialer: &net.Dialer{Timeout: 10 * time.Second}}).DialContext(ctx, "tcp", nativeAddress)
		if e != nil {
			return e
		}
		conn := kafka.NewConn(raw, topic, 0)
		deadline := time.Now().Add(10 * time.Second)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		e = conn.SetDeadline(deadline)
		if e == nil {
			if write {
				_, e = conn.WriteMessages(kafka.Message{Value: []byte("MUST-NOT-WRITE")})
			} else {
				_, e = conn.Seek(0, kafka.SeekAbsolute|kafka.SeekDontCheck)
				if e == nil {
					_, e = conn.ReadMessage(8192)
				}
			}
		}
		conn.Close()
		if !errors.Is(e, kafka.TopicAuthorizationFailed) {
			return provision.ErrConflict
		}
	}
	return nil
}
func nativeGroup(ctx context.Context, c *kafka.Client, group, topic string, deny bool) error {
	if !deny {
		if err := nativeGroupReady(ctx, c, group, topic); err != nil {
			return err
		}
	}
	r, e := c.OffsetCommit(ctx, &kafka.OffsetCommitRequest{GroupID: group, GenerationID: -1, Topics: map[string][]kafka.OffsetCommit{topic: {{Partition: 0, Offset: 1}}}})
	if e == nil {
		if r == nil || len(r.Topics) != 1 || len(r.Topics[topic]) != 1 {
			return provision.ErrBroker
		}
		e = r.Topics[topic][0].Error
	}
	if deny {
		if !errors.Is(e, kafka.GroupAuthorizationFailed) {
			return provision.ErrConflict
		}
		return nil
	}
	return e
}

type nativeGroupReader interface {
	FindCoordinator(context.Context, *kafka.FindCoordinatorRequest) (*kafka.FindCoordinatorResponse, error)
	OffsetFetch(context.Context, *kafka.OffsetFetchRequest) (*kafka.OffsetFetchResponse, error)
}

// kafka-go's automatic group routing discards FindCoordinator's broker error.
// Wait using read-only discovery/fetch before the single positive offset commit;
// never interpret NotCoordinator as authorization evidence or retry a write.
func nativeGroupReady(ctx context.Context, c nativeGroupReader, group, topic string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		coordinator, err := c.FindCoordinator(ctx, &kafka.FindCoordinatorRequest{Key: group, KeyType: kafka.CoordinatorKeyTypeConsumer})
		if err == nil {
			if coordinator == nil {
				return provision.ErrBroker
			}
			err = coordinator.Error
			if err == nil {
				b := coordinator.Coordinator
				if b == nil || b.NodeID < 0 || b.NodeID > 2 || b.Host != fmt.Sprintf("native-redpanda-%d.kanz-messaging.svc", b.NodeID) || b.Port != 9094 {
					return provision.ErrConflict
				}
				var offsets *kafka.OffsetFetchResponse
				offsets, err = c.OffsetFetch(ctx, &kafka.OffsetFetchRequest{GroupID: group, Topics: map[string][]int{topic: {0}}})
				if err == nil {
					if offsets == nil {
						return provision.ErrBroker
					}
					err = offsets.Error
					if err == nil {
						if len(offsets.Topics) != 1 || len(offsets.Topics[topic]) != 1 || offsets.Topics[topic][0].Partition != 0 {
							return provision.ErrBroker
						}
						err = offsets.Topics[topic][0].Error
					}
				}
			}
		}
		if err == nil {
			return nil
		}
		if !errors.Is(err, kafka.GroupCoordinatorNotAvailable) && !errors.Is(err, kafka.GroupLoadInProgress) && !errors.Is(err, kafka.NotCoordinatorForGroup) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

type nativeLostAck struct {
	*kafka.Client
	lost bool
}

func (a *nativeLostAck) CreateTopics(ctx context.Context, r *kafka.CreateTopicsRequest) (*kafka.CreateTopicsResponse, error) {
	result, e := a.Client.CreateTopics(ctx, r)
	if e == nil && !a.lost && result != nil && len(result.Errors) == 1 && result.Errors[r.Topics[0].Topic] == nil {
		a.lost = true
		return nil, provision.ErrBroker
	}
	return result, e
}
func nativePartial(ctx context.Context, c *kafka.Client, cluster string) error {
	raw, e := provision.ReadManifest("/manifest/topics-job.yaml")
	if e != nil {
		return e
	}
	digest := sha256.Sum256(raw)
	p, e := provision.NewPlan(raw, hex.EncodeToString(digest[:]), "partial", "User:spiffe://kanz.internal/ns/tenant-partial/sa/risk-engine", cluster)
	if e != nil {
		return e
	}
	p, e = provision.WithPrincipalEncoding(p, "redpanda-rule-escaped-uri", nativePeer)
	if e != nil {
		return e
	}
	p, e = provision.WithDurabilityProfile(p, "redpanda-raft", nativePeer)
	if e != nil {
		return e
	}
	wrapper := &nativeLostAck{Client: c}
	receipt, e := provision.Run(ctx, wrapper, p, "apply")
	if !errors.Is(e, provision.ErrBroker) || !wrapper.lost || !receipt.PartialOrUnknown || receipt.TopicAttempts != 1 || receipt.ACLAttempts != 0 || receipt.Complete {
		return provision.ErrConflict
	}
	// Wait for actual newly-created topic readiness before storing the sentinel.
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		m, e := c.Metadata(ctx, &kafka.MetadataRequest{Topics: []string{p.Topics[0].Name}})
		if e == nil && m != nil && m.ClusterID != cluster {
			return provision.ErrConflict
		}
		if e == nil && m != nil && m.ClusterID == cluster && len(m.Topics) == 1 && m.Topics[0].Error == nil && len(m.Topics[0].Partitions) == p.Topics[0].Partitions {
			ready := true
			for _, partition := range m.Topics[0].Partitions {
				if partition.Error != nil || partition.Leader.Host == "" || len(partition.Isr) < 2 {
					ready = false
				}
			}
			if ready {
				break
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	if e = nativeProduce(ctx, c, p.Topics[0].Name, "preserved-partial-history"); e != nil {
		return e
	}
	receipt, e = provision.Run(ctx, c, p, "apply")
	if e != nil || !receipt.Complete || receipt.TopicAttempts != len(p.Topics)-1 {
		return provision.ErrConflict
	}
	if e = nativeReadContains(ctx, c, p.Topics[0].Name, "preserved-partial-history"); e != nil {
		return e
	}
	receipt, e = provision.Run(ctx, c, p, "verify")
	if e != nil || !receipt.Complete || receipt.TopicAttempts != 0 || receipt.ACLAttempts != 0 {
		return provision.ErrConflict
	}
	if !strings.HasPrefix(p.Topics[0].Name, "partial.") {
		return provision.ErrInput
	}
	return nil
}
