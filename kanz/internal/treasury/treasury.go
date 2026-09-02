// Package treasury measures the cash drag on a portfolio — and refuses to state
// a figure it cannot stand behind (#963).
//
// # Why a measurement, and only a measurement
//
// Uninvested cash is a permanent performance drag that compounds daily and is
// invisible to every control this platform has: it breaches no mandate, trips no
// risk limit, and produces no reconciliation break. It is a loss that only a
// control designed to look for it will ever report.
//
// #963 describes the engine that would ACT on it — a target buffer, a sweep into
// a money-market vehicle, a forward cash ladder. That engine is blocked, and the
// ordering is not a scheduling detail. A sweep whose forward ladder cannot see
// coupons, dividends (#588) or margin calls (#408) will sweep cash a corporate
// action was about to need, and on the capital path a failed settlement is a
// counterparty event rather than an inconvenience. Building the sweep first
// produces a control that is confidently wrong in the direction that causes
// settlement failures, which is worse than not having it.
//
// So this package does the part that is NOT blocked and that sizes the rest:
// it says how much cash is sitting idle, and — for every portfolio where it
// cannot say — exactly which missing input is why. The second half is the more
// useful one today, because it turns "#588 is open" from a ticket into a number.
//
// # Why it refuses so often, and why that is the point
//
// A drag figure is cash over NAV. BOTH SIDES OF THAT FRACTION HAVE A KNOWN WAY
// OF BEING WRONG in this estate, and each one is silent:
//
//   - The numerator. accounting's ledger folds six kinds of journal entry and two
//     have no producer anywhere (#588), so a book's Cash is short every dividend
//     and coupon ever paid to it. comp.CashCompleteness is the producer's own
//     statement of that, and nil there means UNSTATED rather than complete.
//   - The denominator. A Book whose NAV is NAVBasisGrossPositions carries the sum
//     of position market values and NOTHING ELSE — no cash, no financing. #780 is
//     what that costs: gross and NAV were the same number, so a leverage ratio was
//     structurally 1.0 and a cap bound on nothing. Dividing cash by that is not a
//     drag figure; it is cash over a number that is not the fund's value.
//
// Either one silently produces a plausible percentage. A treasury desk told "this
// book is 3% cash" when the true figure is 6% makes a real allocation decision on
// it, and nothing downstream would ever contradict the number. So Measure states
// a figure ONLY when both sides are vouched for, and otherwise returns the reason
// — which names the team that has to fix it.
//
// This is the same discipline internal/measureread applies to risk measures and
// comp.NAVBasis applies to leverage: a value this plane will not state is not a
// zero, and "nobody said" is not "checked, and fine".
package treasury

import (
	"math/big"
	"strings"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/dec"
)

// Status is whether an idle-cash figure may be stated.
//
// THE ZERO VALUE IS THE REFUSAL, deliberately. A Drag nobody filled in must not
// read as a measured 0% — that is the exact collapse this package exists to
// prevent, and a struct returned by an early error path is where it would happen.
type Status int

const (
	// StatusUnknown means no figure may be stated. Reason says which input is
	// missing; IdleShare is nil, NOT zero.
	StatusUnknown Status = iota
	// StatusMeasured means both sides of the fraction were vouched for and
	// IdleShare is the portfolio's idle-cash share.
	StatusMeasured
)

func (s Status) String() string {
	if s == StatusMeasured {
		return "MEASURED"
	}
	return "UNKNOWN"
}

// Reason names why a drag figure could not be stated.
//
// A CLOSED SET, because it becomes a metric label and an operator's routing
// decision. Each value sends somebody different: cash_unvouched is #588 and the
// accounting feed, nav_not_equity is the mark feed and the position projector,
// currency_mismatch is a data defect in one book. A free-text reason would be
// unbounded cardinality on the metric and unroutable in the log.
type Reason string

