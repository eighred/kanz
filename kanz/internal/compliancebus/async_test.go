package compliancebus

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	comp "github.com/eighred/kanz/internal/compliance"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"
	"github.com/eighred/kanz/pkg/bus"
)

// THE ASYNCHRONOUS RECORDER (#713).
//
// What is graded here is the difference between this and the synchronous
// recorder beside it: that Record does not wait for the broker, that a decision
// still carries its tenant when the caller's context is long gone, and that a
// drop is counted rather than swallowed. Every one of those fails quietly, and
// the first one fails by making order admission slower rather than by failing at
// all.

// --- fixtures -------------------------------------------------------------

type asyncBus struct {
	mu     sync.Mutex
	events []bus.Event
	err    error
	block  chan struct{} // non-nil ⇒ Publish waits on it
}

func (c *asyncBus) Publish(_ context.Context, e bus.Event) error {
	if c.block != nil {
		<-c.block
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
	return c.err
}

func (c *asyncBus) seen() []bus.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]bus.Event, len(c.events))
	copy(out, c.events)
	return out
}

func asyncQuiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

var asyncEvaluatedAt = time.Date(2026, 8, 24, 14, 30, 0, 0, time.UTC)

func asyncPreTrade(orderID string) comp.DecisionRecord {
	return comp.DecisionRecord{
		Phase:    comp.PhasePreTrade,
		TenantID: "acme",
		Allowed:  true,
		OrderID:  orderID,
		Issuer:   "operator:akif",
		Result: &compliancepb.ComplianceResult{
			Status:      compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS,
			PortfolioId: "fund-alpha",
			MandateId:   "M-1",
			EvaluatedAt: timestamppb.New(asyncEvaluatedAt),
		},
	}
}

func mustAsync(t *testing.T, b Bus, opts ...AsyncOption) *AsyncRecorder {
	t.Helper()
	r, err := NewAsyncRecorder(b, asyncQuiet(), opts...)
	if err != nil {
		t.Fatalf("NewAsyncRecorder: %v", err)
	}
	return r
}

// --- the property the whole thing exists for ------------------------------

// RECORD DOES NOT WAIT FOR THE BROKER.
//
// This is the difference in kind from the synchronous recorder, and it is why the
// OMS may use this one at all: order admission is outbox-backed and nothing on it
// waits for a broker today. A Record that blocked would put broker latency into
// admission — a change to the trading path, made for a reporting reason, and
// invisible in every functional test.
func TestRecordDoesNotWaitForTheBroker(t *testing.T) {
	block := make(chan struct{})
	b := &asyncBus{block: block}
	rec := mustAsync(t, b)
	defer func() { close(block); rec.Close() }()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			_ = rec.Record(context.Background(), asyncPreTrade("ORD-1"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Record blocked while the broker was wedged — every order admission now waits for " +
			"the audit sink, which is exactly what the outbox path was built to avoid")
	}
}

// --- what reaches the wire ------------------------------------------------

// THE DECISION REACHES THE SUBJECT AUDIT-01 SELECTS, with the order it gated and
// the evaluation's own time.
func TestTheDecisionIsPublishedOnTheAuditSubject(t *testing.T) {
	b := &asyncBus{}
	rec := mustAsync(t, b)
	if err := rec.Record(context.Background(), asyncPreTrade("ORD-7")); err != nil {
		t.Fatalf("Record: %v", err)
	}
	rec.Close() // drains

	events := b.seen()
	if len(events) != 1 {
		t.Fatalf("published %d events, want 1", len(events))
	}
	e := events[0]
	if e.Subject != SubjectDecision {
		t.Errorf("subject = %q, want %q — AUDIT-01's projection selects that one", e.Subject, SubjectDecision)
	}
	if !e.EventTime.Equal(asyncEvaluatedAt) {
		t.Errorf("event time = %s, want the evaluation time %s. This recorder is delayed BY DESIGN, "+
			"so stamping publish time would make every decision look later than it was", e.EventTime, asyncEvaluatedAt)
	}
	if e.PartitionKey != "fund-alpha" {
		t.Errorf("partition key = %q, want the portfolio so one book's trail stays ordered", e.PartitionKey)
	}
	log, ok := e.Payload.(*observationpb.DecisionLog)
	if !ok {
		t.Fatalf("payload is %T, want a DecisionLog", e.Payload)
	}
	if got := log.GetAttributes()["order_id"]; got != "ORD-7" {
		t.Errorf("order_id = %q, want ORD-7 — the record cannot be tied back to the order it gated", got)
	}
	if got := log.GetAttributes()["phase"]; got != comp.PhasePreTrade {
		t.Errorf("phase = %q, want %q", got, comp.PhasePreTrade)
	}
}

