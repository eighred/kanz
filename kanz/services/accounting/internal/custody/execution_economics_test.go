package custody

import (
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/recon"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestDeclaredExecutionEconomics(t *testing.T) {
	day := time.Date(2020, 2, 4, 0, 0, 0, 0, time.UTC)
	sale := recon.Transaction{
		ExternalRef: "sale", InstrumentID: "SYN", QuantityInInstrumentUnits: true,
		Quantity: big.NewRat(-21, 2), Price: big.NewRat(5, 4), Commission: big.NewRat(1, 2),
		Cash: big.NewRat(101, 8), CurrencyCode: "USD", TradeDate: day, SettlementDate: day,
		EconomicsConvention: executionEconomicsConvention,
	}
	source, reason := signedEconomics(sale, day)
	if reason != "" || source.gross.Cmp(big.NewRat(105, 8)) != 0 {
		t.Fatalf("declared sale: %s %v", reason, source)
	}
	// The canonical posting recorded a lower price and lower fee than the source.
	book := ledger.ExecutionEconomics{
		Quantity: big.NewRat(-21, 2), Price: big.NewRat(1, 1), Commission: big.NewRat(1, 4),
		GrossCash: big.NewRat(21, 2), NetCash: big.NewRat(41, 4), SettledCash: big.NewRat(41, 4),
		SettlementBasis: ledger.SettlementSettled, TradeDate: day, SettlementDate: day,
		Venue: "XSIM", VenueExecutionID: "7", CashCurrency: "USD", CommissionCurrency: "USD",
	}
	row := &accountingpb.ExecutionEconomicsRow{Status: "unknown"}
	assessBookedEconomics(row, sale, "journal-sale", "XSIM", &book, source)
	if row.Status != "mismatch" || row.CustodianSettlementDate != "2020-02-04" || row.BookSettlementDate != row.CustodianSettlementDate || row.EntryId != "journal-sale" {
		t.Fatal(row)
	}
	run := Run{Subject: Subject{PortfolioID: "P", CustodianID: "C", BusinessDate: day}, CompletedAt: day.Add(24 * time.Hour), RunID: "R", Outcome: OutcomeClean, ExecutionEconomics: &accountingpb.ExecutionEconomicsComparison{Rows: []*accountingpb.ExecutionEconomicsRow{row}}}
	record, err := runRecord(t.Context(), "tenant", run)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := runFromRecord(record.Payload)
	if err != nil || !proto.Equal(saved.ExecutionEconomics, run.ExecutionEconomics) {
		t.Fatal("completion lost execution economics", err)
	}
	run.ExecutionEconomics = nil
	record, err = runRecord(t.Context(), "tenant", run)
	if err != nil {
		t.Fatal(err)
	}
	saved, err = runFromRecord(record.Payload)
	if err != nil || saved.ExecutionEconomics != nil {
		t.Fatal("legacy completion acquired economics", err)
	}
	d := row.Difference
	for _, v := range []struct {
		got  *commonpb.Decimal
		want *big.Rat
	}{
		{d.Quantity, big.NewRat(0, 1)}, {d.Price, big.NewRat(-1, 4)}, {d.Commission, big.NewRat(-1, 4)}, {d.GrossCash, big.NewRat(-21, 8)}, {d.NetCash, big.NewRat(-19, 8)}, {d.SettledCash, big.NewRat(-19, 8)},
	} {
		if v.got == nil || dec.FromProto(v.got).Cmp(v.want) != 0 {
			t.Fatalf("delta %v want %s", v.got, v.want)
		}
	}
	for _, tc := range []struct {
		name, reason string
		change       func(*recon.Transaction)
	}{
		{"absent", "convention_missing", func(s *recon.Transaction) { s.EconomicsConvention = "" }},
		{"other", "convention_unsupported", func(s *recon.Transaction) { s.EconomicsConvention = "other" }},
		{"cash", "source_cash_inconsistent", func(s *recon.Transaction) { s.Cash = new(big.Rat) }},
		{"units", "source_terms_unverified", func(s *recon.Transaction) { s.QuantityInInstrumentUnits = false }},
		{"fee absent", "source_terms_unverified", func(s *recon.Transaction) { s.Commission = nil }},
		{"rebate", "source_terms_unverified", func(s *recon.Transaction) { s.Commission = big.NewRat(-1, 2) }},
		{"settles tomorrow", "source_terms_unverified", func(s *recon.Transaction) { s.SettlementDate = day.Add(24 * time.Hour) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := sale
			tc.change(&copy)
			if _, r := signedEconomics(copy, day); r != tc.reason {
				t.Fatal(r)
			}
		})
	}
	for _, tc := range []struct {
		reason string
		change func(*ledger.ExecutionEconomics)
	}{
		{"currency_mismatch", func(b *ledger.ExecutionEconomics) { b.CommissionCurrency = "BNB" }},
		{"book_terms_unverified", func(b *ledger.ExecutionEconomics) { b.SettlementBasis = ledger.SettlementPending }},
		{"book_terms_unverified", func(b *ledger.ExecutionEconomics) { b.Venue = "foreign" }},
	} {
		copy := book
		tc.change(&copy)
		r := &accountingpb.ExecutionEconomicsRow{Status: "unknown"}
		assessBookedEconomics(r, sale, "journal-sale", "XSIM", &copy, source)
		if r.Status != "unknown" || r.Reason != tc.reason || r.Book != nil || r.Difference != nil {
			t.Fatal(r)
		}
	}
}

