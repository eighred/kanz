package custody

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/recon"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"
)

func testRunContext(ctx context.Context) context.Context { return bus.WithTenantID(ctx, "__system__") }

func TestReconcileCompletionCaptureFailureDoesNotResolveBreaks(t *testing.T) {
	st := NewMemoryStore()
	seed := detectedQuantityBreak(t0, 100, 90)
	if _, err := st.UpsertBreaks(t.Context(), subject(), seed, t0); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveStatement(t.Context(), statement("agreed", map[string]int64{"AAPL": 100}, nil)); err != nil {
		t.Fatal(err)
	}
	pub := &capturePublisher{}
	r, err := NewReconciler(st, loaderFor(bookWith(map[string]int64{"AAPL": 100}, nil)), pub, nil, nil, testLogger(), func() time.Time { return t0.Add(time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
	// Without a tenant, no canonical completion record can be captured. The
	// otherwise clean comparison must not resolve an operator's existing break.
	_, reconcileErr := r.Reconcile(t.Context(), subject())
	b, err := st.LoadBreak(t.Context(), seed[0].BreakID)
	if err != nil || b.Status != BreakOpen || b.Revision != 1 {
		t.Fatalf("capture failure committed a resolution: %+v %v", b, err)
	}
	if reconcileErr == nil {
		t.Fatal("accepted a run whose completion could not be captured")
	}
	if _, err := st.LatestRun(t.Context(), "PF1", "CUST-A"); !errors.Is(err, ErrNoRun) {
		t.Fatalf("capture failure committed a run: %v", err)
	}
	if len(pub.events) != 0 || st.outbox.PendingCount() != 0 {
		t.Fatal("capture failure emitted a completion")
	}
}

func TestMemoryCustodyRunCaptureFailureRollsBackLifecycle(t *testing.T) {
	for _, failure := range []string{"missing_tenant", "unrepresentable_fact"} {
		t.Run(failure, func(t *testing.T) {
			st := NewMemoryStore()
			ctx := testRunContext(t.Context())
			seed := detectedQuantityBreak(t0, 100, 90)
			if _, err := st.UpsertBreaks(ctx, subject(), seed, t0); err != nil {
				t.Fatal(err)
			}
			run := Run{RunID: "refused", Subject: subject(), Outcome: OutcomeClean, CompletedAt: t0.Add(time.Hour), Tolerance: new(big.Rat)}
			if failure == "missing_tenant" {
				ctx = t.Context()
			} else {
				run.Outcome = OutcomeBreaks
				run.Breaks = detectedQuantityBreak(t0, 100, 80)
				run.Breaks[0].Diff = new(big.Rat).SetFrac64(1, 3)
			}
			if _, err := st.CommitRun(ctx, run); err == nil {
				t.Fatal("completion capture failure was accepted")
			}
			b, err := st.LoadBreak(ctx, seed[0].BreakID)
			if err != nil || b.Status != BreakOpen || b.Revision != 1 || b.Diff.Cmp(big.NewRat(10, 1)) != 0 {
				t.Fatalf("failed capture changed lifecycle: %+v %v", b, err)
			}
			if _, err := st.LatestRun(ctx, "PF1", "CUST-A"); !errors.Is(err, ErrNoRun) {
				t.Fatalf("failed capture committed run: %v", err)
			}
			if st.outbox.PendingCount() != 0 {
				t.Fatal("failed capture committed a completion")
			}
		})
	}
}

func TestCustodyRunPublicationPendingPreservesCommitAndRetries(t *testing.T) {
	st := NewMemoryStore()
	ctx := testRunContext(t.Context())
	if err := st.SaveStatement(ctx, statement("S1", map[string]int64{"AAPL": 90}, nil)); err != nil {
		t.Fatal(err)
	}
	pub := &capturePublisher{err: errors.New("broker unavailable")}
	clock := t0
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	metrics.SeedPair("PF1", "CUST-A")
	r, err := NewReconciler(st, loaderFor(bookWith(map[string]int64{"AAPL": 100}, nil)), pub, new(big.Rat), metrics, nil, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	first, err := r.Reconcile(ctx, subject())
	if !errors.Is(err, ErrPublicationPending) || first.Outcome != OutcomeBreaks || st.outbox.PendingCount() != 1 {
		t.Fatalf("business result/pending publication confused: %+v %v", first, err)
	}
	b, _ := st.LoadBreak(ctx, first.Breaks[0].BreakID)
	if b.Revision != 1 {
		t.Fatal("business transition missing")
	}
	if v, ok := gaugeValue(t, reg, "kanz_accounting_reconciliation_last_success_timestamp_seconds", map[string]string{"portfolio": "PF1", "custodian": "CUST-A"}); !ok || v != 0 {
		t.Fatal("pending publication advanced delivered-success metric")
	}
	if _, err := r.Reconcile(ctx, subject()); !errors.Is(err, ErrPublicationPending) {
		t.Fatal(err)
	}
	b, _ = st.LoadBreak(ctx, b.BreakID)
	if b.Revision != 1 || st.outbox.PendingCount() != 1 {
		t.Fatal("retry repeated the transition or completion")
	}
	clock = clock.Add(time.Minute)
	if _, err := r.Reconcile(ctx, subject()); !errors.Is(err, ErrPublicationPending) || st.outbox.PendingCount() != 2 {
		t.Fatalf("later run lost earlier completion: %v", err)
	}
	pub.err = nil
	relay, err := outbox.NewRelay(st.Outbox(), pub, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if n, err := relay.DrainOnce(ctx); err != nil || n != 2 {
		t.Fatalf("restart relay: sent=%d err=%v", n, err)
	}
	if n, err := relay.DrainOnce(ctx); err != nil || n != 0 {
		t.Fatalf("repeated relay duplicated completions: sent=%d err=%v", n, err)
	}
	if len(pub.events) != 2 || pub.events[0].CorrelationID != first.RunID || pub.events[1].CorrelationID == first.RunID {
		t.Fatal("relay lost or reordered an earlier immutable completion")
	}
	b, _ = st.LoadBreak(ctx, b.BreakID)
	if b.Revision != 2 {
		t.Fatalf("later run/repeated relay changed lifecycle incorrectly: %+v", b)
	}
}

func TestCustodyRunMissingPublisherIsPendingNotHealthy(t *testing.T) {
	st := NewMemoryStore()
	r, err := NewReconciler(st, loaderFor(bookWith(nil, nil)), nil, new(big.Rat), nil, testLogger(), func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	run, err := r.Reconcile(testRunContext(t.Context()), subject())
	if !errors.Is(err, ErrPublicationPending) || run.Outcome != OutcomeNoStatement || st.outbox.PendingCount() != 1 {
		t.Fatalf("missing publisher appeared healthy or lost completion: %+v %v", run, err)
	}
}

func TestCustodyFailedAndNoStatementRunsDoNotResolveBreaks(t *testing.T) {
	for _, outcome := range []Outcome{OutcomeFailed, OutcomeNoStatement} {
		st := NewMemoryStore()
		ctx := testRunContext(t.Context())
		seed := FromRecon(subject(), []recon.Break{{Kind: recon.BreakQuantity, Key: "AAPL", IBOR: big.NewRat(100, 1), Custodian: big.NewRat(90, 1), Diff: big.NewRat(10, 1)}}, t0)
		if _, err := st.UpsertBreaks(ctx, subject(), seed, t0); err != nil {
			t.Fatal(err)
		}
		if _, err := st.CommitRun(ctx, Run{RunID: outcome.String(), Subject: subject(), Outcome: outcome, CompletedAt: t0, Tolerance: new(big.Rat)}); err != nil {
			t.Fatal(err)
		}
		b, _ := st.LoadBreak(ctx, seed[0].BreakID)
		if b.Status != BreakOpen || b.Revision != 1 || st.outbox.PendingCount() != 1 {
			t.Fatal("diagnostic run resolved an unassessed difference")
		}
	}
}

func TestCustodyRunCompletionRoundTripAndIdentity(t *testing.T) {
	st := NewMemoryStore()
	ctx := testRunContext(t.Context())
	run := Run{RunID: "original", Subject: subject(), Outcome: OutcomeBreaks, Breaks: detectedQuantityBreak(t0, 100, 90), CompletedAt: t0, Tolerance: new(big.Rat)}
	first, err := st.CommitRun(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	records, err := st.Outbox().Pending(ctx, pairKey("PF1", "CUST-A"), 10)
	if err != nil || len(records) != 1 {
		t.Fatalf("completion capture: %+v %v", records, err)
	}
	restored, err := runFromRecord(records[0].Record.Payload)
	if err != nil {
		t.Fatal(err)
	}
	want, err := runToProto(first)
	if err != nil {
		t.Fatal(err)
	}
	got, err := runToProto(restored)
	if err != nil || !proto.Equal(got, want) {
		t.Fatalf("immutable completion round trip: got=%v want=%v err=%v", got, want, err)
	}
	conflict := run
	conflict.Subject.CustodianID = "OTHER"
	if _, err := st.CommitRun(ctx, conflict); !errors.Is(err, ErrRunIdentityConflict) {
		t.Fatalf("run ID accepted for a different subject: %v", err)
	}
	legacy := run
	legacy.RunID = "legacy"
	if err := st.SaveRun(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CommitRun(ctx, legacy); !errors.Is(err, ErrRunCompletionMissing) {
		t.Fatalf("invented immutable completion for a historical run: %v", err)
	}
}

func TestMemoryCustodyRunHistoryRemainsImmutableAfterDelivery(t *testing.T) {
	st := NewMemoryStore()
	ctx := testRunContext(t.Context())
	run := Run{RunID: "original", Subject: subject(), Outcome: OutcomeBreaks, Breaks: detectedQuantityBreak(t0, 100, 90), CompletedAt: t0, Tolerance: new(big.Rat)}
	first, err := st.CommitRun(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	want, err := runToProto(first)
	if err != nil {
		t.Fatal(err)
	}
	pub := &capturePublisher{}
	relay, err := outbox.NewRelay(st.Outbox(), pub, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if sent, err := relay.DrainOnce(ctx); err != nil || sent != 1 || st.outbox.PendingCount() != 0 || len(pub.events) != 1 {
		t.Fatalf("completion was not actually evicted after delivery: sent=%d err=%v", sent, err)
	}
	// Neither the input, the commit result nor a read result owns the store's
	// canonical history. The queue no longer has the published payload to repair it.
	run.Breaks[0].Diff.SetInt64(999)
	run.Tolerance.SetInt64(999)
	first.Breaks[0].IBOR.SetInt64(999)
	latest, err := st.LatestRun(ctx, "PF1", "CUST-A")
	if err != nil {
		t.Fatal(err)
	}
	latest.Breaks[0].Diff.SetInt64(777)
	latest.Breaks[0].Assignee = "caller"
	latest.Tolerance.SetInt64(777)
	fresh, err := st.LatestRun(ctx, "PF1", "CUST-A")
	got, encodeErr := runToProto(fresh)
	if err != nil || encodeErr != nil || !proto.Equal(got, want) {
		t.Fatalf("caller changed canonical run history: got=%v want=%v err=%v/%v", got, want, err, encodeErr)
	}
	b, err := st.LoadBreak(ctx, run.Breaks[0].BreakID)
	if err != nil || b.Diff.Cmp(big.NewRat(10, 1)) != 0 {
		t.Fatalf("commit input changed stored lifecycle: %+v %v", b, err)
	}
	// Memory actions remain disabled. The existing lifecycle fixture models an
	// operator's assignment without granting a volatile store durable authority.
	if err := b.Assign("alice", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	b.Revision++ // The fixture includes the revision an applied assignment advances.
	if err := recordFixtureLifecycle(t, st, ctx, b); err != nil {
		t.Fatal(err)
	}
	later := Run{RunID: "later", Subject: subject(), Outcome: OutcomeBreaks, Breaks: detectedQuantityBreak(t0, 100, 80), CompletedAt: t0.Add(time.Hour), Tolerance: new(big.Rat)}
	if _, err := st.CommitRun(ctx, later); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		repeated, err := st.CommitRun(ctx, run)
		got, encodeErr := runToProto(repeated)
		if err != nil || encodeErr != nil || !proto.Equal(got, want) {
			t.Fatalf("retry lost original immutable completion: got=%v want=%v err=%v/%v", got, want, err, encodeErr)
		}
		repeated.Breaks[0].Diff.SetInt64(555)
		repeated.Tolerance.SetInt64(555)
	}
	b, err = st.LoadBreak(ctx, b.BreakID)
	if err != nil || b.Revision != 3 || b.Status != BreakAssigned || b.Assignee != "alice" || b.Diff.Cmp(big.NewRat(20, 1)) != 0 || st.outbox.PendingCount() != 1 {
		t.Fatalf("retry repeated lifecycle or queued original again: %+v %v", b, err)
	}
}

func TestMemoryCustodyLegacyRunHistoryIsDetachedWithoutCompletion(t *testing.T) {
	st := NewMemoryStore()
	run := Run{RunID: "legacy", Subject: subject(), Outcome: OutcomeBreaks, Breaks: detectedQuantityBreak(t0, 100, 90), CompletedAt: t0, Tolerance: new(big.Rat)}
	if err := st.SaveRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	run.Breaks[0].Diff.SetInt64(999)
	run.Tolerance.SetInt64(999)
	latest, err := st.LatestRun(t.Context(), "PF1", "CUST-A")
	if err != nil || latest.Breaks[0].Diff.Cmp(big.NewRat(10, 1)) != 0 || latest.Tolerance.Sign() != 0 {
		t.Fatalf("legacy input changed saved history: %+v %v", latest, err)
	}
	latest.Breaks[0].Diff.SetInt64(777)
	if _, err := st.CommitRun(testRunContext(t.Context()), run); !errors.Is(err, ErrRunCompletionMissing) {
		t.Fatalf("legacy identity acquired a fabricated completion: %v", err)
	}
	latest, err = st.LatestRun(t.Context(), "PF1", "CUST-A")
	if err != nil || latest.Breaks[0].Diff.Cmp(big.NewRat(10, 1)) != 0 || st.outbox.PendingCount() != 0 {
		t.Fatalf("legacy read changed history or enqueued a completion: %+v %v", latest, err)
	}
}
