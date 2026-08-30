package outbox

// THE CAUSE OF A STALL MUST BE READABLE WITHOUT A DATABASE SESSION (#817).
//
// `last_error` was written on every failed attempt and cleared on every
// successful publish, and NOTHING read it back: it was absent from
// pendingColumns, absent from Pending, and absent from every log line, metric
// and read surface. The only way to see why the head of a key would not publish
// was psql against production during the incident that made it matter.
//
// These tests run against the in-process queue, so they prove the CONTRACT — a
// cause recorded by MarkFailed comes back on the next Pending, and the relay
// reports it — but not the COLUMN. postgres_test.go carries the column, gated on
// TEST_POSTGRES_URL.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// capturingHandler collects slog records so a test can assert on the attribute
// an operator would actually read, rather than on a string the test built.
type capturingHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r.Clone())
	return nil
}

func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

// attrs returns the named attribute from every captured record that carries it.
func (h *capturingHandler) attrs(key string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.recs {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == key {
				out = append(out, a.Value.String())
			}
			return true
		})
	}
	return out
}

// THE QUEUE MUST HAND BACK THE CAUSE IT WAS GIVEN.
//
// This is the whole read path in one assertion. Without it MarkFailed is a
// write-only sink: the attempt count climbs, the age gauge climbs, and the one
// field that says WHY is unreachable from Go.
func TestAFailedRecordReportsItsCauseOnTheNextRead(t *testing.T) {
	ctx := testCtx()
	q := NewMemory()
	mustAppend(t, q, fact(t, ctx, "order.order.accepted", "o1"))

	pending, err := q.Pending(ctx, "o1", 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("Pending before any attempt = (%d records, %v), want (1, nil)", len(pending), err)
	}
	if pending[0].LastError != "" {
		t.Errorf("a record nothing has tried reports LastError=%q, want empty — "+
			"a cause that appears before an attempt would make an untried record look poisoned",
			pending[0].LastError)
	}

	if err := q.MarkFailed(ctx, pending[0].ID, errors.New("broker refused: no responders for order.order.accepted")); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	pending, err = q.Pending(ctx, "o1", 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("Pending after a failed attempt = (%d records, %v), want (1, nil)", len(pending), err)
	}
	if !strings.Contains(pending[0].LastError, "no responders") {
		t.Errorf("LastError = %q after a failed publish, want the broker's refusal — "+
			"the cause was recorded and cannot be read back, which is #817: the only route to it "+
			"is a psql session against production during the outage", pending[0].LastError)
	}
	if pending[0].Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", pending[0].Attempts)
	}
}

// A RECORD THAT RECOVERS MUST STOP REPORTING THE BLIP IT RECOVERED FROM.
//
// Both queues clear the cause when the record publishes. Through the Queue
// interface only the disappearance is observable — Pending skips published rows
// — so this asserts the observable half here and
// TestPostgresOutboxClearsTheCauseWhenTheRecordFinallyPublishes reads the column
// itself, where the clear is what stops a recovered record from being reported
// as poisoned by anything that later queries the table.
func TestARecoveredRecordLeavesThePendingSetWithItsCause(t *testing.T) {
	ctx := testCtx()
	q := NewMemory()
	mustAppend(t, q, fact(t, ctx, "order.order.accepted", "o1"))

	pending, err := q.Pending(ctx, "o1", 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("Pending = (%d, %v)", len(pending), err)
	}
	id := pending[0].ID
	if err := q.MarkFailed(ctx, id, errors.New("a blip")); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if err := q.MarkPublished(ctx, id); err != nil {
		t.Fatalf("MarkPublished: %v", err)
	}
	pending, err = q.Pending(ctx, "o1", 10)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("a published record is still pending with LastError=%q", pending[0].LastError)
	}
}

// THE CAUSE IS BOUNDED IN ONE PLACE.
//
// It is a column, not a log: an unbounded broker error — a venue returning an
// HTML error page, a wrapped chain naming every hop — must not be what a row
// grows to. The bound lived only inside Postgres.MarkFailed, so the in-process
// queue would have stored an arbitrarily long string and the two Queue
// implementations would have disagreed about what they store.
func TestTheRecordedCauseIsBoundedTheSameWayEverywhere(t *testing.T) {
	huge := errors.New(strings.Repeat("x", causeLimit*3))
	got := boundedCause(huge)
	if len(got) != causeLimit {
		t.Errorf("boundedCause kept %d bytes of a %d-byte error, want %d — "+
			"an unbounded cause is a row that grows without limit",
			len(got), causeLimit*3, causeLimit)
	}

	ctx := testCtx()
	q := NewMemory()
	mustAppend(t, q, fact(t, ctx, "order.order.accepted", "o1"))
	pending, err := q.Pending(ctx, "o1", 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("Pending = (%d, %v)", len(pending), err)
	}
	if err := q.MarkFailed(ctx, pending[0].ID, huge); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	pending, err = q.Pending(ctx, "o1", 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("Pending = (%d, %v)", len(pending), err)
	}
	if len(pending[0].LastError) != causeLimit {
		t.Errorf("the in-process queue stored %d bytes where Postgres stores %d — "+
			"the two Queue implementations disagree about what a recorded cause is",
			len(pending[0].LastError), causeLimit)
	}
}

// THE RELAY REPORTS THE CAUSE IT INHERITED, NOT ONLY THE ONE IT JUST SAW.
//
// This is the surfacing #817 asks for, and the reason the live error alone is
// not it. The relay already logs the error from the attempt IT made. The record
// outlives the pod: oms-deploy.yaml runs replicas: 2, a pod is rescheduled, and
// the replica that now owns the key never saw the original refusal. What it can
// see is the column — and only once the column is on the projection.
//
// The test drives the second observation through a SECOND relay with its own
// logger, so the inherited cause cannot come from anything the first relay left
// in memory.
func TestTheRelayLogsTheCauseAPreviousAttemptRecorded(t *testing.T) {
	ctx := testCtx()
	q := NewMemory()
	mustAppend(t, q, fact(t, ctx, "order.order.accepted", "o1"))

	first := &recorder{failOn: "order.order.accepted"}
	relayA, err := NewRelay(q, first, quietLogger())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if n, err := relayA.DrainOnce(ctx); err != nil || n != 0 {
		t.Fatalf("first pass = (%d, %v), want (0, nil)", n, err)
	}

	// A different pod. Its publisher refuses for a DIFFERENT reason, so the two
	// causes are distinguishable and a test cannot pass by reporting the live one
	// twice.
	second := &recorder{failOn: "order.order.accepted", failWith: "connection reset by peer"}
	logs := &capturingHandler{}
	relayB, err := NewRelay(q, second, slog.New(logs))
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if n, err := relayB.DrainOnce(ctx); err != nil || n != 0 {
		t.Fatalf("second pass = (%d, %v), want (0, nil)", n, err)
	}

	prior := logs.attrs("prior_error")
	if len(prior) != 1 {
		t.Fatalf("the stall log carried %d prior_error attributes, want 1 — a replica that did not "+
			"witness the first failure reports only the symptom it just saw, and the recorded cause "+
			"stays reachable only by psql (#817)", len(prior))
	}
	if !strings.Contains(prior[0], "injected publish failure") {
		t.Errorf("prior_error = %q, want the cause the FIRST relay recorded — this replica is "+
			"reporting its own live error as the history", prior[0])
	}
	live := logs.attrs("err")
	if len(live) != 1 || !strings.Contains(live[0], "connection reset by peer") {
		t.Errorf("err = %v, want the live refusal — the live error must not be replaced by the "+
			"inherited one; the pair is what says whether the condition changed shape", live)
	}
}
