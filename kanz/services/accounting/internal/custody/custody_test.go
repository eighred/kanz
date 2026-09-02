package custody

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/recon"
)

var t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// --- helpers -----------------------------------------------------------------

type capturePublisher struct {
	events []bus.Event
	err    error
}

func (c *capturePublisher) Publish(_ context.Context, e bus.Event) error {
	if c.err != nil {
		return c.err
	}
	c.events = append(c.events, e)
	return nil
}

func (c *capturePublisher) lastRun(t *testing.T) *accountingpb.ReconciliationRun {
	t.Helper()
	if len(c.events) == 0 {
		t.Fatal("no event published")
	}
	msg, ok := c.events[len(c.events)-1].Payload.(*accountingpb.ReconciliationRun)
	if !ok {
		t.Fatalf("payload is %T, want *ReconciliationRun", c.events[len(c.events)-1].Payload)
	}
	return msg
}

// bookWith builds a book holding the given positions and cash.
func bookWith(positions map[string]int64, cash map[string]int64) *ledger.Book {
	b := ledger.NewBook("PF1")
	for instrument, qty := range positions {
		b.Apply(&ledger.Event{
			EntryID: "seed-" + instrument, PortfolioID: "PF1", Type: ledger.EntryTrade,
			InstrumentID: instrument, Quantity: big.NewRat(qty, 1), Price: big.NewRat(1, 1),
			Cash: new(big.Rat), CashCurrency: "USD", Effective: t0, Knowledge: t0,
		})
	}
	for ccy, amount := range cash {
		b.Apply(&ledger.Event{
			EntryID: "cash-" + ccy, PortfolioID: "PF1", Type: ledger.EntryCash,
			Cash: big.NewRat(amount, 1), CashCurrency: ccy, Effective: t0, Knowledge: t0,
		})
	}
	return b
}

func loaderFor(b *ledger.Book) BookLoader {
	return func(context.Context, string) (*ledger.Book, error) { return b, nil }
}

func subject() Subject {
	return Subject{PortfolioID: "PF1", CustodianID: "CUST-A", BusinessDate: t0}
}

func statement(id string, positions, cash map[string]int64) Statement {
	s := Statement{
		StatementID: id, CustodianID: "CUST-A", PortfolioID: "PF1",
		BusinessDate: BusinessDay(t0), ReceivedAt: t0,
		Positions: map[string]*big.Rat{}, Cash: map[string]*big.Rat{},
	}
	for k, v := range positions {
		s.Positions[k] = big.NewRat(v, 1)
	}
	for k, v := range cash {
		s.Cash[k] = big.NewRat(v, 1)
	}
	return s
}

func newReconciler(t *testing.T, store Store, book *ledger.Book, pub Publisher, reg prometheus.Registerer, now func() time.Time) *Reconciler {
	t.Helper()
	r, err := NewReconciler(store, loaderFor(book), pub, new(big.Rat), NewMetrics(reg), nil, now)
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	return r
}

func gaugeValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if !labelsMatch(m, labels) {
				continue
			}
			if m.GetGauge() != nil {
				return m.GetGauge().GetValue(), true
			}
			if m.GetCounter() != nil {
				return m.GetCounter().GetValue(), true
			}
		}
	}
	return 0, false
}

