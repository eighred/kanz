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
		"discover":  "spiffe://kanz.internal/ns/kanz-messaging/sa/kafka-provisioner",
		"partial":   "spiffe://kanz.internal/ns/kanz-messaging/sa/kafka-provisioner",
		"tenant-a":  "spiffe://kanz.internal/ns/tenant-acme/sa/risk-engine",
		"tenant-b":  "spiffe://kanz.internal/ns/tenant-bravo/sa/risk-engine",
		"compute-a": "spiffe://kanz.internal/ns/kanz-services/sa/archiver-acme",
		"ungranted": "spiffe://kanz.internal/ns/tenant-acme/sa/ungranted",
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
	meta, err := client.Metadata(ctx, &kafka.MetadataRequest{Topics: []string{}})
	if err != nil || meta == nil || !provision.ValidClusterID(meta.ClusterID) || len(meta.Brokers) != 3 {
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
	if err = nativeProduce(ctx, client, topic, value); err != nil {
		return out, err
	}
	if err = nativeReadContains(ctx, client, topic, value); err != nil {
		return out, err
	}
	if err = nativeDeniedTopic(ctx, config, foreign+".order.order"); err != nil {
		return out, err
	}
	if err = nativeGroup(ctx, client, tenant+".fixture", topic, false); err != nil {
		return out, err
	}
	if err = nativeGroup(ctx, client, foreign+".fixture", topic, true); err != nil {
		return out, err
	}
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
	if r == nil || r.Error != nil {
		return provision.ErrBroker
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
			break
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
