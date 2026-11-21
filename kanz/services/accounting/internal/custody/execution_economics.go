package custody

import (
	"context"
	"math/big"
	"sort"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/recon"
	"github.com/jackc/pgx/v5"
)

const executionEconomicsConvention = "unit_price_net_cash_same_currency_commission_v1"
const maxEconomicsRows = 128

// compareExecutionEconomics is called only after authenticating the stored
// statement and dated assignment. The measured entry IDs identify the original
// transaction-grain book snapshot. Their values are never economic inputs.
func compareExecutionEconomics(ctx context.Context, tx pgx.Tx, run Run, stmt Statement) (*accountingpb.ExecutionEconomicsComparison, error) {
	basis := run.Basis
	if basis == nil || basis.Mode != BasisDeclared || basis.Transactions == nil || basis.Transactions.Pass != recon.LegRan.String() || stmt.Grain != recon.GrainTransactions || !run.Outcome.IsFullComparison() {
		return nil, nil
	}
	accounts := basis.Accounts[stmt.CustodianID]
	bindings := assignmentsForCustodian(basis.Assignments, stmt.CustodianID)
	if len(accounts) != 1 || len(bindings) != 1 || bindings[0].AccountID != accounts[0] {
		return nil, nil
	}
	if len(stmt.Transactions) > maxEconomicsRows {
		return &accountingpb.ExecutionEconomicsComparison{UnassessedReason: "row_limit"}, nil
	}
	entries := make(map[string]string)
	for entry, ref := range basis.MeasuredEntries {
		if _, exists := entries[ref]; exists {
			entries[ref] = ""
		} else {
			entries[ref] = entry
		}
	}
	result := new(accountingpb.ExecutionEconomicsComparison)
	for _, line := range stmt.Transactions {
		row := &accountingpb.ExecutionEconomicsRow{ExternalRef: line.ExternalRef, InstrumentId: line.InstrumentID, CurrencyCode: line.CurrencyCode, Convention: line.EconomicsConvention, Status: "unknown"}
		result.Rows = append(result.Rows, row)
		source, reason := signedEconomics(line, stmt.BusinessDate)
		if reason != "" {
			row.Reason = reason
			continue
		}
		entry := entries[line.ExternalRef]
		if entry == "" {
			row.Reason = "book_terms_unverified"
			continue
		}
		booked, err := ledger.ReadCustodyExecutionEconomics(ctx, tx, stmt.PortfolioID, accounts[0], entry, line.ExternalRef, line.InstrumentID, stmt.BusinessDate, basis.JournalPosition)
		if err != nil {
			return nil, err
		}
		assessBookedEconomics(row, line, entry, bindings[0].Venue, booked, source)
	}
	sort.Slice(result.Rows, func(i, j int) bool { return result.Rows[i].ExternalRef < result.Rows[j].ExternalRef })
	return result, nil
}

// Each vector has quantity, unit price, commission, gross, net and posted
// settlement cash. Only the source computation interprets the signed convention.
type economicFigures struct{ quantity, price, commission, gross, net, settled *big.Rat }

func signedEconomics(line recon.Transaction, day time.Time) (economicFigures, string) {
	switch {
	case line.EconomicsConvention == "":
		return economicFigures{}, "convention_missing"
	case line.EconomicsConvention != executionEconomicsConvention:
		return economicFigures{}, "convention_unsupported"
	case !line.QuantityInInstrumentUnits || line.InstrumentID == "" || line.CurrencyCode == "" || line.Quantity == nil || line.Quantity.Sign() == 0 || line.Price == nil || line.Price.Sign() <= 0 || line.Commission == nil || line.Commission.Sign() < 0 || line.Cash == nil || !BusinessDay(line.TradeDate).Equal(day) || !BusinessDay(line.SettlementDate).Equal(day):
		return economicFigures{}, "source_terms_unverified"
	}
	gross := new(big.Rat).Neg(new(big.Rat).Mul(line.Quantity, line.Price))
	if new(big.Rat).Sub(gross, line.Commission).Cmp(line.Cash) != 0 {
		return economicFigures{}, "source_cash_inconsistent"
	}
	return economicFigures{line.Quantity, line.Price, line.Commission, gross, line.Cash, line.Cash}, ""
}

func assessBookedEconomics(row *accountingpb.ExecutionEconomicsRow, line recon.Transaction, entry, venue string, booked *ledger.ExecutionEconomics, source economicFigures) {
	if booked == nil || venue == "" || booked.Venue != venue || booked.VenueExecutionID == "" || booked.SettlementBasis != ledger.SettlementSettled || !BusinessDay(booked.TradeDate).Equal(BusinessDay(line.TradeDate)) || !BusinessDay(booked.SettlementDate).Equal(BusinessDay(line.SettlementDate)) {
		row.Reason = "book_terms_unverified"
		return
	}
	if booked.CashCurrency != line.CurrencyCode || booked.CommissionCurrency != line.CurrencyCode {
		row.Reason = "currency_mismatch"
		return
	}
	book := economicFigures{booked.Quantity, booked.Price, booked.Commission, booked.GrossCash, booked.NetCash, booked.SettledCash}
	b, s, d := new(accountingpb.ExecutionEconomicsFigures), new(accountingpb.ExecutionEconomicsFigures), new(accountingpb.ExecutionEconomicsFigures)
	status := "equal"
	for _, v := range []struct {
		b, s                *big.Rat
		wireB, wireS, wireD **commonpb.Decimal
	}{
		{book.quantity, source.quantity, &b.Quantity, &s.Quantity, &d.Quantity},
		{book.price, source.price, &b.Price, &s.Price, &d.Price},
		{book.commission, source.commission, &b.Commission, &s.Commission, &d.Commission},
		{book.gross, source.gross, &b.GrossCash, &s.GrossCash, &d.GrossCash},
		{book.net, source.net, &b.NetCash, &s.NetCash, &d.NetCash},
		{book.settled, source.settled, &b.SettledCash, &s.SettledCash, &d.SettledCash},
	} {
		if v.b == nil {
			row.Reason = "book_terms_unverified"
			return
		}
		delta := new(big.Rat).Sub(v.b, v.s)
		var bok, sok, dok bool
		*v.wireB, bok = exactProto(v.b)
		*v.wireS, sok = exactProto(v.s)
		*v.wireD, dok = exactProto(delta)
		if !bok || !sok || !dok {
			row.Reason = "precision_unverified"
			return
		}
		if delta.Sign() != 0 {
			status = "mismatch"
		}
	}
	row.Status, row.EntryId, row.Book, row.Custodian, row.Difference = status, entry, b, s, d
	row.BookSettlementDate = booked.SettlementDate.UTC().Format(time.DateOnly)
	row.CustodianSettlementDate = line.SettlementDate.UTC().Format(time.DateOnly)
}
