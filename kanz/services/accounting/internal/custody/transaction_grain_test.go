package custody

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/recon"
)

// THE TRANSACTION GRAIN, THROUGH THE WHOLE CONTROL (#1049).
//
// recon proves the comparison. These prove that an execution break reaches the
// SAME lifecycle every other break does — a stable cross-run id, an age, an
// assignee, the gauges and the wire — rather than a parallel answer beside it.

// tradeExecution is one book-side execution settled on the run's business date.
func tradeExecution(ref, instrument, qty string) ledger.Execution {
	return ledger.Execution{
		Ref: ref, EntryID: "fill:" + ref, InstrumentID: instrument,
		Quantity: dec.Rat(qty), Price: dec.Rat("1"), Cash: new(big.Rat), CashCurrency: "USD",
		VenueAccountID: "okx-sub-1",
		TradeDate:      t0, SettlementBasis: ledger.SettlementSettled, SettlementDate: t0,
	}
}

// tradeLine is one custodian-side trade line for the same date.
func tradeLine(ref, instrument, qty string) recon.Transaction {
	return recon.Transaction{
		ExternalRef: ref, InstrumentID: instrument, Quantity: dec.Rat(qty),
		TradeDate: t0, SettlementDate: t0, CurrencyCode: "USD",
	}
}

// TestTransactionReconBreakEntersTheSameLifecycle: a run over a statement that
// declares trade lines produces execution breaks with stable ids, an age and an
// operator surface — the existing lifecycle, not a second one.
func TestTransactionReconBreakEntersTheSameLifecycle(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	// The netted totals AGREE: the book holds +100 AAPL across two fills, and the
	// custodian reports +100 across two trade lines — but one of its lines is an
	// execution the book has never heard of, and one of the book's is an execution
	// it never settled.
	stmt := statement("S-tx", map[string]int64{"AAPL": 100}, nil)
	stmt.Grain = recon.GrainTransactions
	stmt.Transactions = []recon.Transaction{
		tradeLine("fill-a", "AAPL", "60"),
		tradeLine("venue-x9", "AAPL", "40"),
	}
	if err := store.SaveStatement(ctx, stmt); err != nil {
		t.Fatalf("SaveStatement: %v", err)
	}

	pub := &capturePublisher{}
	r, err := NewReconciler(store,
		loaderWith(bookWith(map[string]int64{"AAPL": 100}, nil), []ledger.Execution{
			tradeExecution("fill-a", "AAPL", "60"),
			tradeExecution("fill-b", "AAPL", "40"),
		}),
		pub, new(big.Rat), nil, nil, func() time.Time { return t0 })
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	run, err := r.Reconcile(ctx, subject())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if run.Outcome != OutcomeBreaks {
		t.Fatalf("outcome = %s, want %s. The netted totals agree exactly, so a netted-only control "+
			"concludes CLEAN over a book holding an execution the custodian never settled",
			run.Outcome, OutcomeBreaks)
	}

	byID := map[string]Break{}
	for _, b := range run.Breaks {
		byID[b.BreakID] = b
	}
	wantAtCustodian := BreakID("PF1", "CUST-A", recon.BreakExecutionMissingAtCustodian, "fill-b")
	wantInIBOR := BreakID("PF1", "CUST-A", recon.BreakExecutionMissingInIBOR, "venue-x9")
	for _, id := range []string{wantAtCustodian, wantInIBOR} {
		b, ok := byID[id]
		if !ok {
			t.Fatalf("the run carries no break %q. Got %v.\n\nAn execution break must go into the SAME "+
				"lifecycle every other break does: a break nobody can age is a break nobody chases.",
				id, run.Breaks)
		}
		if b.Status != BreakOpen {
			t.Errorf("%s status = %s, want %s", id, b.Status, BreakOpen)
		}
		if b.FirstSeenAt.IsZero() {
			t.Errorf("%s has no FirstSeenAt — the age an operator triages by is unmeasurable", id)
		}
	}

	// AND IT REACHES THE WIRE. A kind the engine can produce and the wire cannot
	// carry refuses the publish of the ENTIRE run, which loses the evidence for
	// the breaks that did map.
	msg := pub.lastRun(t)
	kinds := map[accountingpb.ReconciliationBreakKind]bool{}
	for _, b := range msg.GetBreaks() {
		kinds[b.GetKind()] = true
	}
	for _, want := range []accountingpb.ReconciliationBreakKind{
		accountingpb.ReconciliationBreakKind_RECONCILIATION_BREAK_KIND_EXECUTION_MISSING_AT_CUSTODIAN,
		accountingpb.ReconciliationBreakKind_RECONCILIATION_BREAK_KIND_EXECUTION_MISSING_IN_IBOR,
	} {
		if !kinds[want] {
			t.Errorf("the published run carries no %s break — the engine produced one and the wire did not", want)
		}
	}

	// The operator surface: an execution break is assignable and explainable like
	// any other, and Explain keeps it OUTSTANDING so it goes on ageing.
	b, err := store.LoadBreak(ctx, wantInIBOR)
	if err != nil {
		t.Fatalf("LoadBreak: %v", err)
	}
	if err := b.Assign("ops-desk", t0); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if !b.Status.Outstanding() {
		t.Error("an assigned execution break left the outstanding set — it would stop ageing and stop paging")
	}
}