const (
	// ReasonNone is the reason on a MEASURED drag.
	ReasonNone Reason = ""
	// ReasonCashUnknown is a book with no cash figure at all — no announcement has
	// arrived for this portfolio, or the last one aged past cashview's bound.
	ReasonCashUnknown Reason = "cash_unknown"
	// ReasonCashUnvouched is a cash figure whose producer did not state that it is
	// whole: either it named omitted entry types (#588's dividends and coupons) or
	// it said nothing at all. Both mean nobody stands behind the numerator.
	ReasonCashUnvouched Reason = "cash_unvouched"
	// ReasonNAVUnknown is a book that could not be valued — a holding with no live
	// mark, or cash unknown at the moment equity was attempted.
	ReasonNAVUnknown Reason = "nav_unknown"
	// ReasonNAVNotEquity is a NAV that is a gross-positions proxy rather than the
	// fund's equity. The number exists and is the wrong denominator (#780).
	ReasonNAVNotEquity Reason = "nav_not_equity"
	// ReasonNAVNotPositive is a book whose equity is zero or negative. A share of
	// it is not meaningful, and a negative denominator would render a NEGATIVE
	// idle-cash percentage onto a dashboard.
	ReasonNAVNotPositive Reason = "nav_not_positive"
	// ReasonCurrencyMismatch is cash denominated in something other than the
	// book's base currency. Netting them would be an unconverted FX error.
	ReasonCurrencyMismatch Reason = "currency_mismatch"
)

// Reasons returns every refusal reason, for seeding a metric's labels.
//
// DERIVED FROM ONE LIST IN ONE PLACE. A consumer that enumerated these by hand
// would be a second copy of the vocabulary, and a new reason would then ship with
// no series behind it — the defect #806 and #803 both were. ReasonNone is
// excluded: it is the absence of a reason, not a refusal, and seeding it would
// put a permanently-zero "" label on the metric.
func Reasons() []Reason {
	return []Reason{
		ReasonCashUnknown,
		ReasonCashUnvouched,
		ReasonNAVUnknown,
		ReasonNAVNotEquity,
		ReasonNAVNotPositive,
		ReasonCurrencyMismatch,
	}
}

// Drag is what this plane will say about one portfolio's idle cash.
type Drag struct {
	Status Status
	Reason Reason
	// Detail is an operator-facing note naming what to go and fix. Empty on a
	// MEASURED drag.
	Detail string
	// IdleShare is cash as a fraction of equity — nil unless MEASURED.
	//
	// A *big.Rat AND NOT A float64, matching every other ratio in this estate
	// (ConcentrationRule's weights, LeverageRule's gross/NAV). The float only
	// appears at the Prometheus boundary, where a histogram bucket needs one.
	IdleShare *big.Rat
	// Cash and NAV are the two sides, carried so a report can show the figures
	// the share was computed from rather than only the percentage.
	Cash *commonpb.Money
	NAV  *commonpb.Money
}

// Measured reports whether a figure may be stated.
func (d Drag) Measured() bool { return d.Status == StatusMeasured }

// Percent renders the idle share as a float for a metric bucket.
//
// THE ONLY float64 IN THIS PACKAGE, and it is not money — it is a dimensionless
// ratio crossing into a histogram, which cannot take a Rat. The exact value stays
// on IdleShare; nothing computes with this.
//
// It returns 0 on an UNKNOWN drag, and the caller MUST NOT record that: a refusal
// observed into the histogram is a portfolio reported as holding no idle cash,
// which is the false-zero this whole package is arranged against.
func (d Drag) Percent() float64 {
	if !d.Measured() || d.IdleShare == nil {
		return 0
	}
	f, _ := d.IdleShare.Float64()
	return f
}

