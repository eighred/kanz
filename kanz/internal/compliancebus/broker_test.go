package compliancebus_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/bustest"
	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/compliancebus"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"
	"github.com/eighred/kanz/pkg/bus"
)

// #713'S "VERIFIED WHEN", AGAINST A REAL BROKER.
//
// Every other test of the asynchronous recorder drives a FAKE bus, and AGENTS.md
// names that limit precisely: "fakeBus does not validate envelopes, so it accepts
// what a real broker rejects. A green suite using it is not a broker proof."
//
// That limit lands on the exact field this recorder exists to carry. The OMS
// publishes its decisions from a BACKGROUND WORKER, long after the inbound
// delivery whose context carries the tenant is gone, and the OMS producer has no
// tenant fallback — so an empty tenant_id is refused by bus.Validate on the live
// path and accepted silently by every fake. That refusal crash-looped the OMS
// once already. A fake bus cannot tell the two apart; this can.
//
// Gated on TEST_NATS_URL. A JetStream broker needs no Docker:
//
//	go install github.com/nats-io/nats-server/v2@latest
//	nats-server -js -sd <empty-dir>
//	TEST_NATS_URL=nats://localhost:4222 go test ./internal/compliancebus/...
//
// USE A PRISTINE STORE DIRECTORY. The subject rides a retained stream, so a
// broker reused across runs hands this test the previous run's decisions — which
// is how a counting assertion in this repository once passed on its first run and
// failed on every one after.

func brokerLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// liveRecorder wires the real producer and the real recorder against url.
func liveRecorder(t *testing.T, ctx context.Context, url, tenant string) (*compliancebus.AsyncRecorder, *bus.NATSClient) {
	t.Helper()
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "compliancebus-it"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// THE OMS'S OWN PRODUCER SHAPE. ProducerConfig.Tenant is deliberately EMPTY:
	// the OMS builds its producer without one (its events carry the tenant of
	// whichever delivery they answer), and that is the whole reason the recorder
	// has to stamp the tenant itself. Setting it here would supply the field this
	// test exists to prove the RECORD carries.
	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source: "oms", ProducerVersion: "it",
	})
	if err != nil {
		t.Fatalf("producer: %v", err)
	}
	var publishErr error
	rec, err := compliancebus.NewAsyncRecorder(producer, brokerLogger(),
		compliancebus.WithErrorHandler(func(err error) { publishErr = err }))
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	t.Cleanup(func() {
		rec.Close()
		if publishErr != nil {
			t.Errorf("the broker refused a decision this recorder published: %v", publishErr)
		}
	})
	return rec, client
}

func decisionFor(tenant, portfolio, orderID string, allowed bool) comp.DecisionRecord {
	return comp.DecisionRecord{
		Phase:    comp.PhasePreTrade,
		TenantID: tenant,
		Allowed:  allowed,
		OrderID:  orderID,
		Issuer:   "operator:akif",
		Result: &compliancepb.ComplianceResult{
			Status:      compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS,
			PortfolioId: portfolio,
			MandateId:   "m-" + portfolio,
			EvaluatedAt: timestamppb.New(time.Now().UTC()),
		},
	}
}