// TestTransactionReconRunIsCleanUnderABalancesOnlyFeed: the leg must not run, and
// the netted verdict must still be a real one.
func TestTransactionReconRunIsCleanUnderABalancesOnlyFeed(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	stmt := statement("S-bal", map[string]int64{"AAPL": 100}, nil)
	stmt.Grain = recon.GrainBalancesOnly
	if err := store.SaveStatement(ctx, stmt); err != nil {
		t.Fatalf("SaveStatement: %v", err)
	}
	r, err := NewReconciler(store,
		loaderWith(bookWith(map[string]int64{"AAPL": 100}, nil), []ledger.Execution{
			tradeExecution("fill-a", "AAPL", "60"),
			tradeExecution("fill-b", "AAPL", "40"),
		}),
		&capturePublisher{}, new(big.Rat), nil, nil, func() time.Time { return t0 })
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	run, err := r.Reconcile(ctx, subject())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if run.Outcome != OutcomeClean {
		t.Fatalf("outcome = %s, want %s. A custodian that states it sends no trade lines must not have "+
			"every execution in the book reported against it: an empty list is not a claim that "+
			"nothing traded", run.Outcome, OutcomeClean)
	}
}

// TestStatementRefusesTransactionsWithoutTheGrain: a producer that sends trade
// lines while asserting BALANCES_ONLY (or nothing) would have every one of them
// silently ignored. A feed that looks wired and a control that compares nothing
// must not be the same observable state.
func TestStatementRefusesTransactionsWithoutTheGrain(t *testing.T) {
	for _, grain := range []recon.Grain{recon.GrainUnknown, recon.GrainBalancesOnly} {
		s := statement("S1", map[string]int64{"AAPL": 100}, nil)
		s.Grain = grain
		s.Transactions = []recon.Transaction{tradeLine("fill-a", "AAPL", "60")}
		err := s.Validate()
		if err == nil {
			t.Fatalf("a statement carrying trade lines at grain %s validated.\n\nThe transaction leg "+
				"runs only on %s, so those lines would be dropped with nothing saying so — a feed that "+
				"looks wired over a control that compares nothing.", grain, recon.GrainTransactions)
		}
		if !strings.Contains(err.Error(), "would be ignored") {
			t.Errorf("refusal for grain %s does not say the lines would be ignored: %v", grain, err)
		}
	}
}

// TestStatementRefusesAnUnusableTransactionReference: the reference is the whole
// of the match AND part of a derived break id.
func TestStatementRefusesAnUnusableTransactionReference(t *testing.T) {
	for _, tc := range []struct {
		name string
		txs  []recon.Transaction
		want string
	}{
		{"no reference", []recon.Transaction{{InstrumentID: "AAPL"}}, "no external_ref"},
		{"a duplicate reference", []recon.Transaction{tradeLine("f1", "AAPL", "1"), tradeLine("f1", "AAPL", "2")}, "twice"},
		{"the id separator", []recon.Transaction{tradeLine("f1"+IDSeparator+"x", "AAPL", "1")}, "id separator"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := statement("S1", map[string]int64{"AAPL": 100}, nil)
			s.Grain = recon.GrainTransactions
			s.Transactions = tc.txs
			err := s.Validate()
			if err == nil {
				t.Fatalf("a statement with %s validated — the reference is the whole of the match and "+
					"part of the break's derived id", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal does not mention %q: %v", tc.want, err)
			}
		})
	}
}

// TestStatementConsumerCarriesTheGrainOffTheWire: the decode must preserve the
// third value. A wire grain that arrived as "unknown" would leave a fully wired
// trade-level feed reconciling at the netted grain, permanently and quietly.
func TestStatementConsumerCarriesTheGrainOffTheWire(t *testing.T) {
	store := NewMemoryStore()
	c, err := NewStatementConsumer(store, "acme", nil)
	if err != nil {
		t.Fatalf("NewStatementConsumer: %v", err)
	}
	qty, ok := dec.ToProtoScaled(dec.Rat("60"))
	if !ok {
		t.Fatal("60 is not representable")
	}
	msg := statementProto("S-wire", map[string]string{"AAPL": "60"})
	msg.Grain = accountingpb.StatementGrain_STATEMENT_GRAIN_TRANSACTIONS
	msg.Transactions = []*accountingpb.StatementTransaction{{
		ExternalRef:    "fill-a",
		InstrumentId:   "AAPL",
		Quantity:       qty,
		TradeDate:      timestamppb.New(t0),
		SettlementDate: timestamppb.New(t0),
		CurrencyCode:   "USD",
	}}
	if err := deliver(t, c, "acme", msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	stored, err := store.LatestStatement(context.Background(), subject())
	if err != nil {
		t.Fatalf("LatestStatement: %v", err)
	}
	if stored.Grain != recon.GrainTransactions {
		t.Fatalf("stored grain = %s, want %s. A trade-level feed decoded as %s reconciles at the netted "+
			"grain forever, and nothing in the run distinguishes it from a custodian that sends balances",
			stored.Grain, recon.GrainTransactions, recon.GrainUnknown)
	}
	if len(stored.Transactions) != 1 || stored.Transactions[0].ExternalRef != "fill-a" {
		t.Fatalf("stored transactions = %v, want one line for fill-a", stored.Transactions)
	}
	if got := stored.Transactions[0].Quantity; got == nil || got.Cmp(dec.Rat("60")) != 0 {
		t.Errorf("decoded quantity = %v, want 60", got)
	}
}

// TestWireGrainRoundTripsEveryGrain: recon.Grains() is the source of truth, and
// wireGrain/grainFromWire are the single join. A grain that does not survive the
// round trip arrives as UNKNOWN, which switches the transaction pass off with
// nothing saying so.
func TestWireGrainRoundTripsEveryGrain(t *testing.T) {
	for _, g := range recon.Grains() {
		if got := grainFromWire(wireGrain(g)); got != g {
			t.Errorf("grain %s round-tripped to %s — the join between the engine and the wire has drifted, "+
				"and an unmapped grain silently disables the execution comparison", g, got)
		}
	}
}
