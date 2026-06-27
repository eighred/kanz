package collateral

import (
	"sort"
	"time"
)

// COLL-01d — financing economics and the forward cash ladder. A FinancingTrade
// (repo / reverse-repo / securities lending) funds a position; its cash flows —
// the near-leg exchange and the far-leg unwind with accrued financing — land on
// settlement dates, extending the portfolio's forward cash projection. The cash
// ladder rolls those dated flows into a running settlement-date balance, the
// forward view of the Portfolio cashBalance the risk engine carries as a single
// number today.

// FinancingKind mirrors collateral.v1.FinancingKind.
type FinancingKind int

const (
	Repo FinancingKind = iota
	ReverseRepo
	SecLending
	SecBorrow
)

// FinancingTrade is a repo / sec-lending trade — the float working shape behind
// collateral.v1.FinancingTrade.
type FinancingTrade struct {
	ID       string
	Kind     FinancingKind
	Currency string
	// Principal is the cash leg amount (positive).
	Principal float64
	// Rate is the annualized financing rate (repo rate / lending fee), decimal.
	Rate  float64
	Start time.Time
	End   time.Time
}

// actDays is the ACT/365F year fraction between two dates.
func actDays(start, end time.Time) float64 {
	return end.Sub(start).Hours() / 24 / 365
}

// Interest is the financing accrued over the trade's life (Principal·Rate·τ).
func (f FinancingTrade) Interest() float64 {
	return f.Principal * f.Rate * actDays(f.Start, f.End)
}

// CashFlows returns the trade's dated cash flows. For a Repo we BORROW cash now
// (+principal at start) and repay principal + interest at the end (−); a
// ReverseRepo is the mirror (lend now, receive principal + interest back). A
// securities-lending trade exchanges only the fee leg at the end.
func (f FinancingTrade) CashFlows() []CashFlow {
	switch f.Kind {
	case Repo:
		return []CashFlow{
			{Date: f.Start, Currency: f.Currency, Amount: f.Principal},
			{Date: f.End, Currency: f.Currency, Amount: -(f.Principal + f.Interest())},
		}
	case ReverseRepo, SecBorrow:
		return []CashFlow{
			{Date: f.Start, Currency: f.Currency, Amount: -f.Principal},
			{Date: f.End, Currency: f.Currency, Amount: f.Principal + f.Interest()},
		}
	default: // SecLending — we earn the fee at the end, no principal exchange
		return []CashFlow{
			{Date: f.End, Currency: f.Currency, Amount: f.Interest()},
		}
	}
}

// CashFlow is one dated settlement-cash movement.
type CashFlow struct {
	Date     time.Time
	Currency string
	Amount   float64
}

// LadderRung is one settlement date in the forward cash ladder: the net flow on
// that date and the running balance after it.
type LadderRung struct {
	Date    time.Time
	Net     float64
	Balance float64
}

// BuildLadder rolls dated cash flows into a forward ladder starting from an
// opening balance, netting same-date flows and accumulating the running balance
// in date order. The final rung's balance equals opening + Σ flows — the cash
// ladder nets to the closing balance (the COLL-01e property).
func BuildLadder(opening float64, flows []CashFlow) []LadderRung {
	byDate := make(map[time.Time]float64)
	var dates []time.Time
	for _, f := range flows {
		if _, seen := byDate[f.Date]; !seen {
			dates = append(dates, f.Date)
		}
		byDate[f.Date] += f.Amount
	}
	sort.Slice(dates, func(i, j int) bool { return dates[i].Before(dates[j]) })

	balance := opening
	rungs := make([]LadderRung, 0, len(dates))
	for _, d := range dates {
		net := byDate[d]
		balance += net
		rungs = append(rungs, LadderRung{Date: d, Net: net, Balance: balance})
	}
	return rungs
}

// ClosingBalance is the balance after the last rung (opening when there are no
// flows) — the forward-projected cash.
func ClosingBalance(opening float64, flows []CashFlow) float64 {
	rungs := BuildLadder(opening, flows)
	if len(rungs) == 0 {
		return opening
	}
	return rungs[len(rungs)-1].Balance
}
