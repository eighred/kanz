package archive_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/archiver/internal/archive"
)

type fakeKafka struct {
	got  []bus.Message
	fail error
}

func (f *fakeKafka) Publish(_ context.Context, m bus.Message) error {
	if f.fail != nil {
		return f.fail
	}
	f.got = append(f.got, m)
	return nil
}

// body frames e the same way bus.Producer does on publish — an EventFrame
// wrapping the envelope — since that is what bus.Unframe expects to decode.
func body(t *testing.T, e *envelopepb.Envelope) []byte {
	t.Helper()
	b, err := proto.Marshal(&envelopepb.EventFrame{Envelope: e})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func newArchiver(k archive.Publisher) *archive.Archiver {
	return archive.New(archive.Config{
		Tenant: "acme",
		Group:  "archiver",
		Kafka:  k,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestHandle_ProducesVerbatimToTheMappedTopic(t *testing.T) {
	e := &envelopepb.Envelope{
		EventType:    "order.order.submitted",
		TenantId:     "acme",
		EventId:      "evt-1",
		PartitionKey: "portfolio-7",
		EventClass:   envelopepb.EventClass_EVENT_CLASS_FACT,
	}
	raw := body(t, e)
	k := &fakeKafka{}

	if err := newArchiver(k).Handle(context.Background(), bus.Message{Subject: "order.order.submitted", Body: raw}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(k.got) != 1 {
		t.Fatalf("published %d messages, want 1", len(k.got))
	}
	m := k.got[0]
	if m.Subject != "acme.order.order" {
		t.Errorf("topic = %q, want %q", m.Subject, "acme.order.order")
	}
	// The KEY is what preserves per-entity order across partitions.
	if string(m.Key) != "portfolio-7" {
		t.Errorf("key = %q, want %q", m.Key, "portfolio-7")
	}
	// VERBATIM: the archiver is a transport, not a producer.
	if string(m.Body) != string(raw) {
		t.Error("body was rewritten — the archiver must never re-stamp the log of record")
	}
	if m.Headers["Kanz-Event-Id"] != "evt-1" {
		t.Errorf("Kanz-Event-Id = %q, want evt-1 (the downstream dedup key)", m.Headers["Kanz-Event-Id"])
	}
}

// THE CENTRAL GUARANTEE. A failed produce must NACK, so NATS redelivers. Acking a
// message we failed to archive is silent, permanent data loss — the exact bug this
// service exists to end.
func TestHandle_KafkaFailureNacks(t *testing.T) {
	e := &envelopepb.Envelope{EventType: "order.order.submitted", TenantId: "acme", EventId: "evt-1"}
	k := &fakeKafka{fail: errors.New("kafka down")}

	err := newArchiver(k).Handle(context.Background(), bus.Message{Body: body(t, e)})
	if err == nil {
		t.Fatal("Handle returned nil on a failed produce — NATS would ACK and the event would be LOST FOREVER")
	}
}

// An unmappable event can NEVER succeed on redelivery — it is TERMINAL, not
// transient. It is dead-lettered to dlq.archiver verbatim, with cause
// headers, and Handle returns nil so NATS acks it: NACK-looping a message
// that will never map is exactly the failure mode this split exists to end.
func TestHandle_UnroutableEventIsDeadLetteredAndAcked(t *testing.T) {
	e := &envelopepb.Envelope{EventType: "order.order.submitted", TenantId: "someone-else", EventId: "evt-1"}
	raw := body(t, e)
	k := &fakeKafka{}

	err := newArchiver(k).Handle(context.Background(), bus.Message{Subject: "order.order.submitted", Body: raw})
	if err != nil {
		t.Fatalf("Handle returned %v for a terminal (unroutable) event — want nil (acked)", err)
	}
	if len(k.got) != 1 {
		t.Fatalf("published %d messages, want 1 (the DLQ produce)", len(k.got))
	}
	m := k.got[0]
	if m.Subject != "dlq.archiver" {
		t.Errorf("DLQ subject = %q, want dlq.archiver", m.Subject)
	}
	if string(m.Body) != string(raw) {
		t.Error("DLQ body was not the raw envelope verbatim")
	}
	assertParkContract(t, m, "route", "order.order.submitted")
	if m.Headers["Kanz-Event-Id"] != "evt-1" {
		t.Errorf("Kanz-Event-Id = %q, want evt-1 (readable even though routing failed)", m.Headers["Kanz-Event-Id"])
	}
	// The archiver's own near-miss spelling of the originating subject is gone
	// (#285). It is asserted by NAME rather than by the constant, because the
	// whole defect was that the constant and the wire disagreed.
	if _, ok := m.Headers["Kanz-DLQ-Subject"]; ok {
		t.Error("Kanz-DLQ-Subject is back. It is a second name for Kanz-DLQ-Original-Subject, " +
			"and a drain keyed on the canonical one finds nothing and reports success")
	}
	if _, ok := m.Headers["Kanz-DLQ-Reason"]; ok {
		t.Error("Kanz-DLQ-Reason is back. Both of its values are ClassTerminal, so it duplicates " +
			"Kanz-DLQ-Class; the reason rides in Kanz-DLQ-Error and in the metric's reason label")
	}
}

// assertParkContract checks a parked message against pkg/bus's DLQ wire contract
// (#285): the archiver must carry what a drain reads, under the shared names.
//
// The header names are spelled out as LITERALS here on purpose. Asserting through
// bus.HeaderDLQClass would pass even if that constant's value changed, which is
// exactly the silent break — the constant and the wire have to be pinned to each
// other somewhere, and a test is the only place that can be done.
func assertParkContract(t *testing.T, m bus.Message, wantReason, wantOrigin string) {
	t.Helper()

	if got := m.Headers["Kanz-DLQ-Original-Subject"]; got != wantOrigin {
		t.Errorf("Kanz-DLQ-Original-Subject = %q, want %q — without it a drain has no address to "+
			"send the event back to and refuses", got, wantOrigin)
	}
	// The reason it failed is the prefix of the error, not a header of its own,
	// and it is the same token as the kanz_archiver_failed_total reason label.
	if got := m.Headers["Kanz-DLQ-Error"]; !strings.HasPrefix(got, wantReason+": ") {
		t.Errorf("Kanz-DLQ-Error = %q, want it to begin %q — that prefix is what Kanz-DLQ-Reason "+
			"used to carry, and the operator reading the topic needs it", got, wantReason+": ")
	}
	// BOTH park reasons are terminal: unframe and route are pure functions of the
	// bytes and this archiver's static config, so nothing in the world changes to
	// make either succeed. Transient here would invite a drain to replay it,
	// fail identically and re-park — the loop the class exists to prevent.
	if got := m.Headers["Kanz-DLQ-Class"]; got != "terminal" {
		t.Errorf("Kanz-DLQ-Class = %q, want terminal", got)
	}
	parkedAt := m.Headers["Kanz-DLQ-Parked-At"]
	if parkedAt == "" {
		t.Fatal("Kanz-DLQ-Parked-At is absent — a drain's min-age gate cannot establish the " +
			"message's age and refuses it")
	}
	// RFC3339Nano specifically: pkg/bus.parkedAtOf parses it with that layout and
	// refuses anything else, so a differently-formatted timestamp is the same as
	// no timestamp.
	if _, err := time.Parse(time.RFC3339Nano, parkedAt); err != nil {
		t.Errorf("Kanz-DLQ-Parked-At = %q does not parse as RFC3339Nano (%v) — pkg/bus.parkedAtOf "+
			"refuses it, so the age gate is unusable", parkedAt, err)
	}
}

// An undecodable body is equally terminal: it will never decode differently
// on redelivery. Same dead-letter-then-ack treatment, but no event_id header
// — the body couldn't even be unframed to read one.
func TestHandle_UndecodableBodyIsDeadLetteredAndAcked(t *testing.T) {
	raw := []byte("not a protobuf envelope")
	k := &fakeKafka{}

	err := newArchiver(k).Handle(context.Background(), bus.Message{Subject: "order.order.submitted", Body: raw})
	if err != nil {
		t.Fatalf("Handle returned %v for an undecodable body — want nil (acked)", err)
	}
	if len(k.got) != 1 {
		t.Fatalf("published %d messages, want 1 (the DLQ produce)", len(k.got))
	}
	m := k.got[0]
	if m.Subject != "dlq.archiver" {
		t.Errorf("DLQ subject = %q, want dlq.archiver", m.Subject)
	}
	if string(m.Body) != string(raw) {
		t.Error("DLQ body was not the raw body verbatim")
	}
	assertParkContract(t, m, "unframe", "order.order.submitted")
	if _, ok := m.Headers["Kanz-Event-Id"]; ok {
		t.Error("Kanz-Event-Id was set for an undecodable body — no event_id could ever be read")
	}
}

// If the DLQ produce ITSELF fails, that is just another Kafka outage —
// transient, not terminal. Handle must NACK: an event that reached neither
// its real topic nor the DLQ must never be acked, or it is lost forever.
func TestHandle_DeadLetterProduceFailureNacks(t *testing.T) {
	e := &envelopepb.Envelope{EventType: "order.order.submitted", TenantId: "someone-else", EventId: "evt-1"}
	k := &fakeKafka{fail: errors.New("kafka down")}

	err := newArchiver(k).Handle(context.Background(), bus.Message{Body: body(t, e)})
	if err == nil {
		t.Fatal("Handle returned nil when the DLQ produce itself failed — event reached neither the real topic nor the DLQ and would be LOST")
	}
}

// blockingSubscriber mimics bus.NATSClient.Subscribe: it does not return until ctx
// is done. Each call records its subject the instant it is invoked, BEFORE
// blocking — which is what lets the test observe "was this subject ever
// subscribed" without waiting for the (never-happening, pre-fix) return.
type blockingSubscriber struct {
	mu       sync.Mutex
	subjects []string
	calledCh chan string
}

func (b *blockingSubscriber) Subscribe(ctx context.Context, subject, _ string, _ bus.Handler) error {
	b.mu.Lock()
	b.subjects = append(b.subjects, subject)
	b.mu.Unlock()
	b.calledCh <- subject
	<-ctx.Done()
	return nil
}

// TestRun_SubscribesEveryConfiguredSubject is the regression test for the
// CRITICAL finding: Run() looped over subjects and called the (blocking)
// Subscribe SEQUENTIALLY, so it never got past subject #1 — subjects 2..N were
// never subscribed for the life of the process while the service reported
// healthy. bus.NATSClient.Subscribe genuinely blocks until ctx.Done() (see
// pkg/bus/nats.go), so a non-blocking fake would not catch this; this fake
// blocks the same way.
func TestRun_SubscribesEveryConfiguredSubject(t *testing.T) {
	subjects := []string{"order.>", "strategy.>", "execution.>", "accounting.>"}
	sub := &blockingSubscriber{calledCh: make(chan string, len(subjects))}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := archive.New(archive.Config{
		Tenant:   "acme",
		Group:    "archiver",
		Subjects: subjects,
		NATS:     sub,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	runDone := make(chan error, 1)
	go func() { runDone <- a.Run(ctx) }()

	got := map[string]bool{}
	deadline := time.After(3 * time.Second)
	for len(got) < len(subjects) {
		select {
		case s := <-sub.calledCh:
			got[s] = true
		case <-deadline:
			t.Fatalf("timed out waiting for all subjects to be subscribed — only %v were ever subscribed, want all of %v (Run is blocking sequentially on the first subject)", got, subjects)
		}
	}

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run returned error after ctx cancel: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx was cancelled")
	}

	for _, s := range subjects {
		if !got[s] {
			t.Errorf("subject %q was never subscribed", s)
		}
	}
}

// Lag is the metric that matters. Success and failure counts are how an operator
// sees the archiver refusing events (a NACK loop is invisible otherwise: it logs,
// redelivers, logs, redelivers, and nothing else changes).
func TestHandle_CountsOutcomes(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := archive.NewMetrics(reg)

	e := &envelopepb.Envelope{EventType: "order.order.submitted", TenantId: "acme", EventId: "evt-1"}
	ok := &fakeKafka{}
	a := archive.New(archive.Config{
		Tenant: "acme", Group: "archiver", Kafka: ok, Metrics: m,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if err := a.Handle(context.Background(), bus.Message{Body: body(t, e)}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := testutil.ToFloat64(m.Archived.WithLabelValues("acme.order.order")); got != 1 {
		t.Errorf("archived_total = %v, want 1", got)
	}

	bad := &fakeKafka{fail: errors.New("kafka down")}
	a2 := archive.New(archive.Config{
		Tenant: "acme", Group: "archiver", Kafka: bad, Metrics: m,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err := a2.Handle(context.Background(), bus.Message{Body: body(t, e)}); err == nil {
		t.Fatal("expected a produce failure")
	}
	if got := testutil.ToFloat64(m.Failed.WithLabelValues("acme.order.order", "publish")); got != 1 {
		t.Errorf("failed_total{reason=publish} = %v, want 1", got)
	}
}

// The two terminal (dead-letter-then-ack) paths must each be counted under their
// own cause — an operator watching failed_total must be able to tell "we are
// receiving cross-tenant junk" (route) apart from "our own producer is emitting
// garbage" (unframe) without reading logs.
func TestHandle_CountsDeadLetterCauses(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := archive.NewMetrics(reg)
	k := &fakeKafka{}
	a := archive.New(archive.Config{
		Tenant: "acme", Group: "archiver", Kafka: k, Metrics: m,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	// route: cross-tenant event can never map to a topic for this archiver.
	routeEvt := &envelopepb.Envelope{EventType: "order.order.submitted", TenantId: "someone-else", EventId: "evt-1"}
	if err := a.Handle(context.Background(), bus.Message{Subject: "order.order.submitted", Body: body(t, routeEvt)}); err != nil {
		t.Fatalf("Handle (route): %v", err)
	}
	if got := testutil.ToFloat64(m.Failed.WithLabelValues("", "route")); got != 1 {
		t.Errorf("failed_total{reason=route} = %v, want 1", got)
	}

	// unframe: undecodable body.
	if err := a.Handle(context.Background(), bus.Message{Subject: "order.order.submitted", Body: []byte("not a protobuf envelope")}); err != nil {
		t.Fatalf("Handle (unframe): %v", err)
	}
	if got := testutil.ToFloat64(m.Failed.WithLabelValues("", "unframe")); got != 1 {
		t.Errorf("failed_total{reason=unframe} = %v, want 1", got)
	}
}

// If the DLQ produce itself fails, the event reached NEITHER its real topic NOR
// the DLQ — the most severe outcome the archiver can have, and it must be
// distinguishable in failed_total from an ordinary successful dead-letter.
func TestHandle_CountsDLQPublishFailure(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := archive.NewMetrics(reg)
	bad := &fakeKafka{fail: errors.New("kafka down")}
	a := archive.New(archive.Config{
		Tenant: "acme", Group: "archiver", Kafka: bad, Metrics: m,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	e := &envelopepb.Envelope{EventType: "order.order.submitted", TenantId: "someone-else", EventId: "evt-1"}
	if err := a.Handle(context.Background(), bus.Message{Body: body(t, e)}); err == nil {
		t.Fatal("expected the DLQ produce failure to NACK")
	}
	if got := testutil.ToFloat64(m.Failed.WithLabelValues(archive.DLQSubject, "dlq_publish")); got != 1 {
		t.Errorf("failed_total{topic=%q, reason=dlq_publish} = %v, want 1", archive.DLQSubject, got)
	}
	// The routing cause is ALSO counted — dlq_publish does not replace it, since
	// the underlying cause (route, here) is still true operational signal.
	if got := testutil.ToFloat64(m.Failed.WithLabelValues("", "route")); got != 1 {
		t.Errorf("failed_total{reason=route} = %v, want 1", got)
	}
}