// Measure states a book's idle-cash drag, or the reason it cannot.
//
// # The order of the checks is the order an operator should act
//
// THE NUMERATOR IS CHECKED FIRST. A drag figure with no trustworthy cash number
// is not a degraded drag figure — there is nothing to measure. NAV problems come
// second because they are about EXPRESSING a known cash number as a share; the
// cash itself is the subject. When a book fails both, the reported reason is the
// cash one, and that is deliberate: #588 is the systemic cause across the whole
// estate and fixing it changes every book at once, where a mark gap is usually
// one instrument.
//
// A nil book is UNKNOWN rather than a panic. This runs on a monitor sweep over
// every book the estate holds, and a measurement that can take down the control
// loop it observes is a worse trade than a missing sample.
func Measure(b *comp.Book) Drag {
	if b == nil {
		return Drag{Reason: ReasonCashUnknown, Detail: "no book"}
	}

	// --- the numerator ---
	if b.Cash == nil {
		return Drag{
			Reason: ReasonCashUnknown,
			Detail: "no cash balance for this portfolio: accounting has announced none on " +
				"accounting.balance.portfolio, or the last announcement aged past cashview's bound",
		}
	}
	// UNSTATED AND INCOMPLETE COLLAPSE HERE, and only here. They are different
	// facts with different owners — nobody wired the statement, versus a feed that
	// does not exist — and comp.CashCompleteness keeps them apart for the reader
	// that needs them. What they share is the only thing this decision is about:
	// the producer did not vouch for the figure, so a drag computed from it is a
	// percentage nobody stands behind. Detail carries the distinction on.
	if !b.CashCompleteness.Vouched() {
		d := Drag{Reason: ReasonCashUnvouched, Cash: b.Cash, NAV: b.NAV}
		if b.CashCompleteness.Stated() {
			d.Detail = "the cash balance omits entry types its own producer named (" +
				strings.Join(b.CashCompleteness.Omitted(), ",") + "): the idle figure would be short by " +
				"every such payment, and on a SHORT book it is OVERSTATED instead (#588)"
			return d
		}
		d.Detail = "the producer of this cash balance stated no completeness, so whether the " +
			"number is whole is unknown — check kanz_accounting_entry_source_wired on the " +
			"accounting deployment (#614)"
		return d
	}

	// --- the denominator ---
	if b.NAV == nil {
		return Drag{
			Reason: ReasonNAVUnknown, Cash: b.Cash,
			Detail: navDetail(b, "this book could not be valued"),
		}
	}
	if b.NAVBasis != comp.NAVBasisEquity {
		return Drag{
			Reason: ReasonNAVNotEquity, Cash: b.Cash, NAV: b.NAV,
			Detail: navDetail(b, "NAV is "+b.NAVBasis.String()+" and not equity, so it is the sum of "+
				"position market values with no cash and no financing in it; a share of that is not "+
				"a share of the fund (#780)"),
		}
	}

	cash, err := dec.MoneyIn(b.Cash, b.BaseCurrency)
	if err != nil {
		return Drag{
			Reason: ReasonCurrencyMismatch, Cash: b.Cash, NAV: b.NAV,
			Detail: "cash is denominated in " + b.Cash.GetCurrencyCode() + " and the book's base " +
				"currency is " + b.BaseCurrency + "; netting them without a rate is an FX error",
		}
	}
	nav, err := dec.MoneyIn(b.NAV, b.BaseCurrency)
	if err != nil {
		return Drag{
			Reason: ReasonCurrencyMismatch, Cash: b.Cash, NAV: b.NAV,
			Detail: "NAV is denominated in " + b.NAV.GetCurrencyCode() + " and the book's base " +
				"currency is " + b.BaseCurrency,
		}
	}
	// A ZERO OR NEGATIVE DENOMINATOR IS REFUSED, NOT DIVIDED BY. big.Rat.Quo
	// panics on a zero divisor, and a negative equity would render a negative
	// idle-cash percentage — a book that is underwater reported as holding less
	// than no idle cash, which reads as the best-managed fund on the dashboard.
	if nav.Sign() <= 0 {
		return Drag{
			Reason: ReasonNAVNotPositive, Cash: b.Cash, NAV: b.NAV,
			Detail: "equity is " + dec.Str(nav) + ": a share of a non-positive denominator is not " +
				"a drag figure, and a negative one would report an underwater book as the least " +
				"idle on the estate",
		}
	}

	return Drag{
		Status:    StatusMeasured,
		IdleShare: new(big.Rat).Quo(cash, nav),
		Cash:      b.Cash,
		NAV:       b.NAV,
	}
}

// navDetail appends the producer's own note about why NAV is what it is.
//
// comp.NAVBasisDetail EXISTS BECAUSE THE REFUSAL IS OTHERWISE UNACTIONABLE — "no
// mark for SOL-USD" and "cash unknown" send an operator to two different teams,
// and the producer is the only layer that knows which. Dropping it here would
// re-create exactly the unactionable refusal that field was added to end.
func navDetail(b *comp.Book, lead string) string {
	if b.NAVBasisDetail == "" {
		return lead
	}
	return lead + ": " + b.NAVBasisDetail
}