// A PRE-TRADE DECISION PUBLISHED BY THE RECORDER IS ACCEPTED BY A REAL BROKER AND
// COMES BACK OFF THE SUBJECT AUDIT-01 SELECTS.
//
// This is #713's first clause. What it proves that the fake cannot: the envelope
// SURVIVES bus.Validate. Every field the validator requires — a non-empty tenant,
// an event time, a domain, a schema ref — is present on a record whose caller's
// context no longer exists.
func TestAPreTradeDecisionReachesTheAuditSubjectOverARealSpine(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to drive the compliance decision path over a real spine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	admin, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	// The production topology binds this subject on the PLATFORM stream; on a bare
	// broker a scratch one stands in, which still proves the publish is accepted
	// and the payload round-trips.
	bustest.EnsureSubjects(t, ctx, js, "COMPLIANCE_DECISION_IT_"+suffix,
		[]string{compliancebus.SubjectDecision})

	rec, _ := liveRecorder(t, ctx, url, "acme")
	orderID := "ORD-" + suffix
	if err := rec.Record(ctx, decisionFor("acme", "fund-alpha-"+suffix, orderID, true)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	rec.Close() // drains, so the publish has been attempted and acked by the broker

	// READ BACK FROM THE STREAM, NOT FROM A CORE SUBSCRIPTION.
	//
	// A core subscriber sees a message IN FLIGHT and would be satisfied by a
	// broker that stored nothing — which is the opposite of what "on the audit
	// chain" means. AUDIT-01's projection consumes the STREAM, so the stream is
	// what has to hold the record. (The publish is JetStream-acked, so it would
	// also have failed outright with no stream bound; this makes the persistence
	// evidence direct rather than inferred from an absence of errors.)
	env, payload := fetchDecision(t, ctx, js, orderID)
	// THE TENANT IS THE POINT. bus.Validate refuses an empty one, so a record that
	// arrives here at all proves the recorder stamped it from the RECORD rather
	// than hoping for a context that no longer exists.
	if env.GetTenantId() != "acme" {
		t.Errorf("tenant_id = %q, want acme", env.GetTenantId())
	}
	if env.GetEventType() != compliancebus.DecisionEventType {
		t.Errorf("event_type = %q, want %q", env.GetEventType(), compliancebus.DecisionEventType)
	}
	if env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_OBSERVATION {
		t.Errorf("event_class = %v, want OBSERVATION — AUDIT-01 selects compliance decisions off "+
			"the observation stream", env.GetEventClass())
	}

	var log observationpb.DecisionLog
	if err := proto.Unmarshal(payload, &log); err != nil {
		t.Fatalf("the framed payload is not a DecisionLog: %v", err)
	}
	if got := log.GetAttributes()["order_id"]; got != orderID {
		t.Errorf("order_id = %q, want %q — the record cannot be tied back to the order it gated", got, orderID)
	}
	if got := log.GetAttributes()["phase"]; got != comp.PhasePreTrade {
		t.Errorf("phase = %q, want %q — the pre-trade and post-trade halves of this trail are told "+
			"apart by this attribute alone, since they share a subject", got, comp.PhasePreTrade)
	}
	if got := log.GetAttributes()["allowed"]; got != "true" {
		t.Errorf("allowed = %q, want true — an ADMISSION must be recorded, not only a refusal: "+
			"'why was this trade allowed' is the question a regulator asks", got)
	}
	if log.GetDecider() != compliancebus.Decider {
		t.Errorf("decider = %q, want %q", log.GetDecider(), compliancebus.Decider)
	}
}

// BOTH VERDICTS RIDE THE SAME SUBJECT AND ARE TOLD APART BY THE RECORD.
//
// A trail that carried only refusals would answer "why was this order stopped"
// and not "why was this one allowed", and the second is the question asked about
// the trade that lost the money.
func TestBothVerdictsReachTheSubjectOverARealSpine(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to drive the compliance decision path over a real spine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	admin, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	bustest.EnsureSubjects(t, ctx, js, "COMPLIANCE_DECISION_IT_"+suffix,
		[]string{compliancebus.SubjectDecision})

	rec, _ := liveRecorder(t, ctx, url, "acme")
	ids := map[bool]string{}
	for _, allowed := range []bool{true, false} {
		id := fmt.Sprintf("ORD-%s-%v", suffix, allowed)
		ids[allowed] = id
		if err := rec.Record(ctx, decisionFor("acme", "fund-beta-"+suffix, id, allowed)); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	rec.Close()

	seen := map[string]string{}
	for _, allowed := range []bool{true, false} {
		_, payload := fetchDecision(t, ctx, js, ids[allowed])
		var log observationpb.DecisionLog
		if err := proto.Unmarshal(payload, &log); err != nil {
			t.Fatal(err)
		}
		seen[log.GetAttributes()["allowed"]] = log.GetAttributes()["order_id"]
	}
	for _, want := range []string{"true", "false"} {
		if seen[want] == "" {
			t.Errorf("no decision with allowed=%s reached the subject; saw %v", want, seen)
		}
	}
}

// THE RECORD'S TENANT IS LOAD-BEARING, PROVEN BY ITS ABSENCE.
//
// This is the crash-loop, reproduced: a decision with no tenant is REFUSED by the
// broker. It matters that this is asserted against a real spine — the fake bus
// every other test uses accepts it silently, so without this the tenant field
// would look decorative and the next edit could drop it.
//
// The recorder's contract holds either way: Record still returns nil (an audit
// sink must not fail a trading decision), the failure surfaces through the error
// handler, and the OMS counts it.
func TestADecisionWithNoTenantIsRefusedByTheBroker(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to drive the compliance decision path over a real spine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	admin, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	bustest.EnsureSubjects(t, ctx, js, "COMPLIANCE_DECISION_IT_"+suffix,
		[]string{compliancebus.SubjectDecision})

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "compliancebus-it-notenant"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "oms", ProducerVersion: "it"})
	if err != nil {
		t.Fatal(err)
	}

	refused := make(chan error, 1)
	rec, err := compliancebus.NewAsyncRecorder(producer, brokerLogger(),
		compliancebus.WithErrorHandler(func(err error) {
			select {
			case refused <- err:
			default:
			}
		}))
	if err != nil {
		t.Fatal(err)
	}

	// TenantID deliberately empty — the state a background worker lands in when
	// the record does not carry it.
	rejected := decisionFor("", "fund-gamma-"+suffix, "ORD-notenant-"+suffix, true)
	if err := rec.Record(ctx, rejected); err != nil {
		t.Fatalf("Record returned an error; it must stay best-effort whatever the broker says: %v", err)
	}
	rec.Close() // drains, so the publish has been attempted by the time this returns

	select {
	case err := <-refused:
		if err == nil {
			t.Fatal("the error handler fired with a nil error")
		}
	default:
		t.Fatal("a decision with NO TENANT was accepted by the broker. Either bus.Validate no " +
			"longer refuses an empty tenant_id — in which case one tenant's compliance decision " +
			"can be filed under another's — or this test is no longer reaching the live path")
	}
}

