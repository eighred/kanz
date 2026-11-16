package custody

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/recon"
	"google.golang.org/protobuf/proto"
)

type presenceStore struct {
	*MemoryStore
	saves int
}

func (s *presenceStore) SaveStatement(ctx context.Context, statement Statement) error {
	s.saves++
	return s.MemoryStore.SaveStatement(ctx, statement)
}

func presenceStatement(id string) *accountingpb.CustodianStatement {
	msg := statementProto(id, nil)
	msg.Positions = []*accountingpb.StatementPosition{{InstrumentId: "AAPL", Quantity: &commonpb.Decimal{}}}
	msg.Cash = []*accountingpb.StatementCash{{CurrencyCode: "USD", Balance: &commonpb.Decimal{}}}
	msg.Grain = accountingpb.StatementGrain_STATEMENT_GRAIN_TRANSACTIONS
	return msg
}

func TestStatementPresenceRefusesMissingRequiredAmountsBeforeSaving(t *testing.T) {
	for _, field := range []string{"position quantity", "cash balance"} {
		t.Run(field, func(t *testing.T) {
			store := &presenceStore{MemoryStore: NewMemoryStore()}
			consumer, err := NewStatementConsumer(store, "acme", testLogger())
			if err != nil {
				t.Fatal(err)
			}
			msg := presenceStatement("missing")
			if field == "position quantity" {
				msg.Positions[0].Quantity = nil
			} else {
				msg.Cash[0].Balance = nil
			}
			if err := deliver(t, consumer, "acme", msg); err == nil {
				t.Fatal("missing required amount was accepted as a measured zero")
			}
			if store.saves != 0 {
				t.Fatalf("invalid statement reached SaveStatement %d times", store.saves)
			}
		})
	}
}