func TestExecutionEconomicsIntakeAndBounds(t *testing.T) {
	at := timestamppb.Now()
	msg := &accountingpb.CustodianStatement{StatementId: "S", PortfolioId: "P", CustodianId: "C", BusinessDate: at, ReceivedAt: at, Grain: accountingpb.StatementGrain_STATEMENT_GRAIN_TRANSACTIONS,
		Transactions: []*accountingpb.StatementTransaction{{ExternalRef: "X", ExecutionEconomics: &accountingpb.StatementExecutionEconomics{Convention: executionEconomicsConvention, Commission: &commonpb.Decimal{Coefficient: 0}}}}}
	stmt, err := statementFromProto(msg)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := transactionsToJSON(stmt.Transactions)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := jsonToTransactions(payload)
	if err != nil || !slices.EqualFunc(restored, stmt.Transactions, equalTransaction) || restored[0].Commission == nil || restored[0].Commission.Sign() != 0 {
		t.Fatalf("%v %v", restored, err)
	}
	restored[0].Commission = nil
	if equalTransaction(restored[0], stmt.Transactions[0]) {
		t.Fatal("missing commission became zero")
	}
	restored[0] = stmt.Transactions[0]
	restored[0].EconomicsConvention = "aged_contract"
	if equalTransaction(restored[0], stmt.Transactions[0]) {
		t.Fatal("changed convention became equal")
	}
	run := Run{Outcome: OutcomeClean, Basis: &RunBasis{Mode: BasisDeclared, Transactions: &TransactionComparison{Pass: "ran"}, Accounts: map[string][]string{"C": {"ACC"}}, Assignments: []AccountAssignment{{CustodianID: "C", AccountID: "ACC", Venue: "XSIM"}}}}
	stmt.Transactions = make([]recon.Transaction, maxEconomicsRows+1)
	result, err := compareExecutionEconomics(t.Context(), nil, run, stmt)
	if err != nil || result.GetUnassessedReason() != "row_limit" || len(result.Rows) != 0 {
		t.Fatalf("%v %v", result, err)
	}
	stmt.Transactions = []recon.Transaction{{ExternalRef: "z"}, {ExternalRef: "a"}}
	result, err = compareExecutionEconomics(t.Context(), nil, run, stmt)
	if err != nil || len(result.Rows) != 2 || result.Rows[0].ExternalRef != "a" || result.Rows[0].Reason != "convention_missing" || result.Rows[1].Reason != "convention_missing" {
		t.Fatal(result, err)
	}
	run.Basis.Assignments = nil
	result, err = compareExecutionEconomics(t.Context(), nil, run, stmt)
	if err != nil || result != nil {
		t.Fatal("unproven account was assessed", result, err)
	}
}
