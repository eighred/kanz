package compliance

import (
	"strings"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"

	// decutil: see equity.go — this package's own tests declare a local `dec(...)`
	// helper, so the platform decimal package is aliased rather than shadowed.
	decutil "github.com/eighred/kanz/internal/dec"
)

// Evidence keys a cash-ceiling refusal carries beyond the shared completeness
// pair. Named constants for the reason EvidenceBalanceCompleteness is one: a
// test asserting on a literal while the rule writes a different literal is a
// guard that checks nothing.
const (
	// EvidenceIdleCash is the uninvested balance the ceiling was compared
	// against, or that could not be compared. Present on every CashBufferRule
	// violation that got as far as reading a balance.
	EvidenceIdleCash = "idle_cash"
	// EvidenceIdleCashLimit is the mandate's declared ceiling, or "unset" when
	// the mandate declared none.
	EvidenceIdleCashLimit = "max_idle_cash"
)

// CashBufferRule enforces a CashBufferLimit: the portfolio's UNINVESTED cash must
// not exceed the ceiling its mandate declares (#963).
//
// # What it is bought against
//
// Uninvested cash is a permanent performance drag that compounds daily, and it
// is invisible to every other control on this platform: it breaches no other
// mandate, trips no risk limit, and produces no reconciliation break. A fund
// sitting on twice its intended operating balance for a quarter loses real money
// and nothing on this estate would ever say so. internal/treasury measures the
// drag — that is the number; this is the first control with a POLICY on it, and
// the mandate is where the policy has to live, because compliance is the one
// service that must not hold a second copy of a constraint.
//
// # It is the ceiling of the pair BuyingPowerLimit opens
//
// The two are one cash policy. BuyingPowerLimit.min_cash_after is the floor — the
// operating buffer the fund declines to spend below. This is the ceiling, above
// which the same cash stops being prudence and becomes drag. Nothing else about
// them is symmetric, and the asymmetry is the whole design of this function.
//
// # THE SAME BAD NUMBER FAILS THE FLOOR SAFE AND THE CEILING OPEN
//
// Cash on this platform is short every dividend and coupon ever paid to a long
// book (#588): accounting's ledger is authorized to fold corporate actions, its
// fold is written and tested, and nothing publishes an
// accounting.v1.CorporateAction, so none has ever reached the journal the
// balances are computed from.
//
// For a FLOOR, an understated balance refuses orders the fund could afford. That
// is wrong and it is SAFE, and it is why BuyingPowerRule ships while #588 is
// open — it annotates the refusal (attributeCash) and lets the verdict stand.
//
// For a CEILING, an understated balance passes. The cash the missing entries
// hide is precisely the idle cash this rule exists to find, so the defect and
// the subject are the same money: the rule would run, report no breach, and be
// recorded as a control that checked and approved. That is the shape #640 cost
// this repository once already.
//
// SO THIS RULE REFUSES AN UNVOUCHED BALANCE RATHER THAN COMPARING IT, and the
// direction argument is not "assume the cash is higher". It is that the SIGN IS
// NOT KNOWABLE: accounting's foldCorpAct pays quantity x per-unit with the sign
// of the holding, so an unfolded dividend understates a long book's cash and
// OVERSTATES a short book's. A ceiling compared against a number that could be
// wrong either way is not a control, and grossing the balance up by an assumed
// entitlement would be inventing the entry the feed did not deliver.
//
// # IT NEVER GATES AN ORDER
//
// The remedy for excess cash is to BUY something. A ceiling enforced at the
// pre-trade gate would therefore refuse the very orders that discharge it — the
// projected book still carries the pre-existing excess, so the first purchase out
// of an over-cash portfolio would be denied for the condition it is fixing. It
// would also refuse a SELL for raising cash, which traps a fund in a position at
// the moment it most needs to leave: the de-risking trap VenueMarginRule is
// careful to open an exemption for, arriving here as the rule's whole posture
// rather than as an edge case.
//
// So an order-path candidate passes untouched, and the ceiling is enforced only
// on the compliance monitor's passive sweep — the one loop in this estate that
// re-runs every book it holds whether the portfolio traded or not. Idle cash sits
// in the portfolios that are NOT trading, so that is also the only loop that
// samples the right set (the reason internal/treasury's measurement rides it).
//
// # What discharges a breach is a SWEEP, and there is not one yet
//
// A breach here says "this book holds more cash than its mandate permits". The
// action it calls for is to sweep the excess into a money-market vehicle, and
// #963 is explicit that the sweep must travel intent → risk → compliance →
// mandate → OMS like any other order rather than becoming a second way to move
// capital. That engine is BLOCKED and deliberately not built: it needs a forward
// cash ladder over settlement obligations, corporate-action entitlements (#588)
// and margin calls (#408), and a sweep whose ladder cannot see those will sweep
// cash a coupon was about to need — a failed settlement, which on the capital
// path is a counterparty event rather than an inconvenience. Reporting the breach
// is the half that is safe to ship first, and the operator's remedy today is a
// manual investment decision, which is what a passive breach is for.
//
// # A REBALANCE PROPOSAL IS NOT AN ORDER AND IS REFUSED, as three rules already are
//
// optimization.CheckMandate runs this same rule set against a SYNTHETIC book
// built from a weight vector: every position is w x NAV, and Cash, Risk and
// Margin are all nil because a weight vector has none of them. It also carries no
// Order, so the exemption above does not apply and this rule refuses on an
// unknown balance — making a portfolio that declares an idle-cash ceiling
// un-rebalanceable, which is the same inverse-direction error the gate exemption
// exists to avoid.
//
// THAT IS A PRE-EXISTING DEFECT AND NOT A NEW ONE: BuyingPowerRule,
// RiskLimitRule and VenueMarginRule all refuse that book for exactly the same
// reason today, so any portfolio declaring a spending floor, a VaR cap or margin
// trading is already un-rebalanceable. The repair is one discriminator on
// Candidate saying the book is hypothetical, applied to all four rules at once —
// a change to three shipped controls, with its own issue. It is NOT worked
// around here: a fourth private workaround is how the four diverge.
func CashBufferRule(c *Candidate, rule *compliancepb.Rule) *compliancepb.Violation {
	cb := rule.GetCashBuffer()
	if cb == nil {
		return paramsMismatch("cash_buffer")
	}

	// THE ORDER-PATH EXEMPTION COMES AHEAD OF EVERY IDLE-CASH BRANCH BELOW, and
	// behind paramsMismatch alone. An unknown balance, an unvouched one and a
	// ceiling with no number must not refuse an order either — this rule has no
	// verdict to offer about an order, so it must not reach the gate through its
	// failure branches when it does not reach it through its success branch. The
	// one exception is deliberate: params that do not match the declared type is
	// not a statement about cash at all, it is a mandate this engine cannot read,
	// and a portfolio governed by an unreadable mandate is refused everywhere.
	if c.Order != nil {
		return nil
	}

	max := cb.GetMaxIdleCash()
	if max == nil {
		// A CEILING WITH NO NUMBER IS NOT A CEILING, and — unlike the floor one
		// field over — it cannot be defaulted to zero. "Hold no cash at all" is a
		// policy no fund can honour, so reading an unset field that way would turn
		// a half-filled mandate into a permanent breach nobody declared. A
		// misconfigured mandate fails closed under its own severity.
		return &compliancepb.Violation{
			Message: "cash buffer declares no maximum — an idle-cash ceiling with no number cannot be " +
				"defaulted to zero, because that would read as a mandate to hold no cash at all",
			Evidence: map[string]string{EvidenceIdleCashLimit: "unset"},
		}
	}

	if c.Book.Cash == nil {
		return &compliancepb.Violation{
			Message: "idle cash cannot be verified: no cash balance for this portfolio",
			Evidence: map[string]string{
				EvidenceIdleCash:      "unavailable",
				EvidenceIdleCashLimit: ratString(ratFromDecimal(max)),
			},
		}
	}

	// THE NUMERATOR IS CHECKED BEFORE IT IS COMPARED, and the ordering matters
	// more here than anywhere else in this file: a book that is both unvouched and
	// apparently over its ceiling must be reported as UNVERIFIABLE, not as a
	// breach. "Apparently over" is not evidence of over — the missing entries move
	// a short book's cash the other way — and a breach FACT that turns out to
	// rest on a feed gap is how an operations desk learns to discount them.
	if !c.Book.CashCompleteness.Vouched() {
		return unvouchedCeiling(c.Book.Cash, max, c.Book.CashCompleteness)
	}

	cash, err := decutil.MoneyIn(c.Book.Cash, c.Book.BaseCurrency)
	if err != nil {
		// NETTING IS NOT THE FAILURE HERE — RE-SCALING IS. The ceiling is
		// denominated in the book's base currency, so comparing a JPY balance
		// against it silently applies the exchange rate to the LIMIT.
		//
		// The common way to land here is not a mis-booked balance. It is a book
		// the compliance monitor derives BaseCurrency for from its POSITIONS, so a
		// portfolio holding only cash has none at all — which is exactly the
		// portfolio an idle-cash ceiling is written for. That is a defect in the
		// producer and it is refused rather than guessed at here.
		return &compliancepb.Violation{
			Message: "idle cash cannot be verified: " + err.Error(),
			Evidence: map[string]string{
				EvidenceIdleCash:      ratString(ratFromDecimal(c.Book.Cash.GetAmount())),
				EvidenceIdleCashLimit: ratString(ratFromDecimal(max)),
				"currency":            c.Book.Cash.GetCurrencyCode(),
				"base_currency":       c.Book.BaseCurrency,
			},
		}
	}

	ceiling := ratFromDecimal(max)
	if cash.Cmp(ceiling) <= 0 {
		return nil
	}
	v := &compliancepb.Violation{
		Message: "portfolio holds more uninvested cash than its mandate permits",
		Evidence: map[string]string{
			EvidenceIdleCash:      ratString(cash),
			EvidenceIdleCashLimit: ratString(ceiling),
			"currency":            c.Book.BaseCurrency,
		},
	}
	// The balance was vouched for to get here, so this always records "complete" —
	// written anyway, and from the shared constant, so a reader of the breach FACT
	// can tell a drag finding that stands on a whole balance from one that does
	// not WITHOUT knowing which branch produced it.
	v.Evidence[EvidenceBalanceCompleteness] = "complete"
	return v
}