func TestStatementPresencePreservesOptionalFiguresAndReferenceBreak(t *testing.T) {
	msg := presenceStatement("optional")
	msg.Transactions = []*accountingpb.StatementTransaction{{ExternalRef: "unavailable"}}
	store := NewMemoryStore()
	consumer, err := NewStatementConsumer(store, "acme", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := deliver(t, consumer, "acme", msg); err != nil {
		t.Fatal(err)
	}
	statement, err := store.LatestStatement(t.Context(), subject())
	if err != nil {
		t.Fatal(err)
	}
	line := statement.Transactions[0]
	if line.Quantity != nil || line.Price != nil || line.Cash != nil {
		t.Fatalf("unreported transaction figures became measured values: %+v", line)
	}
	// An absent amount must not delete the independent reference discrepancy.
	breaks, leg, err := recon.Reconcile(bookWith(nil, nil), nil, recon.Statement{
		PortfolioID: statement.PortfolioID, Positions: statement.Positions, Cash: statement.Cash,
		BusinessDate: statement.BusinessDate, Transactions: statement.Transactions, Grain: statement.Grain,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !leg.Ran() || len(breaks) != 1 || breaks[0].Kind != recon.BreakExecutionMissingInIBOR || breaks[0].Key != "unavailable" {
		t.Fatalf("unavailable transaction lost its reference discrepancy: %s %+v", leg, breaks)
	}
	if breaks[0].Custodian != nil || breaks[0].Diff != nil {
		t.Fatalf("unknown execution quantity was rendered as a measured difference: %+v", breaks[0])
	}
}

func TestStatementPresencePreservesReportedZeroAndExactDecimalFigures(t *testing.T) {
	msg := presenceStatement("reported")
	// This coefficient is below MaxInt64; its final 1e-18 unit must survive.
	exact := &commonpb.Decimal{Coefficient: 1000000000000000001, Exponent: -18}
	msg.Positions = append(msg.Positions, &accountingpb.StatementPosition{InstrumentId: "EXACT", Quantity: exact})
	msg.Cash = append(msg.Cash, &accountingpb.StatementCash{CurrencyCode: "EUR", Balance: exact})
	msg.Transactions = []*accountingpb.StatementTransaction{
		{ExternalRef: "zero", Quantity: &commonpb.Decimal{}, Price: &commonpb.Decimal{}, Cash: &commonpb.Decimal{}},
		{ExternalRef: "exact", Quantity: exact, Price: exact, Cash: exact},
	}
	statement, err := statementFromProto(msg)
	if err != nil {
		t.Fatal(err)
	}
	for _, amount := range []*big.Rat{statement.Positions["AAPL"], statement.Cash["USD"], statement.Transactions[0].Quantity, statement.Transactions[0].Price, statement.Transactions[0].Cash} {
		if amount == nil || amount.Sign() != 0 {
			t.Fatalf("reported zero was unavailable or changed: %v", amount)
		}
	}
	want := new(big.Rat).SetFrac(big.NewInt(1000000000000000001), big.NewInt(1000000000000000000))
	for _, amount := range []*big.Rat{statement.Positions["EXACT"], statement.Cash["EUR"], statement.Transactions[1].Quantity, statement.Transactions[1].Price, statement.Transactions[1].Cash} {
		if amount == nil || amount.Cmp(want) != 0 {
			t.Fatalf("exact 1e-18 figure changed: got %v want %s", amount, want.RatString())
		}
	}
}

func TestStatementPresenceRefusesInvalidSuppliedOptionalFigures(t *testing.T) {
	for _, field := range []string{"quantity", "price", "cash"} {
		t.Run(field, func(t *testing.T) {
			msg := presenceStatement("invalid")
			line := &accountingpb.StatementTransaction{ExternalRef: "invalid"}
			invalid := &commonpb.Decimal{Coefficient: 1, Exponent: 65}
			switch field {
			case "quantity":
				line.Quantity = invalid
			case "price":
				line.Price = invalid
			case "cash":
				line.Cash = invalid
			}
			msg.Transactions = []*accountingpb.StatementTransaction{line}
			if _, err := statementFromProto(msg); err == nil {
				t.Fatal("invalid supplied figure was treated as unavailable")
			}
		})
	}
}

func TestStatementPresenceCompletionPreservesUnavailableAndKnownValues(t *testing.T) {
	tiny := new(big.Rat).SetFrac(big.NewInt(1), big.NewInt(1000000000000000000))
	for _, tc := range []struct {
		name                        string
		kind                        recon.BreakKind
		ibor, custodian, difference *big.Rat
	}{
		{"book reference only", recon.BreakExecutionMissingAtCustodian, nil, new(big.Rat), nil},
		{"custodian reference only", recon.BreakExecutionMissingInIBOR, new(big.Rat), nil, nil},
		{"reported zero", recon.BreakExecutionMissingInIBOR, new(big.Rat), new(big.Rat), new(big.Rat)},
		{"exact 1e-18", recon.BreakExecutionMissingInIBOR, new(big.Rat), tiny, new(big.Rat).Neg(tiny)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := Break{BreakID: "reference", Kind: tc.kind, Key: "reference", IBOR: tc.ibor, Custodian: tc.custodian, Diff: tc.difference, ValuesVerified: true, Status: BreakOpen}
			msg, err := runToProto(Run{RunID: "presence", Subject: subject(), Outcome: OutcomeBreaks, CompletedAt: t0, Breaks: []Break{item}})
			if err != nil {
				t.Fatal(err)
			}
			row := msg.GetBreaks()[0]
			for i, value := range []*commonpb.Decimal{row.GetIbor(), row.GetCustodian(), row.GetDifference()} {
				want := []*big.Rat{tc.ibor, tc.custodian, tc.difference}[i]
				if (value == nil) != (want == nil) {
					t.Fatalf("FACT presence changed at figure %d: %v vs %v", i, value, want)
				}
			}
			payload, err := proto.Marshal(msg)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := runFromRecord(payload)
			if err != nil || len(restored.Breaks) != 1 || restored.Breaks[0].Key != "reference" || !restored.Breaks[0].ValuesVerified {
				t.Fatalf("immutable FACT retry lost the discrepancy: %+v %v", restored, err)
			}
			for i, value := range []*big.Rat{restored.Breaks[0].IBOR, restored.Breaks[0].Custodian, restored.Breaks[0].Diff} {
				want := []*big.Rat{tc.ibor, tc.custodian, tc.difference}[i]
				if (value == nil) != (want == nil) || (value != nil && value.Cmp(want) != 0) {
					t.Fatalf("retry changed figure %d: %v vs %v", i, value, want)
				}
			}
		})
	}
}

func TestStatementPresenceCompletionRetainsWireRefusals(t *testing.T) {
	wide, ok := new(big.Rat).SetString("123456789.000000000000000001")
	if !ok {
		t.Fatal("invalid independent exact fixture")
	}
	for _, item := range []Break{
		{Kind: recon.BreakExecutionMissingInIBOR, IBOR: new(big.Rat)}, // legacy figures are not newly verified absence
		{Kind: recon.BreakCash, ValuesVerified: true, IBOR: new(big.Rat)},
		{Kind: recon.BreakExecutionMissingInIBOR, ValuesVerified: true, IBOR: new(big.Rat), Custodian: wide, Diff: new(big.Rat).Neg(wide)},
		{Kind: recon.BreakExecutionMissingInIBOR, ValuesVerified: true, IBOR: new(big.Rat), Custodian: big.NewRat(1, 3), Diff: big.NewRat(-1, 3)},
	} {
		if _, err := runToProto(Run{RunID: "refused", Subject: subject(), Breaks: []Break{item}}); err == nil {
			t.Fatalf("legacy, required missing or unrepresentable figures were published: %+v", item)
		}
	}
}

func TestStatementPresenceCompletionRejectsInvalidSuppliedRetryAmount(t *testing.T) {
	msg := &accountingpb.ReconciliationRun{RunId: "invalid", Outcome: accountingpb.ReconciliationOutcome_RECONCILIATION_OUTCOME_BREAKS,
		Breaks: []*accountingpb.ReconciliationBreak{{Kind: accountingpb.ReconciliationBreakKind_RECONCILIATION_BREAK_KIND_EXECUTION_MISSING_IN_IBOR,
			Status: accountingpb.BreakStatus_BREAK_STATUS_OPEN, Custodian: &commonpb.Decimal{Coefficient: 1, Exponent: 65}}}}
	payload, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runFromRecord(payload); !errors.Is(err, ErrUnverifiedPrecision) {
		t.Fatalf("supplied invalid retry figure became unavailable: %v", err)
	}
}

func TestStatementPresenceMemoryRunCommitsReferenceWithoutAmount(t *testing.T) {
	store := NewMemoryStore()
	consumer, err := NewStatementConsumer(store, "acme", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	msg := presenceStatement("reference-only")
	msg.Transactions = []*accountingpb.StatementTransaction{{ExternalRef: "unavailable"}}
	if err := deliver(t, consumer, "acme", msg); err != nil {
		t.Fatal(err)
	}
	r, err := NewReconciler(store, loaderFor(bookWith(nil, nil)), nil, nil, nil, testLogger(), func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	ctx := bus.WithTenantID(t.Context(), "acme")
	run, err := r.Reconcile(ctx, subject())
	if !errors.Is(err, ErrPublicationPending) || run.Outcome != OutcomeBreaks || len(run.Breaks) != 1 {
		t.Fatalf("unavailable reference did not commit: %+v %v", run, err)
	}
	if item := run.Breaks[0]; item.Custodian != nil || item.Diff != nil || !item.ValuesVerified {
		t.Fatalf("committed reference claims a measured amount: %+v", item)
	}
	pending, err := store.Outbox().Pending(ctx, pairKey("PF1", "CUST-A"), 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("reference completion was not captured: %+v %v", pending, err)
	}
	restored, err := runFromRecord(pending[0].Record.Payload)
	if err != nil || len(restored.Breaks) != 1 || restored.Breaks[0].Custodian != nil || restored.Breaks[0].Diff != nil {
		t.Fatalf("captured reference changed unavailable figures: %+v %v", restored, err)
	}
}