// THE TENANT COMES OFF THE RECORD, NOT OFF THE CONTEXT.
//
// The defect this exists to prevent: pkg/bus stashes the inbound delivery's
// tenant on ctx, this worker runs long after that context is gone, and the OMS's
// producer carries no tenant fallback — so the broker refuses the event outright.
// That refusal crash-looped the OMS once already.
func TestTheTenantSurvivesTheCallersContext(t *testing.T) {
	b := &asyncBus{}
	rec := mustAsync(t, b)

	ctx, cancel := context.WithCancel(context.Background())
	if err := rec.Record(ctx, asyncPreTrade("ORD-9")); err != nil {
		t.Fatalf("Record: %v", err)
	}
	cancel() // the caller's context is gone before the worker publishes
	rec.Close()

	events := b.seen()
	if len(events) != 1 {
		t.Fatalf("published %d events, want 1", len(events))
	}
	if events[0].TenantID != "acme" {
		t.Errorf("tenant_id = %q, want acme. bus.Validate refuses an empty tenant, so this record "+
			"would never reach the trail — and a DEFAULT here would file acme's decision under "+
			"whatever this deployment was configured with: valid, wrong, undetectable downstream",
			events[0].TenantID)
	}
}

// A CANCELLED CALLER CONTEXT DOES NOT CANCEL THE PUBLISH. The decision was made
// and acted upon; losing its record because the request that triggered it
// finished is the audit trail depending on the caller's lifetime.
func TestACancelledCallerDoesNotCancelThePublish(t *testing.T) {
	b := &asyncBus{}
	rec := mustAsync(t, b)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rec.Record(ctx, asyncPreTrade("ORD-11")); err != nil {
		t.Fatalf("Record: %v", err)
	}
	rec.Close()
	if len(b.seen()) != 1 {
		t.Error("nothing was published for a decision recorded with an already-cancelled context")
	}
}

// --- the failures, counted ------------------------------------------------

// A FULL QUEUE DROPS, COUNTS AND REPORTS. Silent shedding would leave the trail
// missing the busiest minutes of the day, which read as quiet ones.
func TestOverflowIsCountedAndReported(t *testing.T) {
	block := make(chan struct{})
	b := &asyncBus{block: block}
	var mu sync.Mutex
	var dropped int
	rec := mustAsync(t, b,
		WithQueueSize(1),
		WithOverflowHandler(func(comp.DecisionRecord) {
			mu.Lock()
			defer mu.Unlock()
			dropped++
		}))
	for i := 0; i < 20; i++ {
		_ = rec.Record(context.Background(), asyncPreTrade("ORD-flood"))
	}
	close(block)
	rec.Close()

	mu.Lock()
	defer mu.Unlock()
	if dropped == 0 {
		t.Fatal("a full queue shed decisions with no overflow report — the trail is short and " +
			"nothing says so")
	}
	if int64(dropped) != rec.Dropped() {
		t.Errorf("handler saw %d drops, counter says %d", dropped, rec.Dropped())
	}
}

// A PUBLISH FAILURE IS REPORTED. The broker refusing and the queue overflowing
// are different incidents with different fixes, and both end with a decision that
// is not on the chain.
func TestAPublishFailureIsReported(t *testing.T) {
	b := &asyncBus{err: errors.New("broker refused")}
	var mu sync.Mutex
	var errs int
	rec := mustAsync(t, b, WithErrorHandler(func(error) {
		mu.Lock()
		defer mu.Unlock()
		errs++
	}))
	_ = rec.Record(context.Background(), asyncPreTrade("ORD-13"))
	rec.Close()

	mu.Lock()
	defer mu.Unlock()
	if errs != 1 {
		t.Errorf("error handler fired %d times, want 1 — a decision that never reached the trail is "+
			"invisible", errs)
	}
}

// A RECORD WITH NO RESULT IS REFUSED WHERE THE CALLER CAN STILL BE NAMED. It
// cannot be stamped with an evaluation time, so the broker would refuse it — and
// on a background worker that refusal arrives detached from whatever produced it.
func TestARecordWithNoResultIsRefusedBeforeQueueing(t *testing.T) {
	b := &asyncBus{}
	rec := mustAsync(t, b)
	if err := rec.Record(context.Background(), comp.DecisionRecord{Phase: comp.PhasePreTrade}); err != nil {
		t.Fatalf("Record returned an error for an unusable record; it must stay best-effort: %v", err)
	}
	rec.Close()
	if got := len(b.seen()); got != 0 {
		t.Errorf("published %d events for a record with no result", got)
	}
}

// CLOSE DRAINS, so the decisions buffered at shutdown still reach the trail.
func TestCloseDrainsTheQueue(t *testing.T) {
	b := &asyncBus{}
	rec := mustAsync(t, b)
	for i := 0; i < 25; i++ {
		_ = rec.Record(context.Background(), asyncPreTrade("ORD-drain"))
	}
	rec.Close()
	if got := len(b.seen()); got != 25 {
		t.Errorf("published %d of 25 queued decisions before shutdown completed — the rest were "+
			"discarded exactly across a deploy", got)
	}
}

// A NIL BUS IS REFUSED AT CONSTRUCTION rather than degrading to a queue that
// publishes nowhere, which at the gate is indistinguishable from one keeping up.
func TestANilBusIsRefused(t *testing.T) {
	if _, err := NewAsyncRecorder(nil, asyncQuiet()); err == nil {
		t.Fatal("NewAsyncRecorder accepted a nil bus")
	}
}