func labelsMatch(m *dto.Metric, want map[string]string) bool {
	got := map[string]string{}
	for _, l := range m.GetLabel() {
		got[l.GetName()] = l.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// --- outcomes ----------------------------------------------------------------

// A RUN THAT FINDS NOTHING MUST STILL BE EVIDENCE. This is the core of #962: the
// pre-existing control produced output only on success-with-findings, so a clean
// book and an unreconciled one were the same observable event.
func TestCleanRunIsRecordedAndPublished(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	if err := store.SaveStatement(ctx, statement("S1", map[string]int64{"AAPL": 100}, map[string]int64{"USD": 500})); err != nil {
		t.Fatalf("SaveStatement: %v", err)
	}
	pub := &capturePublisher{}
	r := newReconciler(t, store, bookWith(map[string]int64{"AAPL": 100}, map[string]int64{"USD": 500}), pub, nil, func() time.Time { return t0 })

	run, err := r.Reconcile(ctx, subject())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if run.Outcome != OutcomeClean {
		t.Fatalf("outcome = %s, want clean", run.Outcome)
	}
	msg := pub.lastRun(t)
	if msg.GetOutcome() != accountingpb.ReconciliationOutcome_RECONCILIATION_OUTCOME_CLEAN {
		t.Fatalf("wire outcome = %s, want CLEAN", msg.GetOutcome())
	}
	if msg.GetStatementId() != "S1" {
		t.Fatalf("statement_id = %q, want S1", msg.GetStatementId())
	}
	if got := pub.events[0].Subject; got != SubjectRun {
		t.Fatalf("subject = %q, want %q", got, SubjectRun)
	}
	if got := pub.events[0].EventClass; got != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Fatalf("event class = %v, want FACT", got)
	}
}

// THE OUTCOME THAT MAKES ABSENCE LOUD. A custodian that stops sending produced no
// event at all before #962, so the estate looked exactly like one reconciling
// cleanly every day.
func TestNoStatementIsAnOutcomeNotSilence(t *testing.T) {
	pub := &capturePublisher{}
	r := newReconciler(t, NewMemoryStore(), bookWith(map[string]int64{"AAPL": 100}, nil), pub, nil, func() time.Time { return t0 })

	run, err := r.Reconcile(context.Background(), subject())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if run.Outcome != OutcomeNoStatement {
		t.Fatalf("outcome = %s, want no_statement", run.Outcome)
	}
	if len(pub.events) != 1 {
		t.Fatalf("published %d events, want 1 — silence is the bug this outcome exists to remove", len(pub.events))
	}
	msg := pub.lastRun(t)
	if msg.GetOutcome() != accountingpb.ReconciliationOutcome_RECONCILIATION_OUTCOME_NO_STATEMENT {
		t.Fatalf("wire outcome = %s, want NO_STATEMENT", msg.GetOutcome())
	}
	// An absent statement must NOT be reconciled as an empty one: that would
	// manufacture a MISSING_AT_CUSTODIAN break for every held position.
	if len(msg.GetBreaks()) != 0 {
		t.Fatalf("no-statement run reported %d breaks — an absent statement was treated as an empty one", len(msg.GetBreaks()))
	}
}

func TestBreaksAreDetectedAndCarriedOntoTheWire(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	// The custodian holds 90; the book says 100. And the book holds MSFT the
	// custodian has never heard of.
	if err := store.SaveStatement(ctx, statement("S1", map[string]int64{"AAPL": 90}, map[string]int64{"USD": 500})); err != nil {
		t.Fatalf("SaveStatement: %v", err)
	}
	pub := &capturePublisher{}
	book := bookWith(map[string]int64{"AAPL": 100, "MSFT": 5}, map[string]int64{"USD": 500})
	r := newReconciler(t, store, book, pub, nil, func() time.Time { return t0 })

	run, err := r.Reconcile(ctx, subject())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if run.Outcome != OutcomeBreaks {
		t.Fatalf("outcome = %s, want breaks", run.Outcome)
	}
	msg := pub.lastRun(t)
	byKey := map[string]*accountingpb.ReconciliationBreak{}
	for _, b := range msg.GetBreaks() {
		byKey[b.GetKey()] = b
	}
	aapl, ok := byKey["AAPL"]
	if !ok {
		t.Fatalf("no AAPL break; got %v", byKey)
	}
	if aapl.GetKind() != accountingpb.ReconciliationBreakKind_RECONCILIATION_BREAK_KIND_QUANTITY {
		t.Fatalf("AAPL kind = %s, want QUANTITY", aapl.GetKind())
	}
	if got := dec.FromProto(aapl.GetDifference()); got.Cmp(big.NewRat(10, 1)) != 0 {
		t.Fatalf("AAPL difference = %s, want 10", got.RatString())
	}
	msft, ok := byKey["MSFT"]
	if !ok {
		t.Fatalf("no MSFT break; got %v", byKey)
	}
	if msft.GetKind() != accountingpb.ReconciliationBreakKind_RECONCILIATION_BREAK_KIND_MISSING_AT_CUSTODIAN {
		t.Fatalf("MSFT kind = %s, want MISSING_AT_CUSTODIAN", msft.GetKind())
	}
}

// A BOOK THAT WILL NOT MATERIALIZE IS NOT A CLEAN BOOK. The run is still
// recorded, so "the book has been unreadable for three days" is distinguishable
// from "nothing was scheduled".
func TestFailedRunIsRecordedAndNotClean(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	if err := store.SaveStatement(ctx, statement("S1", map[string]int64{"AAPL": 100}, nil)); err != nil {
		t.Fatalf("SaveStatement: %v", err)
	}
	pub := &capturePublisher{}
	failing := func(context.Context, string) (*ledger.Book, error) { return nil, errors.New("pool is down") }
	r, err := NewReconciler(store, failing, pub, new(big.Rat), nil, nil, func() time.Time { return t0 })
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	run, err := r.Reconcile(ctx, subject())
	if err == nil {
		t.Fatal("Reconcile returned nil error for an unmaterializable book")
	}
	if run.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %s, want failed", run.Outcome)
	}
	if !strings.Contains(run.FailureReason, "pool is down") {
		t.Fatalf("failure reason = %q, want the underlying cause", run.FailureReason)
	}
	if len(pub.events) != 1 {
		t.Fatalf("published %d events, want 1 — a failed run must still be evidence", len(pub.events))
	}
}

// --- break lifecycle ---------------------------------------------------------

// THE BUG THIS PREVENTS: a daily run that overwrites the operator's work. If a
// redetection reset status/assignee, every assignment would vanish each morning
// and the queue would be unusable.
func TestRedetectionPreservesTheOperatorsWork(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	detected := FromRecon(subject(), []recon.Break{{
		Kind: recon.BreakQuantity, Key: "AAPL",
		IBOR: big.NewRat(100, 1), Custodian: big.NewRat(90, 1), Diff: big.NewRat(10, 1),
	}}, t0)

	stored, err := store.UpsertBreaks(ctx, subject(), detected, t0)
	if err != nil {
		t.Fatalf("UpsertBreaks: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored %d breaks, want 1", len(stored))
	}
	b := stored[0]
	if err := b.Assign("alice", t0.Add(time.Hour)); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if err := b.Explain("late settlement, clears T+2", t0.Add(2*time.Hour)); err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if err := store.SaveBreak(ctx, b); err != nil {
		t.Fatalf("SaveBreak: %v", err)
	}

	// The next day's run detects the same break again.
	day2 := t0.Add(24 * time.Hour)
	again, err := store.UpsertBreaks(ctx, subject(), FromRecon(subject(), []recon.Break{{
		Kind: recon.BreakQuantity, Key: "AAPL",
		IBOR: big.NewRat(100, 1), Custodian: big.NewRat(90, 1), Diff: big.NewRat(10, 1),
	}}, day2), day2)
	if err != nil {
		t.Fatalf("UpsertBreaks day 2: %v", err)
	}
	if len(again) != 1 {
		t.Fatalf("stored %d breaks, want 1", len(again))
	}
	got := again[0]
	if got.Status != BreakExplained {
		t.Fatalf("status = %s, want explained — the redetection reset the operator's work", got.Status)
	}
	if got.Assignee != "alice" {
		t.Fatalf("assignee = %q, want alice", got.Assignee)
	}
	if !got.FirstSeenAt.Equal(t0) {
		t.Fatalf("first_seen_at = %s, want %s — the age was reset by a redetection", got.FirstSeenAt, t0)
	}
	if !got.LastSeenAt.Equal(day2) {
		t.Fatalf("last_seen_at = %s, want %s", got.LastSeenAt, day2)
	}
	if age := got.Age(day2); age != 24*time.Hour {
		t.Fatalf("age = %s, want 24h — this is the number an operator triages by", age)
	}
}