// fetchDecision reads the STREAM carrying the decision subject and returns the
// record whose order_id matches, or fails.
//
// IT SEARCHES BY ORDER ID rather than taking the first message. The subject is
// retained, so on any broker that has been used before, "the first record on the
// stream" is somebody else's — which is how a counting assertion in this
// repository passed once and failed for ever after.
func fetchDecision(t *testing.T, ctx context.Context, js jetstream.JetStream, orderID string) (*envelopepb.Envelope, []byte) {
	t.Helper()
	name, err := js.StreamNameBySubject(ctx, compliancebus.SubjectDecision)
	if err != nil {
		t.Fatalf("no stream carries %s: %v — the publish is JetStream-acked, so this subject being "+
			"unbound means nothing was persisted at all", compliancebus.SubjectDecision, err)
	}
	stream, err := js.Stream(ctx, name)
	if err != nil {
		t.Fatalf("stream %s: %v", name, err)
	}
	cons, err := stream.CreateConsumer(ctx, jetstream.ConsumerConfig{
		FilterSubject: compliancebus.SubjectDecision,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		AckPolicy:     jetstream.AckNonePolicy,
	})
	if err != nil {
		t.Fatalf("consumer on %s: %v", name, err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		batch, err := cons.Fetch(50, jetstream.FetchMaxWait(2*time.Second))
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		for msg := range batch.Messages() {
			env, payload, err := bus.Unframe(msg.Data())
			if err != nil {
				t.Fatalf("what is on the stream is not a bus frame: %v", err)
			}
			var log observationpb.DecisionLog
			if err := proto.Unmarshal(payload, &log); err != nil {
				t.Fatalf("the framed payload is not a DecisionLog: %v", err)
			}
			if log.GetAttributes()["order_id"] == orderID {
				return env, payload
			}
		}
		if err := batch.Error(); err != nil {
			t.Fatalf("fetch batch: %v", err)
		}
	}
	t.Fatalf("no decision for order %s was PERSISTED on %s within 20s. The recorder is "+
		"asynchronous, so a publish the broker refused is silent on the caller's side — which is "+
		"exactly the failure this test exists to catch", orderID, name)
	return nil, nil
}
