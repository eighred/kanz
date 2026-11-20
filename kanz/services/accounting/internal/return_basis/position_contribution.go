package returnbasis

import (
	"context"
	"math"
	"math/big"
	"sort"
	"time"

	"github.com/eighred/kanz/internal/dec"
	performancepb "github.com/eighred/kanz/kanz-schemas-go/performance/v1"
	"github.com/eighred/kanz/services/accounting/internal"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/jackc/pgx/v5"
)

const positionJournalLimit = 4096

// PositionBasis is signed with the dated NAV and cash evidence. Accounting
// derives the holdings and non-security values from the recorded journal.
type PositionBasis struct {
	Opening       *PositionMarks `json:"opening"`
	AtFlow        *PositionMarks `json:"at_flow"`
	Closing       *PositionMarks `json:"closing"`
	TradeCoverage *TradeCoverage `json:"trade_coverage,omitempty"`
}

type PositionMarks struct {
	Reference string               `json:"reference"`
	Effective time.Time            `json:"effective"`
	KnownAt   time.Time            `json:"known_at"`
	Prices    map[string]dec.Exact `json:"prices"`
}

func validPositionBasis(e Evidence) bool {
	if e.PositionBasis == nil {
		return true
	}
	b := e.PositionBasis
	for _, marks := range []*PositionMarks{b.Opening, b.AtFlow, b.Closing} {
		if marks == nil {
			continue
		}
		if !identifier.MatchString(marks.Reference) || marks.Effective.IsZero() ||
			marks.Effective.Location() != time.UTC || marks.Effective.Nanosecond()%1000 != 0 ||
			marks.KnownAt.Location() != time.UTC || marks.KnownAt.Nanosecond()%1000 != 0 ||
			marks.KnownAt.Before(marks.Effective) || marks.KnownAt.After(e.AsOf) || len(marks.Prices) > 32 {
			return false
		}
		for id, price := range marks.Prices {
			amount, err := price.Rat()
			if !identifier.MatchString(id) || err != nil || amount.Sign() < 0 {
				return false
			}
			if _, ok := dec.ToProtoExact(amount); !ok {
				return false
			}
		}
	}
	return validTradeCoverage(b.TradeCoverage)
}

func verifiedAssessment(ctx context.Context, tx pgx.Tx, r Receipt) (*performancepb.DailyReturnAssessment, error) {
	report := assessment(r)
	if r.Evidence.SectorAttribution {
		report.SectorAttribution = &performancepb.DailySectorAttribution{
			Result: &performancepb.DailySectorAttribution_UnassessedReason{UnassessedReason: "sector_portfolio_unassessed"},
		}
	}
	b := r.Evidence.PositionBasis
	if b == nil {
		return report, nil
	}
	positions := &performancepb.DailyPositionContribution{
		Method: performancepb.PositionContributionMethod_POSITION_CONTRIBUTION_METHOD_LINKED_UNCHANGED_HOLDINGS,
	}
	if b.TradeCoverage != nil {
		positions.Method = performancepb.PositionContributionMethod_POSITION_CONTRIBUTION_METHOD_LINKED_BOOKED_TRADES
		positions.TradeCoverageReference = b.TradeCoverage.Reference
	}
	report.PositionContribution = positions
	if b.Opening != nil {
		positions.OpeningPriceReference = b.Opening.Reference
	}
	if b.AtFlow != nil {
		positions.FlowPriceReference = b.AtFlow.Reference
	}
	if b.Closing != nil {
		positions.ClosingPriceReference = b.Closing.Reference
	}
	result, reason, err := positionContribution(ctx, tx, r.Evidence, report)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		positions.Result = &performancepb.DailyPositionContribution_UnassessedReason{UnassessedReason: reason}
	} else {
		positions.Result = &performancepb.DailyPositionContribution_Measured{Measured: result}
	}
	return report, nil
}