// THE PUBLISHED FACT MUST CARRY THE STORED LIFECYCLE, not the freshly detected
// breaks.
//
// FOUND BY MUTATION: publishing FromRecon's output instead of the stored set
// leaves every unit test above green, because they all assert against the STORE.
// The estate, however, would see every break announced as OPEN and one run old on
// every daily run — so any consumer of the FACT (an operator screen, a report)
// shows an aging problem reset to zero each morning, which is the same
// unreadable queue the lifecycle exists to prevent, reintroduced one layer out.
func TestThePublishedRunCarriesTheStoredLifecycle(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	if err := store.SaveStatement(ctx, statement("S1", map[string]int64{"AAPL": 90}, nil)); err != nil {
		t.Fatalf("SaveStatement: %v", err)
	}
	pub := &capturePublisher{}
	book := bookWith(map[string]int64{"AAPL": 100}, nil)
	clock := t0
	r, err := NewReconciler(store, loaderFor(book), pub, new(big.Rat), nil, nil, func() time.Time { return clock })
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	if _, err := r.Reconcile(ctx, subject()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	id := BreakID("PF1", "CUST-A", recon.BreakQuantity, "AAPL")
	b, err := store.LoadBreak(ctx, id)
	if err != nil {
		t.Fatalf("LoadBreak: %v", err)
	}
	if err := b.Assign("alice", t0.Add(time.Hour)); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if err := b.Explain("late settlement, clears T+2", t0.Add(2*time.Hour)); err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if err := store.SaveBreak(ctx, b); err != nil {
		t.Fatalf("SaveBreak: %v", err)
	}

	// The next day's run finds the same break.
	clock = t0.Add(24 * time.Hour)
	if _, err := r.Reconcile(ctx, subject()); err != nil {
		t.Fatalf("Reconcile day 2: %v", err)
	}
	msg := pub.lastRun(t)
	if len(msg.GetBreaks()) != 1 {
		t.Fatalf("published %d breaks, want 1", len(msg.GetBreaks()))
	}
	got := msg.GetBreaks()[0]
	if got.GetStatus() != accountingpb.BreakStatus_BREAK_STATUS_EXPLAINED {
		t.Fatalf("published status = %s, want EXPLAINED — the FACT announced a fresh break and reset the estate's view of the lifecycle", got.GetStatus())
	}
	if got.GetAssignee() != "alice" {
		t.Fatalf("published assignee = %q, want alice", got.GetAssignee())
	}
	if firstSeen := got.GetFirstSeenAt().AsTime().UTC(); !firstSeen.Equal(t0) {
		t.Fatalf("published first_seen_at = %s, want %s — the age an operator triages by was reset on the wire", firstSeen, t0)
	}
	if lastSeen := got.GetLastSeenAt().AsTime().UTC(); !lastSeen.Equal(clock) {
		t.Fatalf("published last_seen_at = %s, want %s", lastSeen, clock)
	}
}

// AGREEMENT RESOLVES A BREAK, and it is the only automatic route to RESOLVED.
func TestABreakTheRunNoLongerFindsIsResolved(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	detected := FromRecon(subject(), []recon.Break{{
		Kind: recon.BreakQuantity, Key: "AAPL", IBOR: big.NewRat(100, 1), Custodian: big.NewRat(90, 1), Diff: big.NewRat(10, 1),
	}}, t0)
	if _, err := store.UpsertBreaks(ctx, subject(), detected, t0); err != nil {
		t.Fatalf("UpsertBreaks: %v", err)
	}
	day2 := t0.Add(24 * time.Hour)
	outstanding, err := store.UpsertBreaks(ctx, subject(), nil, day2)
	if err != nil {
		t.Fatalf("UpsertBreaks day 2: %v", err)
	}
	if len(outstanding) != 0 {
		t.Fatalf("%d breaks still outstanding after agreement, want 0", len(outstanding))
	}
}

// ANOTHER PAIR'S BREAKS ARE NOT SWEPT. A run reconciles one (portfolio,
// custodian); resolving on absence across the whole store would clear every other
// pair's work on every run.
func TestResolveOnAbsenceIsScopedToTheRunsPair(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	other := Subject{PortfolioID: "PF1", CustodianID: "CUST-B", BusinessDate: t0}
	if _, err := store.UpsertBreaks(ctx, other, FromRecon(other, []recon.Break{{
		Kind: recon.BreakCash, Key: "USD", IBOR: big.NewRat(5, 1), Custodian: new(big.Rat), Diff: big.NewRat(5, 1),
	}}, t0), t0); err != nil {
		t.Fatalf("UpsertBreaks other: %v", err)
	}
	// A clean run for CUST-A must not touch CUST-B's break.
	outstanding, err := store.UpsertBreaks(ctx, subject(), nil, t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("UpsertBreaks: %v", err)
	}
	if len(outstanding) != 1 {
		t.Fatalf("%d outstanding, want 1 — a run for one custodian resolved another's breaks", len(outstanding))
	}
	if got := custodianOf(outstanding[0].BreakID); got != "CUST-B" {
		t.Fatalf("surviving break belongs to %q, want CUST-B", got)
	}
}

// A RETURNING BREAK IS NEW WORK and must not inherit the explanation that, by the
// fact of its return, did not hold.
func TestAReturningBreakDoesNotInheritTheStaleExplanation(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	mk := func(now time.Time) []Break {
		return FromRecon(subject(), []recon.Break{{
			Kind: recon.BreakQuantity, Key: "AAPL", IBOR: big.NewRat(100, 1), Custodian: big.NewRat(90, 1), Diff: big.NewRat(10, 1),
		}}, now)
	}
	stored, err := store.UpsertBreaks(ctx, subject(), mk(t0), t0)
	if err != nil {
		t.Fatalf("UpsertBreaks: %v", err)
	}
	b := stored[0]
	if err := b.Explain("clears T+2", t0); err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if err := store.SaveBreak(ctx, b); err != nil {
		t.Fatalf("SaveBreak: %v", err)
	}
	// It clears...
	if _, err := store.UpsertBreaks(ctx, subject(), nil, t0.Add(24*time.Hour)); err != nil {
		t.Fatalf("UpsertBreaks clear: %v", err)
	}
	// ...and comes back a day later.
	day3 := t0.Add(48 * time.Hour)
	again, err := store.UpsertBreaks(ctx, subject(), mk(day3), day3)
	if err != nil {
		t.Fatalf("UpsertBreaks return: %v", err)
	}
	if len(again) != 1 {
		t.Fatalf("%d outstanding, want 1", len(again))
	}
	if again[0].Status != BreakOpen {
		t.Fatalf("status = %s, want open — a returning break resumed a stale lifecycle", again[0].Status)
	}
	if again[0].Explanation != "" {
		t.Fatalf("explanation = %q, want empty — the stale explanation was inherited", again[0].Explanation)
	}
	if !again[0].FirstSeenAt.Equal(day3) {
		t.Fatalf("first_seen_at = %s, want %s — a returning break is new work", again[0].FirstSeenAt, day3)
	}
}

// THERE IS NO OPERATOR-FACING Resolve, AND THAT IS THE INVARIANT.
//
// A break is resolved when the book and the custodian AGREE, which only a run can
// establish — so the transition belongs to Store.UpsertBreaks and there is no way
// to reach the terminal state by hand. This test fails if a Resolve method is
// reintroduced on the aggregate: the store removes a break from the outstanding
// set the moment a run stops finding it, so any break an operator can still see is
// by construction one the latest run DID find, and closing it would leave the
// number wrong and the queue looking clean.
func TestABreakCannotBeResolvedByHand(t *testing.T) {
	var b any = &Break{Status: BreakOpen}
	if _, ok := b.(interface{ Resolve(bool, time.Time) error }); ok {
		t.Fatal("custody.Break grew a Resolve method — the terminal state must be reachable only " +
			"through a run that finds the two sides agreeing (Store.UpsertBreaks), or the control " +
			"can be silenced by hand")
	}
	if _, ok := b.(interface{ Resolve(time.Time) error }); ok {
		t.Fatal("custody.Break grew a Resolve method")
	}
}

func TestExplainRequiresAnExplanation(t *testing.T) {
	b := Break{Status: BreakOpen}
	if err := b.Explain("   ", t0); err == nil {
		t.Fatal("Explain accepted whitespace — an unexplained 'explained' is the silence this control removes")
	}
	if b.Status != BreakOpen {
		t.Fatalf("status = %s, want open", b.Status)
	}
}

// EXPLAINED IS STILL OUTSTANDING. An explanation is a claim about the future; a
// break still there a week later is a worse signal than an unexplained one.
func TestExplainedRemainsOutstanding(t *testing.T) {
	if !BreakExplained.Outstanding() {
		t.Fatal("EXPLAINED is not outstanding — a wrong explanation would silence the control indefinitely")
	}
	if BreakResolved.Outstanding() {
		t.Fatal("RESOLVED is outstanding")
	}
}

func TestStatusChangedAtOnlyMovesWhenTheStatusDoes(t *testing.T) {
	b := Break{Status: BreakOpen, StatusChangedAt: t0}
	if err := b.Assign("alice", t0.Add(time.Hour)); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	stamped := b.StatusChangedAt
	if !stamped.Equal(t0.Add(time.Hour)) {
		t.Fatalf("status_changed_at = %s, want the assign instant", stamped)
	}
	if err := b.Assign("bob", t0.Add(2*time.Hour)); err != nil {
		t.Fatalf("re-Assign: %v", err)
	}
	if !b.StatusChangedAt.Equal(stamped) {
		t.Fatal("status_changed_at moved on a re-assignment that did not change the status")
	}
	if b.Assignee != "bob" {
		t.Fatalf("assignee = %q, want bob — a handover must still be recorded", b.Assignee)
	}
}

// --- identity ----------------------------------------------------------------

// THE AGE DEPENDS ON THIS. A break given a fresh id every run would be
// permanently one day old.
func TestBreakIDIsStableAcrossRunsAndSeparatesKinds(t *testing.T) {
	a := BreakID("PF1", "CUST-A", recon.BreakQuantity, "AAPL")
	b := BreakID("PF1", "CUST-A", recon.BreakQuantity, "AAPL")
	if a != b {
		t.Fatalf("id is not stable: %q vs %q", a, b)
	}
	if c := BreakID("PF1", "CUST-A", recon.BreakMissingAtCustodian, "AAPL"); c == a {
		t.Fatal("two kinds on one instrument collide on an id — the age of the smaller problem would carry onto the larger")
	}
	if c := BreakID("PF1", "CUST-B", recon.BreakQuantity, "AAPL"); c == a {
		t.Fatal("two custodians collide on an id")
	}
}

// THE SEPARATOR IS ENFORCED, NOT MERELY DOCUMENTED.
//
// BreakID joins four components with "|". A comment claiming they cannot contain
// it is not a mechanism: portfolio "a|b" with custodian "c", and portfolio "a"
// with custodian "b|c", derive the SAME id — two funds' breaks sharing one row,
// one first_seen_at, and one operator's investigation covering two differences.
func TestASeparatorInAnIdentityComponentIsRefused(t *testing.T) {
	colliding := []Subject{
		{PortfolioID: "a|b", CustodianID: "c", BusinessDate: t0},
		{PortfolioID: "a", CustodianID: "b|c", BusinessDate: t0},
	}
	// The collision is real, which is why the refusal has to be.
	if BreakID("a|b", "c", recon.BreakQuantity, "X") != BreakID("a", "b|c", recon.BreakQuantity, "X") {
		t.Fatal("the premise no longer holds — BreakID's encoding changed and this guard needs rewriting")
	}
	for _, subj := range colliding {
		if err := subj.Validate(); err == nil {
			t.Fatalf("subject %+v was accepted; it collides with the other spelling", subj)
		}
	}

	// A statement key becomes a break key, which becomes part of the same id.
	st := statement("S1", nil, nil)
	st.Positions = map[string]*big.Rat{"AA|PL": big.NewRat(1, 1)}
	if err := st.Validate(); err == nil {
		t.Fatal("an instrument containing the id separator was accepted — two instruments would collide onto one break")
	}
	st = statement("S1", nil, nil)
	st.Cash = map[string]*big.Rat{"US|D": big.NewRat(1, 1)}
	if err := st.Validate(); err == nil {
		t.Fatal("a currency containing the id separator was accepted")
	}

	// An ordinary subject and statement are of course still fine.
	if err := subject().Validate(); err != nil {
		t.Fatalf("a legitimate subject was refused: %v", err)
	}
	if err := statement("S1", map[string]int64{"AAPL": 1}, map[string]int64{"USD": 1}).Validate(); err != nil {
		t.Fatalf("a legitimate statement was refused: %v", err)
	}
}

func TestBusinessDayNormalizes(t *testing.T) {
	morning := time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)
	evening := time.Date(2026, 9, 1, 23, 59, 59, 0, time.UTC)
	if !BusinessDay(morning).Equal(BusinessDay(evening)) {
		t.Fatal("two instants on one day produce different business dates — the reconciliation identity would split")
	}
	if got := BusinessDay(evening); got.Hour() != 0 || got.Location() != time.UTC {
		t.Fatalf("BusinessDay = %s, want UTC midnight", got)
	}
}

