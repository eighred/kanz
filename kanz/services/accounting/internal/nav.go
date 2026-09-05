// Package accounting is the IBOR's valuation layer (IBOR-01d): daily NAV over the
// folded book, the P&L attribution of a NAV change to its drivers (price / cash /
// fx / corporate-action), and the interest/dividend accruals that carry
// earned-not-received income in NAV. It sits on top of the event-sourced ledger:
// the book is the state, this package values it.
package accounting

import (
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// NAV is a point-in-time valuation of a book: the headline total and its cash /
// security / accrued components, with the attribution of the change since a prior
// valuation. total = cash + securityValue + accrued.
type NAV struct {
	PortfolioID   string
	Currency      string
	Total         *big.Rat
	Cash          *big.Rat
	SecurityValue *big.Rat
	Accrued       *big.Rat
	AsOf          time.Time
	Attribution   []PnlComponent

	// LocalExposure is the net book value contributed by each currency BEFORE
	// conversion to the reporting currency (security + cash + accrued, in local
	// terms). It is the base the FX attribution driver revalues (FXPnL) — the
	// reporting-currency exposure is present but revalues to zero. A domestic
	// book carries the reporting currency alone, which revalues to zero, so
	// FXPnL over it is 0 exactly as it was when ComputeNAV returned nil here.
	LocalExposure map[string]*big.Rat
}

// PnlComponent is one attributed driver of a NAV change. The components sum to
// the total change.
type PnlComponent struct {
	Source string // "price", "cash", "fx", "corporate_action"
	Amount *big.Rat
}

// ComputeNAV values a DOMESTIC book — one whose cash and accrued income are all
// in the reporting currency — at the given prices, and REFUSES any other book.
//
// IT IS NO LONGER A SECOND IMPLEMENTATION, and that is the repair (#1041). It
// was one, and the two had diverged in the direction that costs money: this
// function read ONE cash bucket and ONE accrued bucket, so every other currency
// the book held was not summed, not converted, and NOT REPORTED AS OMITTED. A
// book holding USD 100 and EUR 50 answered NAV{Cash: 100} with a nil error —
// understated by the entire value of every non-reporting currency the fund
// holds, on the fund's headline number, in the direction that reads as "nothing
// to act on". One EUR subscription through the cash-movement endpoint produces
// such a book, and custody reconciliation was already comparing per-currency
// balances the valuation could not express.
//
// It now delegates to ComputeNAVInCurrency with an IDENTITY rate table — the
// relationship that function's own doc comment already asserted. The identity
// table quotes the reporting currency at 1 and nothing else, so a foreign cash
// or accrued bucket fails the completeness gate BY NAME instead of vanishing.
//
// WHAT IT STILL CANNOT PROVE, and the caller must. A position's quote currency
// is not in the book — it is a security-master join (InstrumentCurrency), and
// with none supplied every instrument defaults to the reporting currency. So
// this entry point establishes domesticity for cash and accrued only. A caller
// holding the join passes it to ComputeNAVInCurrency directly, which is what
// Server.computeNAV does; a deployment that holds no join says so at startup
// through the FX posture rather than discovering it one valuation at a time.
func ComputeNAV(b *ledger.Book, currency string, asOf time.Time, prices map[string]*big.Rat) (NAV, error) {
	return ComputeNAVInCurrency(b, currency, asOf, prices, nil, NewFXTable(currency, nil))
}

// ComputeNAVInCurrency values a MULTI-CURRENCY book in a single reporting
// currency (PARITY-05f). Each position is priced in its reference currency
// (instrCcy, defaulting to reporting for a domestic instrument) and converted at
// fx; cash and accrued are summed across every currency the book holds and
// likewise converted. A missing price OR a missing FX rate for a currency the
// book actually uses fails the whole valuation — an incomplete multi-currency
// NAV is never produced (the regulatory-filing completeness discipline).
//
// The returned NAV carries LocalExposure (per-currency pre-conversion book
// value) so FXPnL can attribute the currency-revaluation driver from real rates.
//
// THIS IS THE ONLY VALUATION IN THIS PACKAGE (#1041). ComputeNAV used to be a
// second one, described here as "the fast path of" this general form; that claim
// was true of the arithmetic on a domestic book and false of everything else,
// and the gap was the whole value of every foreign bucket. ComputeNAV now calls
// this with an identity rate table, so the claim holds by construction rather
// than by inspection — TestComputeNAVMatchesTheGeneralFormOnADomesticBook pins
// the numbers from the other side.
func ComputeNAVInCurrency(b *ledger.Book, reporting string, asOf time.Time, prices map[string]*big.Rat, instrCcy InstrumentCurrency, fx FXConverter) (NAV, error) {
	if fx == nil {
		return NAV{}, fmt.Errorf("accounting: NAV requires an FX converter")
	}
	local := map[string]*big.Rat{} // currency → net local-currency book value
	var unvaluable currencySet

	sec := new(big.Rat)
	for inst, p := range b.Positions {
		if p.Qty.Sign() == 0 {
			continue
		}
		px, ok := prices[inst]
		if !ok {
			return NAV{}, fmt.Errorf("accounting: NAV missing price for %q", inst)
		}
		ccy := instrCcy.Of(inst, reporting)
		mvLocal := new(big.Rat).Mul(p.Qty, px)
		addLocal(local, ccy, mvLocal)
		conv, ok := convert(fx, ccy, mvLocal)
		if !ok {
			unvaluable.add(ccy)
			continue
		}
		sec.Add(sec, conv)
	}

	cash := new(big.Rat)
	for ccy, v := range b.Cash {
		if v.Sign() == 0 {
			continue
		}
		addLocal(local, ccy, v)
		conv, ok := convert(fx, ccy, v)
		if !ok {
			unvaluable.add(ccy)
			continue
		}
		cash.Add(cash, conv)
	}

	accrued := new(big.Rat)
	for ccy, v := range b.Accrued {
		if v.Sign() == 0 {
			continue
		}
		addLocal(local, ccy, v)
		conv, ok := convert(fx, ccy, v)
		if !ok {
			unvaluable.add(ccy)
			continue
		}
		accrued.Add(accrued, conv)
	}

	// EVERY UNVALUABLE CURRENCY, NAMED, BEFORE ANY NUMBER IS RETURNED. The loops
	// above accumulate rather than returning on the first miss so this message
	// carries the whole list: an operator reading "no FX rate for EUR" configures
	// one pair, gets the same refusal for CHF, and configures another. The list is
	// sorted because map iteration order is not, and a refusal that names a
	// different currency on each attempt reads as intermittency rather than as a
	// missing configuration.
	if missing := unvaluable.sorted(); len(missing) > 0 {
		return NAV{}, fmt.Errorf("accounting: NAV in %s REFUSED — the book holds value in %s, and "+
			"this valuation has no FX rate for %s. A NAV that left those buckets out would understate "+
			"by their whole amount and look exactly like a correct one, so none is produced (#1041); "+
			"configure the rate (ACCOUNTING_FX_PAIRS) or supply `fx` on the request",
			reporting, strings.Join(missing, ", "), plural(len(missing), "it", "them"))
	}

	total := new(big.Rat).Add(cash, sec)
	total.Add(total, accrued)
	return NAV{
		PortfolioID:   b.PortfolioID,
		Currency:      reporting,
		Total:         total,
		Cash:          cash,
		SecurityValue: sec,
		Accrued:       accrued,
		AsOf:          asOf,
		LocalExposure: local,
	}, nil
}

// convert multiplies a local-currency amount by its FX rate into the reporting
// currency. ok=false ⇒ no rate for that currency; the CALLER records which one
// and refuses the whole valuation, because a per-item error names only the
// currency it tripped over first and map iteration picks that one at random.
func convert(fx FXConverter, ccy string, amount *big.Rat) (*big.Rat, bool) {
	rate, ok := fx.Rate(ccy)
	if !ok {
		return nil, false
	}
	return new(big.Rat).Mul(amount, rate), true
}

// currencySet accumulates the distinct currencies a valuation could not convert,
// in no particular order until sorted.
type currencySet struct {
	seen  map[string]bool
	names []string
}

func (s *currencySet) add(ccy string) {
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	if s.seen[ccy] {
		return
	}
	s.seen[ccy] = true
	s.names = append(s.names, ccy)
}

// sorted returns the currencies in a stable order. A blank currency — a cash leg
// booked with no CashCurrency — is rendered as "" so the refusal names something
// an operator can search the journal for rather than an empty gap in a sentence.
func (s *currencySet) sorted() []string {
	out := make([]string, 0, len(s.names))
	for _, n := range s.names {
		if n == "" {
			n = `""`
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// plural picks the singular or plural form for a count.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// addLocal accumulates a per-currency exposure amount.
func addLocal(m map[string]*big.Rat, ccy string, v *big.Rat) {
	cur := m[ccy]
	if cur == nil {
		cur = new(big.Rat)
		m[ccy] = cur
	}
	cur.Add(cur, v)
}

// Attribute decomposes the NAV change from prior to current into its drivers and
// returns current with Attribution set. The period entries are those folded
// between the two valuations (used to separate trade cash from external flows);
// fx is the currency-revaluation effect (zero for a single-currency book).
//
// Identity: price + cash + fx + corporate_action == current.Total − prior.Total,
// where
//   - cash             = external cash flows (subscriptions/redemptions/fees),
//   - corporate_action = dividend/coupon/merger cash + the change in accrued income,
//   - price            = mark-to-market = Δ security value + trade cash − fx,
//   - fx               = the supplied currency revaluation.
func Attribute(prior, current NAV, periodEntries []*ledger.Event, fx *big.Rat) NAV {
	if fx == nil {
		fx = new(big.Rat)
	}
	tradeCash := new(big.Rat)
	external := new(big.Rat)
	for _, e := range periodEntries {
		if e.Cash == nil {
			continue
		}
		switch e.Type {
		case ledger.EntryTrade:
			tradeCash.Add(tradeCash, e.Cash)
		case ledger.EntryCash, ledger.EntryFee:
			external.Add(external, e.Cash)
		}
	}

	deltaSec := new(big.Rat).Sub(current.SecurityValue, prior.SecurityValue)
	deltaAccrued := new(big.Rat).Sub(current.Accrued, prior.Accrued)

	// price = Δ security value + trade cash − fx (the mark-to-market: holdings
	// revalued, plus price moves since each in-period trade).
	price := new(big.Rat).Add(deltaSec, tradeCash)
	price.Sub(price, fx)

	// corporate-action income = total cash change not explained by trades or
	// external flows, plus accrued-income earned in the period.
	//
	// IT IS A RESIDUAL, AND A 0 HERE MEANS NEITHER "no corporate actions occurred"
	// NOR "none were ingested" (#588). Nothing in this platform publishes an
	// accounting.v1.CorporateAction, so no EntryCorporateAction has ever reached
	// this book and this component can only be the accrual term plus rounding —
	// which reads exactly like a quiet quarter. The signal that DOES tell them
	// apart is the composition root's posture gauge,
	// kanz_accounting_entry_source_wired{type="corporate_action"}; do not read
	// this number as evidence that a split or dividend was processed.
	deltaCash := new(big.Rat).Sub(current.Cash, prior.Cash)
	corpCash := new(big.Rat).Sub(deltaCash, tradeCash)
	corpCash.Sub(corpCash, external)
	corpAction := new(big.Rat).Add(corpCash, deltaAccrued)

	current.Attribution = []PnlComponent{
		{Source: "price", Amount: price},
		{Source: "cash", Amount: external},
		{Source: "fx", Amount: new(big.Rat).Set(fx)},
		{Source: "corporate_action", Amount: corpAction},
	}
	return current
}

// AttributionTotal sums the attribution components — equals current.Total −
// prior.Total by the identity above.
func AttributionTotal(n NAV) *big.Rat {
	sum := new(big.Rat)
	for _, c := range n.Attribution {
		sum.Add(sum, c.Amount)
	}
	return sum
}

// StraightLineAccrual is the linearly-accreted portion of a known income amount
// (a bond coupon, a declared dividend) earned between start and end as of asOf —
// amount × elapsed/total, clamped to [0, amount]. It is the actual/actual day-
// count accrual the book posts daily so NAV reflects earned-not-received income.
func StraightLineAccrual(amount *big.Rat, start, end, asOf time.Time) *big.Rat {
	if amount == nil || !end.After(start) {
		return new(big.Rat)
	}
	if !asOf.After(start) {
		return new(big.Rat)
	}
	if !asOf.Before(end) {
		return new(big.Rat).Set(amount)
	}
	elapsed := big.NewRat(int64(asOf.Sub(start)), 1)
	totalDur := big.NewRat(int64(end.Sub(start)), 1)
	frac := new(big.Rat).Quo(elapsed, totalDur)
	return new(big.Rat).Mul(amount, frac)
}

// AccrualEntry builds the journal posting that books accrued income to asOf —
// an EntryAccrual carrying the incremental accrual since the prior posting. The
// caller passes the increment (accrued-now minus accrued-already-posted) so the
// book's accrued balance tracks the schedule.
func AccrualEntry(entryID, portfolioID, instrument, currency string, increment *big.Rat, asOf, knowledge time.Time) *ledger.Event {
	return &ledger.Event{
		EntryID:      entryID,
		PortfolioID:  portfolioID,
		Type:         ledger.EntryAccrual,
		InstrumentID: instrument,
		Cash:         increment,
		CashCurrency: currency,
		// An accrual has NO settled basis by construction (#1043): accrued income is
		// earned-not-received, so it is folded into Book.Accrued and never into a
		// settled balance. ledger.SettlementUnknown is left in place rather than
		// SettlementPending because nothing here knows the pay date either.
		Effective: asOf,
		Knowledge: knowledge,
		SourceRef: instrument,
	}
}