func positionContribution(ctx context.Context, tx pgx.Tx, e Evidence, report *performancepb.DailyReturnAssessment) (*performancepb.PositionContributionBreakdown, string, error) {
	if report.GetMeasured() == nil {
		return nil, "portfolio_return_unassessed", nil
	}
	b := e.PositionBasis
	split := e.Flows.Split
	if b.Opening == nil || b.Closing == nil || (split != nil && b.AtFlow == nil) {
		return nil, "position_marks_missing", nil
	}
	start, end := report.Start.AsTime(), report.End.AsTime()
	if !b.Opening.Effective.Equal(start) || !b.Closing.Effective.Equal(end) ||
		(split == nil && b.AtFlow != nil) || (split != nil && !b.AtFlow.Effective.Equal(split.Effective)) {
		return nil, "position_mark_window_mismatch", nil
	}
	if err := lockJournal(ctx, tx, e.Portfolio); err != nil {
		return nil, "", err
	}
	var executionWindow ledger.ExecutionWindow
	if b.TradeCoverage != nil {
		executionWindow = ledger.ExecutionWindow{Start: start, End: end}
	}
	entries, err := ledger.RecordedJournal(ctx, tx, e.Portfolio, e.AsOf, positionJournalLimit+1, executionWindow)
	if err != nil {
		return nil, "", err
	}
	if len(entries) > positionJournalLimit {
		return nil, "position_journal_limit", nil
	}
	inWindow := func(at time.Time) bool { return at.After(start) && !at.After(end) }
	for _, event := range entries {
		if event.Action != nil && inWindow(event.Action.PaidAt) {
			return nil, "position_activity_unsupported", nil
		}
		if !inWindow(event.Effective) {
			continue
		}
		switch event.Type {
		case ledger.EntryFee, ledger.EntryAccrual:
			if event.Action == nil && event.CashCurrency == e.Currency {
				if b.TradeCoverage != nil && event.IsExecutionFeeAdjustment() {
					return nil, "trade_fee_revision_unsupported", nil
				}
				continue
			}
		case ledger.EntryTrade:
			if b.TradeCoverage != nil {
				continue
			}
		case ledger.EntryCash:
			if split != nil && event.EntryID == "cash:"+split.MovementID {
				continue
			}
		}
		return nil, "position_activity_unsupported", nil
	}
	openingBook := ledger.ReplayAsOf(e.Portfolio, entries, start, e.AsOf)
	holdings := positionQuantities(openingBook)
	var flowAt time.Time
	if split != nil {
		flowAt = split.Effective
	}
	firstTrading, secondTrading, tradeReason := verifyTrades(e, entries, holdings, start, end, flowAt)
	if tradeReason != "" {
		return nil, tradeReason, nil
	}
	valuate := func(marks *PositionMarks, mark *Close, at time.Time) (contributionValue, string) {
		book := openingBook
		if !at.Equal(start) {
			book = ledger.ReplayAsOf(e.Portfolio, entries, at, e.AsOf)
		}
		return positionValuation(book, e.Currency, marks, mark)
	}
	opening, reason := valuate(b.Opening, e.Opening, start)
	if reason != "" {
		return nil, reason, nil
	}
	closing, reason := valuate(b.Closing, e.Closing, end)
	if reason != "" {
		return nil, reason, nil
	}
	periods := []contributionPeriod{{from: opening, to: closing, link: big.NewRat(1, 1), trading: firstTrading}}
	if split != nil {
		before, reason := valuate(b.AtFlow, split.Before, split.Effective.Add(-time.Microsecond))
		if reason != "" {
			return nil, reason, nil
		}
		after, reason := valuate(b.AtFlow, split.After, split.Effective)
		if reason != "" {
			return nil, reason, nil
		}
		periods = []contributionPeriod{
			{from: opening, to: before, link: new(big.Rat).Quo(closing.nav.Total, after.nav.Total), trading: firstTrading},
			{from: after, to: closing, link: big.NewRat(1, 1), trading: secondTrading},
		}
	}
	result, reason, err := contributionBreakdown(e, periods)
	if err == nil && reason == "" && report.SectorAttribution != nil {
		if err := attributeSectors(ctx, tx, e, report, periods, entries); err != nil {
			return nil, "", err
		}
	}
	return result, reason, err
}

func positionQuantities(book *ledger.Book) map[string]*big.Rat {
	positions := make(map[string]*big.Rat, len(book.Positions))
	for id, p := range book.Positions {
		if p.Qty.Sign() != 0 {
			positions[id] = p.Qty
		}
	}
	return positions
}

type contributionValue struct {
	nav       accounting.NAV
	positions map[string]*big.Rat
}