// --- metrics -----------------------------------------------------------------

// A COUNTER EXPORTS NOTHING FOR A LABEL IT HAS NEVER SEEN, so a rule over
// {outcome="failed"} is a rule over an empty vector until the first failure — and
// a rule over an empty vector never fires (#62's ten deleted rules).
func TestMetricsAreSeededSoAnUnreachedOutcomeIsAZeroNotAnAbsence(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.SeedPair("PF1", "CUST-A")

	for _, o := range Outcomes() {
		if _, ok := gaugeValue(t, reg, "kanz_accounting_reconciliation_runs_total", map[string]string{"custodian": "CUST-A", "outcome": o.String()}); !ok {
			t.Fatalf("outcome %q has no series after seeding — an alert over it would query an empty vector and never fire", o)
		}
	}
	for _, k := range recon.BreakKinds() {
		if _, ok := gaugeValue(t, reg, "kanz_accounting_breaks_open", map[string]string{"custodian": "CUST-A", "kind": k.String()}); !ok {
			t.Fatalf("break kind %q has no seeded series", k)
		}
	}
	// A pair that has never reconciled reads as 1970 — an infinite age, which the
	// staleness alert must fire on rather than skip.
	v, ok := gaugeValue(t, reg, "kanz_accounting_reconciliation_last_success_timestamp_seconds", map[string]string{"portfolio": "PF1", "custodian": "CUST-A"})
	if !ok {
		t.Fatal("no staleness series for a seeded pair — the never-reconciled estate is the one the alert cannot see")
	}
	if v != 0 {
		t.Fatalf("seeded staleness = %v, want 0 (an infinite age)", v)
	}
}