// unvouchedCeiling is the refusal for a balance whose producer did not vouch for
// it, carrying WHICH of the two ways it failed to.
//
// TWO MESSAGES AND NOT ONE, for the same reason attributeCash writes two: nobody
// wired the completeness statement, and a feed that does not exist, send an
// operator to different places. Unstated is a composition-root or an accounting
// old enough to predate the field; incomplete names the missing entry types, and
// the fix for those is #588 rather than anything in this service.
func unvouchedCeiling(cash *commonpb.Money, max *commonpb.Decimal, cc *CashCompleteness) *compliancepb.Violation {
	v := &compliancepb.Violation{
		Evidence: map[string]string{
			EvidenceIdleCash:      ratString(ratFromDecimal(cash.GetAmount())),
			EvidenceIdleCashLimit: ratString(ratFromDecimal(max)),
			"currency":            cash.GetCurrencyCode(),
		},
	}
	if !cc.Stated() {
		v.Evidence[EvidenceBalanceCompleteness] = "unstated"
		v.Message = "idle cash cannot be verified: the balance's producer did not state what the " +
			"balance contains, so a ceiling compared against it could pass on cash nobody counted"
		return v
	}
	v.Evidence[EvidenceBalanceCompleteness] = "incomplete"
	v.Evidence[EvidenceBalanceOmits] = strings.Join(cc.Omitted(), ",")
	v.Message = "idle cash cannot be verified: the book of record says this balance is INCOMPLETE — " +
		"nothing feeds the entry types in balance_omits, and the cash they would add is exactly the " +
		"idle cash this ceiling exists to find"
	return v
}