func positionValuation(book *ledger.Book, currency string, marks *PositionMarks, close *Close) (contributionValue, string) {
	holdings := positionQuantities(book)
	if marks.Prices == nil || len(marks.Prices) != len(holdings) {
		return contributionValue{}, "position_marks_missing"
	}
	prices := make(map[string]*big.Rat, len(holdings))
	values := make(map[string]*big.Rat, len(holdings))
	for id, q := range holdings {
		if q.Sign() < 0 {
			return contributionValue{}, "position_activity_unsupported"
		}
		mark, ok := marks.Prices[id]
		if !ok {
			return contributionValue{}, "position_marks_missing"
		}
		prices[id], _ = mark.Rat()
		values[id] = new(big.Rat).Mul(q, prices[id])
	}
	nav, err := accounting.ComputeNAV(book, currency, marks.Effective, prices)
	if err != nil {
		return contributionValue{}, "position_activity_unsupported"
	}
	signed, _ := close.Total.Rat()
	if signed.Cmp(nav.Total) != 0 {
		return contributionValue{}, "position_nav_not_reconciled"
	}
	return contributionValue{nav: nav, positions: values}, ""
}

type contributionPeriod struct {
	from, to contributionValue
	link     *big.Rat
	trading  tradeLegs
}

func linkedContribution(periods []contributionPeriod, changeFor func(contributionPeriod) *big.Rat) (*big.Rat, *big.Rat) {
	pnl, contribution := new(big.Rat), new(big.Rat)
	for _, period := range periods {
		change := changeFor(period)
		pnl.Add(pnl, change)
		rate := new(big.Rat).Quo(change, period.from.nav.Total)
		contribution.Add(contribution, rate.Mul(rate, period.link))
	}
	return pnl, contribution
}

func contributionBreakdown(e Evidence, periods []contributionPeriod) (*performancepb.PositionContributionBreakdown, string, error) {
	instruments := map[string]struct{}{}
	for _, period := range periods {
		for id := range period.from.positions {
			instruments[id] = struct{}{}
		}
		for id := range period.to.positions {
			instruments[id] = struct{}{}
		}
		for id := range period.trading.principal {
			instruments[id] = struct{}{}
		}
	}
	ids := make([]string, 0, len(instruments))
	for id := range instruments {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	cashPnL, cashContribution := linkedContribution(periods, func(p contributionPeriod) *big.Rat {
		from := new(big.Rat).Add(p.from.nav.Cash, p.from.nav.Accrued)
		to := new(big.Rat).Add(p.to.nav.Cash, p.to.nav.Accrued)
		return to.Sub(to.Sub(to, from), &p.trading.cash)
	})
	cash, ok := dec.ToProtoExact(cashPnL)
	cashReturn, _ := cashContribution.Float64()
	if !ok || math.IsInf(cashReturn, 0) {
		return nil, "position_contribution_out_of_range", nil
	}
	out := &performancepb.PositionContributionBreakdown{CashPnl: cash, CashReturnContribution: cashReturn}
	totalPnL, totalContribution := new(big.Rat).Set(cashPnL), new(big.Rat).Set(cashContribution)
	if e.PositionBasis.TradeCoverage != nil {
		pnl, rate := linkedContribution(periods, func(p contributionPeriod) *big.Rat {
			return new(big.Rat).Neg(&p.trading.fees)
		})
		amount, ok := dec.ToProtoExact(pnl)
		fraction, _ := rate.Float64()
		if !ok || math.IsInf(fraction, 0) {
			return nil, "position_contribution_out_of_range", nil
		}
		out.ExecutionCosts = &performancepb.ExecutionCostContribution{Pnl: amount, ReturnContribution: fraction}
		totalPnL.Add(totalPnL, pnl)
		totalContribution.Add(totalContribution, rate)
	}
	for _, id := range ids {
		pnl, rate := linkedContribution(periods, func(p contributionPeriod) *big.Rat {
			return deltaPosition(p, id)
		})
		amount, ok := dec.ToProtoExact(pnl)
		fraction, _ := rate.Float64()
		if !ok || math.IsInf(fraction, 0) {
			return nil, "position_contribution_out_of_range", nil
		}
		out.Positions = append(out.Positions, &performancepb.PositionReturnContribution{InstrumentId: id, Pnl: amount, ReturnContribution: fraction})
		totalPnL.Add(totalPnL, pnl)
		totalContribution.Add(totalContribution, rate)
	}
	want, _ := exactReturn(e)
	wantPnL := new(big.Rat)
	for _, period := range periods {
		wantPnL.Add(wantPnL, new(big.Rat).Sub(period.to.nav.Total, period.from.nav.Total))
	}
	if totalContribution.Cmp(want) != 0 || totalPnL.Cmp(wantPnL) != 0 {
		return nil, "position_nav_not_reconciled", nil
	}
	out.TotalPnl, ok = dec.ToProtoExact(totalPnL)
	out.TotalReturnContribution, _ = totalContribution.Float64()
	if !ok {
		return nil, "position_contribution_out_of_range", nil
	}
	return out, "", nil
}