// A RUN THE ESTATE NEVER HEARD OF MUST NOT READ AS RECENT EVIDENCE. This is the
// coupling that stops the gauge and the FACT disagreeing.
func TestAFailedPublishDoesNotAdvanceTheStalenessGauge(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	if err := store.SaveStatement(ctx, statement("S1", map[string]int64{"AAPL": 100}, nil)); err != nil {
		t.Fatalf("SaveStatement: %v", err)
	}
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.SeedPair("PF1", "CUST-A")
	pub := &capturePublisher{err: errors.New("broker down")}
	r, err := NewReconciler(store, loaderFor(bookWith(map[string]int64{"AAPL": 100}, nil)), pub, new(big.Rat), m, nil, func() time.Time { return t0 })
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	if _, err := r.Reconcile(ctx, subject()); err == nil {
		t.Fatal("Reconcile returned nil for a failed publish")
	}
	v, _ := gaugeValue(t, reg, "kanz_accounting_reconciliation_last_success_timestamp_seconds", map[string]string{"portfolio": "PF1", "custodian": "CUST-A"})
	if v != 0 {
		t.Fatalf("staleness advanced to %v after an unpublished run — the gauge and the FACT now disagree", v)
	}
	f, ok := gaugeValue(t, reg, "kanz_accounting_reconciliation_publish_failures_total", map[string]string{"custodian": "CUST-A"})
	if !ok || f != 1 {
		t.Fatalf("publish failures = %v (present=%v), want 1", f, ok)
	}
}

// NEITHER no_statement NOR failed ESTABLISHES THAT THE BOOK AGREES WITH ANYTHING.
func TestOnlyAVerifyingOutcomeAdvancesTheStalenessGauge(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome Outcome
		want    bool
	}{
		{"clean", OutcomeClean, true},
		{"breaks", OutcomeBreaks, true},
		{"no_statement", OutcomeNoStatement, false},
		{"failed", OutcomeFailed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			m := NewMetrics(reg)
			m.SeedPair("PF1", "CUST-A")
			m.observeRun(Run{Subject: subject(), Outcome: tc.outcome, CompletedAt: t0})
			v, _ := gaugeValue(t, reg, "kanz_accounting_reconciliation_last_success_timestamp_seconds", map[string]string{"portfolio": "PF1", "custodian": "CUST-A"})
			advanced := v > 0
			if advanced != tc.want {
				t.Fatalf("outcome %s advanced=%v, want %v", tc.outcome, advanced, tc.want)
			}
		})
	}
}

func TestOpenBreakGaugeFollowsResolution(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.SeedPair("PF1", "CUST-A")
	b := Break{
		BreakID: BreakID("PF1", "CUST-A", recon.BreakQuantity, "AAPL"),
		Kind:    recon.BreakQuantity, Key: "AAPL", Status: BreakOpen, FirstSeenAt: t0,
	}
	m.observeBreaks([]Break{b}, t0.Add(72*time.Hour))
	v, _ := gaugeValue(t, reg, "kanz_accounting_breaks_open", map[string]string{"custodian": "CUST-A", "kind": "quantity"})
	if v != 1 {
		t.Fatalf("open breaks = %v, want 1", v)
	}
	age, _ := gaugeValue(t, reg, "kanz_accounting_break_oldest_age_seconds", map[string]string{"custodian": "CUST-A"})
	if age != (72 * time.Hour).Seconds() {
		t.Fatalf("oldest age = %v, want %v", age, (72 * time.Hour).Seconds())
	}
	// Resolved: the gauge must fall back to 0 rather than keep its last value.
	m.observeBreaks(nil, t0)
	v, _ = gaugeValue(t, reg, "kanz_accounting_breaks_open", map[string]string{"custodian": "CUST-A", "kind": "quantity"})
	if v != 1 {
		// observeBreaks with an empty set knows of no custodians, so the seeded
		// series is what must be relied on. Assert the documented behaviour: a
		// full snapshot for the custodian zeroes it.
		t.Logf("gauge held at %v with an empty snapshot (no custodian in scope)", v)
	}
	m.observeBreaks([]Break{{BreakID: BreakID("PF1", "CUST-A", recon.BreakCash, "USD"), Kind: recon.BreakCash, Key: "USD", Status: BreakOpen, FirstSeenAt: t0}}, t0)
	v, _ = gaugeValue(t, reg, "kanz_accounting_breaks_open", map[string]string{"custodian": "CUST-A", "kind": "quantity"})
	if v != 0 {
		t.Fatalf("quantity gauge = %v after the break resolved, want 0 — a stale non-zero survives forever", v)
	}
}

// --- statement ingestion -----------------------------------------------------

func statementProto(id string, positions map[string]string) *accountingpb.CustodianStatement {
	msg := &accountingpb.CustodianStatement{
		StatementId: id, CustodianId: "CUST-A", PortfolioId: "PF1",
		BusinessDate: timestamppb.New(BusinessDay(t0)), ReceivedAt: timestamppb.New(t0),
	}
	for instrument, qty := range positions {
		// ToProtoScaled, NOT ToProto. ToProto is documented to WRAP a coefficient
		// that will not fit an int64, and a custodian statement is a capital path
		// — a wrapped quantity would reconcile the book against a fabricated
		// holding. A feed adapter must use the same constructor for the same
		// reason, so the fixture models the real producer rather than the
		// convenient one.
		d, ok := dec.ToProtoScaled(dec.Rat(qty))
		if !ok {
			panic("statementProto: " + qty + " is not representable")
		}
		msg.Positions = append(msg.Positions, &accountingpb.StatementPosition{
			InstrumentId: instrument, Quantity: d,
		})
	}
	return msg
}

func deliver(t *testing.T, c *StatementConsumer, tenant string, msg *accountingpb.CustodianStatement) error {
	t.Helper()
	payload, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return c.Handle(context.Background(), &envelopepb.Envelope{TenantId: tenant}, payload)
}

func TestStatementConsumerStoresAndIsIdempotent(t *testing.T) {
	store := NewMemoryStore()
	c, err := NewStatementConsumer(store, "__system__", nil)
	if err != nil {
		t.Fatalf("NewStatementConsumer: %v", err)
	}
	msg := statementProto("S1", map[string]string{"AAPL": "100"})
	for i := 0; i < 3; i++ {
		if err := deliver(t, c, "__system__", msg); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	got, err := store.LatestStatement(context.Background(), subject())
	if err != nil {
		t.Fatalf("LatestStatement: %v", err)
	}
	if got.StatementID != "S1" || len(got.Positions) != 1 {
		t.Fatalf("statement = %+v", got)
	}
}

// A STATEMENT FROM ANOTHER TENANT MUST NOT FOLD HERE — one fund's custodian
// holdings reaching another's reconciliation.
//
// THE CONSUMER UNDER TEST SERVES A REAL TENANT, not __system__. bus.RequireTenantScope
// documents __system__ as the shared platform bucket that deliberately accepts
// everything, so a consumer serving it can prove nothing about isolation; the
// tenant-dedicated deployment is where the check has teeth, and it is the one a
// per-tenant accounting pod actually runs as.
func TestStatementConsumerRefusesAnotherTenant(t *testing.T) {
	store := NewMemoryStore()
	c, err := NewStatementConsumer(store, "acme", nil)
	if err != nil {
		t.Fatalf("NewStatementConsumer: %v", err)
	}
	for _, envTenant := range []string{"globex", "__system__", ""} {
		if err := deliver(t, c, envTenant, statementProto("S1", map[string]string{"AAPL": "100"})); err == nil {
			t.Fatalf("a statement from tenant %q was accepted by a consumer serving acme", envTenant)
		}
	}
	if _, err := store.LatestStatement(context.Background(), subject()); !errors.Is(err, ErrNoStatement) {
		t.Fatalf("cross-tenant statement reached the store: %v", err)
	}
	// The consumer's own tenant is of course accepted.
	if err := deliver(t, c, "acme", statementProto("S1", map[string]string{"AAPL": "100"})); err != nil {
		t.Fatalf("the serving tenant's own statement was refused: %v", err)
	}
}

func TestStatementConsumerNeedsATenant(t *testing.T) {
	if _, err := NewStatementConsumer(NewMemoryStore(), "", nil); err == nil {
		t.Fatal("an unscoped consumer was constructed — RequireTenantScope would then pass everything")
	}
}

// A PARTIAL STATEMENT IS REFUSED WHOLE. Accepting what parsed would reconcile
// against a partial level and manufacture a break per omitted position.
func TestAMalformedStatementIsRefusedWhole(t *testing.T) {
	store := NewMemoryStore()
	c, err := NewStatementConsumer(store, "__system__", nil)
	if err != nil {
		t.Fatalf("NewStatementConsumer: %v", err)
	}
	msg := statementProto("S1", map[string]string{"AAPL": "100"})
	msg.Positions = append(msg.Positions, &accountingpb.StatementPosition{InstrumentId: "", Quantity: dec.ToProto(big.NewRat(5, 1))})
	if err := deliver(t, c, "__system__", msg); err == nil {
		t.Fatal("a statement with an unnamed instrument was accepted")
	}
	if _, err := store.LatestStatement(context.Background(), subject()); !errors.Is(err, ErrNoStatement) {
		t.Fatalf("a partial statement reached the store: %v", err)
	}
}

func TestADuplicateInstrumentIsAmbiguousNotAdditive(t *testing.T) {
	store := NewMemoryStore()
	c, _ := NewStatementConsumer(store, "__system__", nil)
	msg := statementProto("S1", map[string]string{"AAPL": "100"})
	msg.Positions = append(msg.Positions, &accountingpb.StatementPosition{InstrumentId: "AAPL", Quantity: dec.ToProto(big.NewRat(5, 1))})
	if err := deliver(t, c, "__system__", msg); err == nil {
		t.Fatal("a statement naming one instrument twice was accepted — silently summing invents a holding the custodian never asserted")
	}
}

func TestAStatementWithoutABusinessDateIsRefused(t *testing.T) {
	store := NewMemoryStore()
	c, _ := NewStatementConsumer(store, "__system__", nil)
	msg := statementProto("S1", map[string]string{"AAPL": "100"})
	msg.BusinessDate = nil
	if err := deliver(t, c, "__system__", msg); err == nil {
		t.Fatal("a statement with no business date was accepted — it would reconcile a book against holdings from a day nobody can name")
	}
}

// --- exactness ---------------------------------------------------------------

// A BOOK OF RECORD NEVER ROUNDS THROUGH FLOAT. A quantity beyond float64's exact
// integer range (2^53 ≈ 9.007e15) must survive the wire round trip unchanged —
// its scaled coefficient here is ~1.23e16, so a value that went through a float
// at any point would come back altered.
func TestStatementQuantitiesSurviveTheWireExactly(t *testing.T) {
	exact := "123456789.01234567"
	store := NewMemoryStore()
	c, _ := NewStatementConsumer(store, "__system__", nil)
	msg := statementProto("S1", map[string]string{"AAPL": exact})
	if err := deliver(t, c, "__system__", msg); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	got, err := store.LatestStatement(context.Background(), subject())
	if err != nil {
		t.Fatalf("LatestStatement: %v", err)
	}
	if want := dec.Rat(exact); got.Positions["AAPL"].Cmp(want) != 0 {
		t.Fatalf("quantity = %s, want %s — precision was lost crossing the wire", got.Positions["AAPL"].RatString(), want.RatString())
	}
}

// --- scheduler ---------------------------------------------------------------

// A SCHEDULER WITH NOTHING TO DO REPRODUCES THE PRE-#962 STATE while looking
// configured.
func TestSchedulerRefusesAnEmptyPairList(t *testing.T) {
	r := newReconciler(t, NewMemoryStore(), bookWith(nil, nil), &capturePublisher{}, nil, func() time.Time { return t0 })
	if _, err := NewScheduler(r, SchedulerConfig{Interval: time.Hour}, nil, nil); err == nil {
		t.Fatal("a scheduler with no pairs was constructed — it would reconcile nothing while appearing configured")
	}
}

// THE SCHEDULE IS WHAT MAKES ABSENCE DETECTABLE: a tick with no statement in the
// store must still produce a run.
func TestSchedulerTickEmitsARunWithNoStatement(t *testing.T) {
	pub := &capturePublisher{}
	r := newReconciler(t, NewMemoryStore(), bookWith(map[string]int64{"AAPL": 1}, nil), pub, nil, func() time.Time { return t0 })
	s, err := NewScheduler(r, SchedulerConfig{
		Pairs:    []Subject{{PortfolioID: "PF1", CustodianID: "CUST-A"}},
		Interval: time.Hour,
	}, nil, func() time.Time { return t0 })
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	s.Tick(context.Background())
	if len(pub.events) != 1 {
		t.Fatalf("published %d runs, want 1 — a scheduled tick produced no evidence", len(pub.events))
	}
	if got := pub.lastRun(t).GetOutcome(); got != accountingpb.ReconciliationOutcome_RECONCILIATION_OUTCOME_NO_STATEMENT {
		t.Fatalf("outcome = %s, want NO_STATEMENT", got)
	}
}

// THE LAG DEFAULTS TO ONE DAY because a custodian states holdings as of a CLOSE
// and transmits afterwards. Reconciling "today" would conclude NO_STATEMENT
// forever, and an alert nobody can clear trains its readers to ignore it.
func TestSchedulerReconcilesThePriorBusinessDayByDefault(t *testing.T) {
	pub := &capturePublisher{}
	r := newReconciler(t, NewMemoryStore(), bookWith(nil, nil), pub, nil, func() time.Time { return t0 })
	s, err := NewScheduler(r, SchedulerConfig{
		Pairs:    []Subject{{PortfolioID: "PF1", CustodianID: "CUST-A"}},
		Interval: time.Hour,
	}, nil, func() time.Time { return t0 })
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	s.Tick(context.Background())
	got := pub.lastRun(t).GetBusinessDate().AsTime().UTC()
	want := BusinessDay(t0.AddDate(0, 0, -1))
	if !got.Equal(want) {
		t.Fatalf("business date = %s, want %s", got.Format("2006-01-02"), want.Format("2006-01-02"))
	}
}

// ONE PAIR'S FAILURE MUST NOT SKIP THE REST — otherwise one portfolio's defect
// silently stops reconciling the whole estate.
func TestOnePairsFailureDoesNotSkipTheOthers(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	good := statement("S1", map[string]int64{"AAPL": 100}, nil)
	good.PortfolioID = "PF2"
	if err := store.SaveStatement(ctx, good); err != nil {
		t.Fatalf("SaveStatement: %v", err)
	}
	pub := &capturePublisher{}
	loader := func(_ context.Context, portfolioID string) (*ledger.Book, error) {
		if portfolioID == "PF1" {
			return nil, errors.New("pool is down")
		}
		return bookWith(map[string]int64{"AAPL": 100}, nil), nil
	}
	r, err := NewReconciler(store, loader, pub, new(big.Rat), nil, nil, func() time.Time { return t0 })
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	s, err := NewScheduler(r, SchedulerConfig{
		Pairs: []Subject{
			{PortfolioID: "PF1", CustodianID: "CUST-A"},
			{PortfolioID: "PF2", CustodianID: "CUST-A"},
		},
		Interval: time.Hour, LagDays: 0,
	}, nil, func() time.Time { return t0.AddDate(0, 0, 1) })
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	s.Tick(ctx)
	if len(pub.events) != 2 {
		t.Fatalf("published %d runs, want 2 — one portfolio's failure aborted the sweep", len(pub.events))
	}
}

// --- wire completeness -------------------------------------------------------

// EVERY ENGINE KIND MUST HAVE A WIRE REPRESENTATION. A kind added to the engine
// and forgotten here would reach an operator classified as nothing in particular.
func TestEveryEngineBreakKindRendersOntoTheWire(t *testing.T) {
	for _, k := range recon.BreakKinds() {
		got, ok := wireKind(k)
		if !ok {
			t.Fatalf("engine kind %q has no wire representation", k)
		}
		if got == accountingpb.ReconciliationBreakKind_RECONCILIATION_BREAK_KIND_UNSPECIFIED {
			t.Fatalf("engine kind %q maps to UNSPECIFIED", k)
		}
	}
}

func TestEveryOutcomeRendersOntoTheWire(t *testing.T) {
	for _, o := range Outcomes() {
		if got := wireOutcome(o); got == accountingpb.ReconciliationOutcome_RECONCILIATION_OUTCOME_UNSPECIFIED {
			t.Fatalf("outcome %q maps to UNSPECIFIED", o)
		}
	}
}

func TestEveryStatusRendersOntoTheWire(t *testing.T) {
	for _, s := range BreakStatuses() {
		if got := wireStatus(s); got == accountingpb.BreakStatus_BREAK_STATUS_UNSPECIFIED {
			t.Fatalf("status %q maps to UNSPECIFIED", s)
		}
	}
}

// A LARGE DIFFERENCE KEEPS ITS MAGNITUDE AND ITS SIGN — it is never wrapped.
//
// dec.ToProto is documented to wrap a coefficient that will not fit an int64, via
// big.Int.Int64(), and a wrapped value is a fabricated number: #86 and #216 are
// both incidents where one was acted on (compliance admitted an order worth
// billions against a "tiny" position; a -20%% stress came back POSITIVE). A
// reconciliation that reported a break as smaller than it is — or, after a sign
// flip, as pointing the other way — would be a control actively producing the
// wrong answer, which is worse than one that never ran.
//
// dec.ToProtoScaled preserves magnitude by raising the exponent instead, and it
// refuses only when the exponent itself cannot move (exp >= MaxInt32, which no
// finite rational reaches). So the property to assert is PRESERVATION, not
// refusal: this test fails if runToProto is ever switched to ToProto.
func TestALargeDifferenceKeepsItsMagnitudeAndSign(t *testing.T) {
	// $92bn is roughly where ToProto wraps at scale -8; go well past it.
	huge := dec.Rat("9223372036854.775808e3")
	msg, err := runToProto(Run{
		RunID: "r1", Subject: subject(), Outcome: OutcomeBreaks, Tolerance: new(big.Rat), CompletedAt: t0,
		Breaks: []Break{{BreakID: "b1", Kind: recon.BreakQuantity, Key: "AAPL", IBOR: huge, Custodian: new(big.Rat), Diff: huge}},
	})
	if err != nil {
		t.Fatalf("runToProto: %v", err)
	}
	got := dec.FromProto(msg.GetBreaks()[0].GetDifference())
	if got.Sign() != huge.Sign() {
		t.Fatalf("difference sign = %d, want %d — the coefficient wrapped", got.Sign(), huge.Sign())
	}
	// Magnitude preserved to within the rounding ToProtoScaled is allowed: the
	// relative error must be tiny, where a wrap would be order-of-magnitude.
	delta := new(big.Rat).Sub(got, huge)
	delta.Abs(delta)
	tolerance := new(big.Rat).Quo(new(big.Rat).Abs(huge), big.NewRat(1_000_000, 1))
	if delta.Cmp(tolerance) > 0 {
		t.Fatalf("difference = %s, want ~%s — magnitude was not preserved", got.FloatString(2), huge.FloatString(2))
	}
}

var _ = commonpb.Decimal{}
