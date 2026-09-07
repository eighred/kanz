package compliance

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	// decutil: this package's own tests declare a local `dec(...)` Decimal-literal
	// helper (engine_test.go), so the platform decimal package is aliased here to
	// avoid the name clash rather than renaming a helper used across every test
	// file in the package.
	decutil "github.com/eighred/kanz/internal/dec"
)

// PreTradeGate is the COMP-01c pre-trade enforcement point — the engine wired to
// the seams it needs to decide a live order: the current book, the mandate in
// effect, a classifier, and a decision recorder. The OMS calls it through the
// OMS-01f compliance.Gate interface (an adapter at the composition root maps
// order.v1.SubmitOrder → OrderDelta), so the OMS never imports this package.
//
// The check is hypothetical: it projects the POST-trade book (current holdings
// plus the order delta), evaluates the mandate against that, and rejects on
// BREACH — a WARN passes (advisory). Every TERMINAL decision is recorded
// (COMP-01e) — admitted or refused, evaluated or short-circuited — THROUGH
// WHATEVER RECORDER THE COMPOSITION ROOT WIRED. A transient failure records
// nothing, because nothing was decided.
//
// CORRECTION (2026-08-24). That sentence used to end "so the audit trail is
// complete", and it was FALSE wherever it mattered most: services/oms/cmd/oms
// passed a bare nil for the recorder, so the enforcement point that decides
// whether capital MOVES recorded nothing anywhere, while the post-trade monitor
// recorded every breach it observed. The platform could say a breach had been
// seen and could not say an order had been checked. The completeness of the
// trail is a property of the WIRING, not of this type, so the type now reports
// what it holds (Posture) rather than asserting what its callers did.
type PreTradeGate struct {
	engine     *Engine
	books      BookSource
	mandates   MandateSource
	margins    MarginSource
	classifier Classifier
	recorder   DecisionRecorder
	logger     *slog.Logger

	requireMandate bool
	onUnreadable   func(tenantID, portfolioID string)
	onUngoverned   func(tenantID, portfolioID string, governance Governance)
	onUnpriced     func(portfolioID, instrumentID string, firstForPair bool)
	onUnaccounted  func(tenantID, portfolioID, omits string)

	// warned holds (tenant, portfolio[, instrument]) keys already named in a WARN
	// — say it once, count always. The tenant is IN the key: keyed by portfolio
	// alone, tenant A's "growth" warning silenced tenant B's, so the second
	// tenant's ungoverned book was the one nobody was told about (#243).
	//
	// IT IS A sayOnce AND NO LONGER A BARE MAP because two of these key shapes
	// end in an instrument id, which is a free-form string off SubmitOrder that
	// nothing validates beyond "not empty" — so the map grew by one permanent
	// entry per invented instrument, on the enforcement point that decides
	// whether capital moves, and reported nothing until the pod was OOM-killed
	// (#814). sayOnce carries its own lock, so the check path still never takes
	// the gate's.
	warned sayOnce
}

// PreTradeOption customizes the gate.
type PreTradeOption func(*PreTradeGate)

// WithRequireMandate makes an UNGOVERNED portfolio a REJECTION rather than an
// admission (EXEC-M14).
//
// Deny-by-default is the house rule everywhere else on this platform, and it is not
// the default HERE for one reason: switching it on rejects every order for every
// portfolio nobody has run kanz-mandate for yet. That is a trading outage dressed as
// a control, and it must be a decision somebody makes on purpose — with the list of
// governed portfolios in front of them. The posture is logged, loudly, at startup
// either way.
func WithRequireMandate(require bool) PreTradeOption {
	return func(g *PreTradeGate) { g.requireMandate = require }
}

// WithUnreadableObserver counts orders refused because the portfolio's published
// mandate could not be applied (#619). Nil ⇒ not counted; the refusal itself does
// not depend on this seam.
func WithUnreadableObserver(fn func(tenantID, portfolioID string)) PreTradeOption {
	return func(g *PreTradeGate) { g.onUnreadable = fn }
}

// WithUngovernedObserver is called for EVERY order against a portfolio no mandate
// governs — the composition root wires it to a counter, so "how much of the book is
// ungoverned" is a number on a dashboard rather than a thing nobody has asked.
//
// IT CARRIES THE VERDICT (#926), because "nobody has mandated this portfolio yet"
// and "this portfolio WAS governed and its mandate is not in force" are different
// operator problems that used to increment the same counter. The observer is where
// the distinction reaches a dashboard; without the parameter the registry could
// tell them apart and nothing downstream could.
func WithUngovernedObserver(fn func(tenantID, portfolioID string, governance Governance)) PreTradeOption {
	return func(g *PreTradeGate) { g.onUngoverned = fn }
}

// WithUnpricedObserver is called for EVERY order the gate refuses for lack of a
// usable price — the composition root wires it to a counter (COMP-M1), the same
// way WithUngovernedObserver makes the ungoverned gap a number on a dashboard
// instead of a thing nobody has asked about.
//
// firstForPair CARRIES THIS GATE'S DEDUP VERDICT OUT TO THE CALLER, and it is
// here because the OMS observer wanted to LOG as well as count (#883). It is
// true exactly when the gate is also warning about this refusal — the first
// sighting of (tenant, portfolio, instrument) in the say-once ledger — and false
// on every repeat.
//
// THE OBSERVER STILL FIRES ON EVERY ORDER. The bool gates what a caller CHOOSES
// to say, never whether it is told: a counter behind this seam is a RATE, and a
// rate incremented only on first sightings is a count of distinct instruments
// wearing a rate's name.
//
// IT IS PASSED RATHER THAN RECOMPUTED because a second ledger in the caller
// would be a second implementation of the thing #814 had just finished
// deduplicating — and it would be a WORSE one: this ledger's key is
// (tenant, portfolio, instrument) while the observer is handed no tenant at all,
// so a caller-side ledger would key on (portfolio, instrument) and let one
// tenant's first warning silence another's. That is #243, one seam out.
func WithUnpricedObserver(fn func(portfolioID, instrumentID string, firstForPair bool)) PreTradeOption {
	return func(g *PreTradeGate) { g.onUnpriced = fn }
}

// WithUnaccountedObserver is called for EVERY order the gate ADMITS against a cash
// balance whose producer did not vouch for it — incomplete (something named as
// missing) or unstated (nobody said). The composition root wires it to a counter,
// the same way WithUngovernedObserver makes the ungoverned gap a number on a
// dashboard rather than a thing nobody has asked about.
//
// ON THE ADMIT PATH ONLY, and that is the whole point (#671). A REFUSAL already
// carries the statement as evidence — attributeCash in rules.go puts
// balance_completeness and balance_omits on the violation, so a reader can tell
// "you are out of money" from "we never counted your dividend". An ADMISSION
// carried nothing at all, and it is the more dangerous direction: foldCorpAct
// pays quantity x per-unit with the SIGN of the holding, so an unfolded dividend
// OVERSTATES a short book's cash. The order the gate lets through on that number
// is one the fund may not be able to pay for, and nothing recorded that the
// number was incomplete when it was let through.
func WithUnaccountedObserver(fn func(tenantID, portfolioID, omits string)) PreTradeOption {
	return func(g *PreTradeGate) { g.onUnaccounted = fn }
}

// WithMarginSource gives the gate the exchange's own margin state for the venue
// account each order spends from (#408, control 3).
//
// IT IS AN OPTION AND NOT A CONSTRUCTOR ARGUMENT because a deployment that does
// not trade on margin needs nothing here, and the rule that reads it only runs
// when a mandate DECLARES margin trading. Leaving it unset is therefore safe and
// is the default; what it is NOT is quiet — a portfolio whose mandate declares
// margin on a gate with no source is REFUSED with a named reason, never
// admitted.
func WithMarginSource(m MarginSource) PreTradeOption {
	return func(g *PreTradeGate) { g.margins = m }
}

// BookSource loads the current book for a portfolio — the holdings the order is
// projected onto. In production this reads the risk-engine/position read model;
// tests supply an in-memory source.
type BookSource interface {
	Book(ctx context.Context, portfolioID string) (*Book, error)
}

// MandateSource resolves the mandate version in effect for one TENANT'S
// portfolio at a point in time (the COMP-01f point-in-time resolution).
// ok=false ⇒ no mandate governs it, which admits the order (no constraints
// declared).
//
// tenantID is in the signature, and not merely in the implementation's map key,
// so that no caller can reach a mandate without saying whose. portfolio_id is a
// caller-chosen string off SubmitOrder; keyed by it alone, tenant B's "growth"
// order was evaluated against tenant A's "growth" limits (#243). A composite key
// behind an interface that cannot carry a tenant would have left every caller
// unable to supply one, which is not a fix.
//
// An error wrapping ErrMandateTenantUnresolved is TERMINAL — the source could
// not say whose rules apply, so none were evaluated and the order must be
// refused. Any other error is transient and the caller should retry.
type MandateSource interface {
	Mandate(ctx context.Context, tenantID, portfolioID string, asOf time.Time) (*compliancepb.Mandate, Governance, error)
}

// OrderDelta is the order's effect on a book, in terms compliance understands —
// the neutral shape the OMS adapter maps order.v1.SubmitOrder into, so this
// package stays decoupled from the order schema. SignedQuantity is +buy/−sell;
// Price values the order (limit price, or a reference/last price for a market
// order — the adapter resolves it).
type OrderDelta struct {
	// TenantID is WHOSE order this is — the envelope's tenant, which the
	// api-gateway stamps from the authenticated principal and bus.Validate
	// requires to be non-empty on the live path. It is NOT the OMS's own
	// configured tenant: the shipped OMS serves __system__, and scoping the
	// mandate lookup to that would ask for the platform's mandate rather than
	// the customer's.
	TenantID       string
	PortfolioID    string
	InstrumentID   string
	SignedQuantity *commonpb.Decimal
	Price          *commonpb.Decimal
	Currency       string
	// Venue is the MIC this order will execute at, off SubmitOrder.venue. Empty
	// for an order that names none.
	//
	// IT DECIDES WHOSE COLLATERAL IS AT STAKE (#408, control 3). An exchange
	// margins and LIQUIDATES per account, and which account this order spends from
	// is (tenant, portfolio, venue) resolved through the deploy-time bindings. No
	// other field on this delta can identify it, and a margin control that could
	// not identify it would be checking some other account's numbers.
	Venue string
	// OrderID and Issuer correlate the decision in the audit log (COMP-01e).
	OrderID string
	Issuer  string
	AsOf    time.Time

	// WorkedSlices is how many child orders this one decision authorises, when
	// the order is worked as a schedule rather than sent whole (#435). Zero for
	// an ordinary order.
	//
	// IT CHANGES NOTHING ABOUT THE EVALUATION and everything about the record.
	// The gate checks the full notional either way — that is the whole point of
	// checking a parent once rather than each slice against a book that has not
	// moved (#483). What it affects is what the audit log says afterwards: the
	// fills land on N order ids, and only the parent has a decision, so the
	// record has to say it authorised more orders than the one it names.
	WorkedSlices uint32
}

// Decision is the gate verdict: Allowed plus the full ComplianceResult (the
// violated rules and evidence) so the caller can map a rejection to an
// OrderRejected reason.
type Decision struct {
	Allowed bool
	Result  *compliancepb.ComplianceResult

	// Ungoverned means NO MANDATE EXISTS for this portfolio — nobody has decided what
	// governs it. That is NOT the same as a mandate with no rules, which is somebody
	// deciding, explicitly, to constrain nothing. The old code collapsed the two into
	// one silent `return Allowed:true`, so "we forgot to put fund X under mandate"
	// and "fund X passed compliance" were the same observable event (EXEC-M14).
	Ungoverned bool

	// Unpriced means the order could not be valued — Price was nil, zero, or
	// negative — so NO RULE WAS EVALUATED against it. That is NOT a rule breach:
	// nothing was checked, because nothing could be checked. The old code
	// collapsed the two into one silent projection: an unpriced order projects
	// at zero market value, and heldPositions (rules.go) treats a zero-value
	// position as flat and skips it — so "we cannot value this order" and "this
	// order passed compliance" were the same observable event, and worse, a
	// market order touching an already-breaching position ERASED that breach
	// from the check (COMP-M1, the same EXEC-M14 collapse one door over). A
	// reviewer reading Unpriced knows to go wire a reference-price source, not
	// to go look for the rule that fired.
	Unpriced bool

	// Unvaluable means the order HAD a usable price but its notional
	// (quantity × price) could not be represented as a Decimal at all, so
	// again NO RULE WAS EVALUATED. It is deliberately not Unpriced: the price
	// was fine, and telling an operator to go wire a price source would send
	// them after a problem that does not exist. It is deliberately not a rule
	// breach either — nothing was checked. The alternative is what this
	// replaced: an int64 multiply that wrapped a billion-dollar notional into
	// a small number and ADMITTED the order.
	Unvaluable bool

	// Unaccounted means the order was ADMITTED against a cash balance its producer
	// did not vouch for — incomplete, or unstated (#671).
	//
	// IT IS NOT A MEMBER OF THE "NOTHING WAS CHECKED" FAMILY above, and the
	// difference is the reason it has its own field rather than joining them.
	// Ungoverned, Unpriced, Unvaluable and Unscoped all mean NO RULE WAS
	// EVALUATED. Here every rule ran and every rule passed — against an INPUT that
	// is missing entries the book of record knows how to fold and this deployment
	// does not produce (#588). The verdict is real; the number it was reached on
	// is not whole.
	//
	// It does not change the verdict, and must not: corporate_action is unproduced
	// in EVERY deployment today, so refusing on it would be a total trading outage
	// rather than a control (#614 pins both directions).
	Unaccounted bool

	// Unscoped means the mandate source could not say WHOSE mandate governs this
	// portfolio, so — a third time — NO RULE WAS EVALUATED. Either the order
	// reached the gate with no tenant, or the lookup landed on the shared
	// __system__ bucket where more than one tenant has published a mandate for
	// this portfolio name.
	//
	// It is refused REGARDLESS of requireMandate, unlike Ungoverned. Ungoverned
	// is a policy question — "nobody has written a mandate yet" is a state an
	// operator may knowingly trade through. This is not: a mandate exists, and
	// the platform cannot tell whether it is this order's. Admitting on that is
	// exactly #243 — tenant B's order cleared against tenant A's concentration
	// limits — and refusing is the only answer that is not a guess.
	Unscoped bool

	// Unreadable means a mandate for this portfolio WAS PUBLISHED and could not be
	// applied, so — a fourth time — NO RULE WAS EVALUATED.
	//
	// It is refused REGARDLESS of requireMandate, for Unscoped's reason and one
	// sharper. Ungoverned is a policy question: "nobody has written a mandate yet"
	// is a state an operator may knowingly trade through. This is not. A mandate
	// exists, the platform cannot read it, and the mandate stream is COMPACTED —
	// the message that failed is the last one on that portfolio's subject, so every
	// consumer that boots re-reads it and fails identically. The portfolio is not
	// waiting to be governed, it is un-governable until someone republishes, and
	// admitting on that is a guess about rules nobody has read (#619).
	Unreadable bool

	// Unconstrained means a mandate EXISTS for this portfolio and carries no
	// rules — somebody decided, explicitly, to constrain nothing. So, a fifth
	// time, NO RULE WAS EVALUATED, and the order is ADMITTED.
	//
	// IT IS NOT Ungoverned, AND THE DISTINCTION IS THE ONE EXEC-M14 EXISTS FOR.
	// "Nobody has decided what governs this portfolio" is a gap; "somebody
	// decided nothing constrains it" is a decision. Collapsing them is what made
	// the gap silent, and this flag is what keeps them apart on the far side of
	// the gate as well as inside it.
	//
	// It exists because the audit record needs it (#797). Without a flag this
	// path reached the decision recorder as a bare Decision and would have been
	// filed under whatever the reason switch's last arm guessed — an audit trail
	// that answers confidently and wrongly, which is worse than one that says
	// UNCLASSIFIED.
	Unconstrained bool
}

// NewPreTradeGate wires the gate. engine defaults to NewEngine(nil).
//
// A NIL SEAM IS STILL ACCEPTED AND IS NO LONGER INVISIBLE. A nil recorder
// disables decision logging and a nil classifier passes every sector and issuer
// limit (#640); both remain legal here, because this constructor cannot know
// which controls a deployment is entitled to. What changed is that the gate
// REPORTS them — see Posture, and #643 for the 1,400-line composition root in
// which those two nils sat as bare arguments on one line for months.
func NewPreTradeGate(engine *Engine, books BookSource, mandates MandateSource, classifier Classifier, recorder DecisionRecorder, logger *slog.Logger, opts ...PreTradeOption) *PreTradeGate {
	if engine == nil {
		engine = NewEngine(nil)
	}
	if logger == nil {
		logger = slog.Default()
	}
	g := &PreTradeGate{
		engine: engine, books: books, mandates: mandates,
		classifier: classifier, recorder: recorder, logger: logger,
	}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// noteUngoverned makes the ungoverned case AUDIBLE.
//
// It was silent: the gate returned Allowed:true and said nothing, so an order for a
// portfolio nobody had put under mandate looked exactly like an order that passed
// compliance. No log, no metric, no difference.
//
// The counter fires on EVERY such order (that is the number that belongs on a
// dashboard). The WARN fires ONCE PER PORTFOLIO — loud enough to be seen in a log,
// quiet enough that it does not drown the log for a fund that trades all day.
func (g *PreTradeGate) noteUngoverned(tenantID, portfolioID string, governance Governance) {
	if g.onUngoverned != nil {
		g.onUngoverned(tenantID, portfolioID, governance)
	}
	// THE VERDICT IS PART OF THE ONCE-KEY (#926). A portfolio first seen as
	// NeverMandated and later LAPSING is a new fact and must warn again; keying
	// on the pair alone would silence the transition — which is the one moment an
	// operator most needs to hear about.
	if !g.firstTime("ungoverned:" + governance.String() + ":" + tenantID + ":" + portfolioID) {
		return
	}
	// THE FIX DEPENDS ON WHICH STATE THIS IS (#926), so the sentence does too.
	// Telling an operator to "put it under mandate" when a mandate already exists
	// and is merely not in force sends them to write a second one — which is not
	// the repair and may not even be possible if the versions are future-dated.
	fix := "put it under mandate with `kanz-mandate --tenant " + tenantID + "`"
	headline := "UNGOVERNED: no mandate has ever been published for this portfolio"
	if governance == MandateLapsed {
		headline = "UNGOVERNED: this portfolio HAS a mandate and none of its versions is in force right now"
		fix = "check the effective dates on its published versions — every surviving version being " +
			"FUTURE-DATED is the #916 shape, where scheduling a change evicted the version in force " +
			"from the compacted stream. Re-publish the version that should be in effect"
	}
	if g.requireMandate {
		g.logger.Warn("REFUSING orders: "+headline,
			"tenant_id", tenantID,
			"portfolio_id", portfolioID,
			"governance", governance.String(),
			"fix", fix+", or unset OMS_REQUIRE_MANDATE")
		return
	}
	g.logger.Warn(headline+" — its orders are being ADMITTED WITH NO COMPLIANCE CONSTRAINTS",
		"tenant_id", tenantID,
		"portfolio_id", portfolioID,
		"governance", governance.String(),
		"fix", fix+", or set OMS_REQUIRE_MANDATE=true to refuse instead")
}

// noteUnscoped makes the unresolvable-tenant refusal audible. It is separate
// from noteUngoverned because the operator action is different: nothing needs
// writing, something needs DISAMBIGUATING — the registry's error names the
// tenants involved. Once per (tenant, portfolio), same as the rest.
func (g *PreTradeGate) noteUnscoped(tenantID, portfolioID string, err error) {
	if !g.firstTime("unscoped:" + tenantID + ":" + portfolioID) {
		return
	}
	g.logger.Warn("REFUSING order: cannot determine WHOSE mandate governs this portfolio — no rule was evaluated",
		"tenant_id", tenantID,
		"portfolio_id", portfolioID,
		"err", err,
		"fix", "two tenants share this portfolio name, or the lookup lost its tenant; see the error")
}

// firstTime reports whether key has not been warned about yet, and records it.
// noteUnreadable makes a broken mandate audible, once per portfolio.
//
// It says REFUSING rather than warning about a gap, because unlike Ungoverned this
// outcome does not depend on posture and the operator cannot choose to trade
// through it. The fix is a republish, and the message names it: the portfolio is
// stuck until the compacted subject carries a mandate that parses.
func (g *PreTradeGate) noteUnreadable(tenantID, portfolioID string, err error) {
	if g.onUnreadable != nil {
		g.onUnreadable(tenantID, portfolioID)
	}
	if !g.firstTime("unreadable:" + tenantID + ":" + portfolioID) {
		return
	}
	g.logger.Error("REFUSING orders: this portfolio has a published mandate that cannot be read",
		"tenant_id", tenantID,
		"portfolio_id", portfolioID,
		"err", err,
		"fix", "republish the mandate — the stream is compacted, so the broken message is the only "+
			"one this portfolio's subject serves until it is replaced")
}

func (g *PreTradeGate) firstTime(key string) bool { return g.warned.first(key) }

// firstTimeAbout is firstTime for the two keys that end in an instrument id —
// the ONE thing on this path a caller supplies and nothing bounds. It is a
// separate method rather than a flag so a new warning cannot join the unbounded
// class by accident: the caller-supplied component has to be handed over
// separately to get there at all. See sayOnce for what remembering it costs and
// what forgetting it costs.
func (g *PreTradeGate) firstTimeAbout(key, instrumentID string) bool {
	return g.warned.firstAbout(key, instrumentID)
}

// noteUnaccounted makes an ADMISSION against an unvouched-for balance AUDIBLE,
// mirroring noteUngoverned.
//
// It was silent: the gate returned Allowed and said nothing, so an order that
// passed the buying-power check against a balance missing every dividend and
// coupon looked exactly like one that passed against a complete balance. No log,
// no metric, no difference — the same collapse EXEC-M14 named, on the input to a
// control rather than on the control itself.
//
// The counter fires on EVERY such admission (that is the number that belongs on a
// dashboard: how much of the day's flow was cleared against a number nobody
// vouched for). The WARN fires ONCE PER PORTFOLIO — loud enough to be seen in a
// log, quiet enough that it does not drown the log for a fund that trades all day.
func (g *PreTradeGate) noteUnaccounted(tenantID, portfolioID string, cc *CashCompleteness) {
	omits := strings.Join(cc.Omitted(), ",")
	if g.onUnaccounted != nil {
		g.onUnaccounted(tenantID, portfolioID, omits)
	}
	if !g.firstTime("unaccounted:" + tenantID + ":" + portfolioID) {
		return
	}
	if cc.Stated() {
		g.logger.Warn("ADMITTING orders against an incomplete cash balance",
			"tenant_id", tenantID,
			"portfolio_id", portfolioID,
			"omits", omits,
			"consequence", "buying power does not account for these entry types; on a SHORT book the "+
				"balance is OVERSTATED, so an order the fund cannot pay for can be admitted")
		return
	}
	g.logger.Warn("ADMITTING orders against a cash balance nobody vouched for",
		"tenant_id", tenantID,
		"portfolio_id", portfolioID,
		"consequence", "the producer stated no completeness, so whether buying power is whole is unknown")
}

// noteUnpriced makes the unpriced-refusal case AUDIBLE, mirroring noteUngoverned
// (EXEC-M14): an order the gate cannot value must not look like an order that
// passed compliance in the logs — it was never evaluated at all.
//
// The counter fires on EVERY such order (that is the number that belongs on a
// dashboard: how often a market/stop order is going unpriced). The WARN fires
// once per (portfolio, instrument) pair — loud enough to be seen, quiet enough
// not to drown the log for an instrument that trades all day with no reference
// price wired.
//
// THE LEDGER IS CONSULTED ONCE AND THE VERDICT IS SHARED, which is what makes
// this gate and its observer hold ONE dedup policy rather than two (#883). The
// observer used to be called before the ledger was touched and could therefore
// only guess; it now receives the same bool this method's own WARN is gated on,
// so a caller that logs says its line on exactly the orders this one does.
func (g *PreTradeGate) noteUnpriced(tenantID, portfolioID, instrumentID string) {
	// Ordered deliberately: firstTimeAbout MUTATES the ledger, so it must be
	// called exactly once per refusal and its answer handed on. Calling it again
	// below to re-ask would return false and silence this gate's own warning.
	first := g.firstTimeAbout("unpriced:"+tenantID+":"+portfolioID, instrumentID)
	if g.onUnpriced != nil {
		g.onUnpriced(portfolioID, instrumentID, first)
	}
	if !first {
		return
	}
	g.logger.Warn("REFUSING order: no usable price to evaluate compliance against",
		"tenant_id", tenantID,
		"portfolio_id", portfolioID,
		"instrument_id", instrumentID,
		"fix", "wire a reference-price source for market/stop orders (COMP-M2)")
}

// noteUnvaluable makes the unvaluable-notional refusal audible, mirroring
// noteUnpriced: once per (portfolio, instrument), loud enough to be seen. This
// should never fire in practice — it takes a notional beyond any representable
// Decimal — so if it does, somebody needs to look at the order, not the price
// feed.
func (g *PreTradeGate) noteUnvaluable(tenantID, portfolioID, instrumentID string) {
	if !g.firstTimeAbout("unvaluable:"+tenantID+":"+portfolioID, instrumentID) {
		return
	}
	g.logger.Warn("REFUSING order: its notional (quantity × price) cannot be represented — the order was NOT evaluated",
		"tenant_id", tenantID,
		"portfolio_id", portfolioID,
		"instrument_id", instrumentID,
		"fix", "check the submitted quantity; the price is not the problem")
}

// maxDecimalExponent bounds the exponent of any Decimal entering the rules
// engine.
//
// It is a SAFETY limit, not a statement about what money means on this platform.
// Real financial values keep |exponent| well under 30 — the smallest crypto
// prices sit near 1e-12, the largest plausible notionals near 1e13 — so nothing
// legitimate is within thirty orders of magnitude of this bound, and it cannot
// refuse a real order. A tighter, meaningful domain would be a POLICY bound, and
// setting policy wrong refuses live orders, which is its own kind of incident.
//
// The number is set by the system's own arithmetic rather than by entry values:
// mulDecimal sums exponents and rescales upward, so values bounded at ±64 on
// entry reach at most ≈ ±168 internally before ratFromDecimal sees them, and
// 10^168 is instant. A bound chosen only against entry values would be wrong.
//
// Why this exists at all: engine.go's ratFromDecimal materialises
// 10^abs(exponent) with no bound, and Decimal.exponent is an unvalidated wire
// field on an order that reaches this gate BEFORE Accept validates it. An order
// carrying {0, 2000000000} did not return in 5 seconds.
const maxDecimalExponent = 64

// decimalInDomain reports whether a Decimal can be safely computed with.
// nil is in-domain — absent is not out-of-range, and the unpriced check owns it.
func decimalInDomain(d *commonpb.Decimal) bool {
	if d == nil {
		return true
	}
	exp := d.GetExponent()
	return exp >= -maxDecimalExponent && exp <= maxDecimalExponent
}

// deltaInDomain reports whether every Decimal on an incoming order is in-domain.
func deltaInDomain(d OrderDelta) bool {
	return decimalInDomain(d.SignedQuantity) && decimalInDomain(d.Price)
}

// bookInDomain reports whether every Decimal on a loaded book is in-domain.
// The book is built from venue fills, so it is externally influenced too;
// validating only the order would leave the same hang reachable through a
// corrupted position.
func bookInDomain(b *Book) bool {
	if b == nil {
		return true
	}
	if b.NAV != nil && !decimalInDomain(b.NAV.GetAmount()) {
		return false
	}
	for i := range b.Positions {
		if !decimalInDomain(b.Positions[i].Quantity) {
			return false
		}
		if b.Positions[i].MarketValue != nil && !decimalInDomain(b.Positions[i].MarketValue.GetAmount()) {
			return false
		}
	}
	return true
}

// Evaluate runs the pre-trade check for one order. A returned error is TRANSIENT
// (book/mandate load failure) and the caller should retry; a clean Decision with
// Allowed=false is a terminal compliance rejection. An order against a portfolio
// with no mandate is allowed.
//
// # IT IS A RECORDING WRAPPER, AND THAT SHAPE IS THE FIX (#797)
//
// The decision logic used to be this function, with ten terminal returns and
// exactly ONE g.record among them — on the full-evaluation path. Nine paths
// recorded nothing, and TWO of those returned Allowed: true: the ungoverned
// portfolio and the mandate that constrains nothing. For an order admitted that
// way the platform could not answer AGENTS.md's attributability requirement —
// "which mandate permitted it" — because nothing anywhere said a compliance
// decision had been made at all. The ungoverned COUNTER gives an aggregate; a
// reviewer asking about THIS order got silence.
//
// Adding g.record to nine call sites would have fixed today's nine and nothing
// about the tenth. So the recording moved OUT: decide() owns every return, this
// function owns the only record, and a new short-circuit cannot skip it without
// being written in the wrong function. test/arch keeps that true.
//
// A TRANSIENT ERROR RECORDS NOTHING, deliberately. Nothing was decided — the
// book or the mandate could not be read — and a record saying otherwise would
// put a decision in the audit trail that no enforcement point ever made. The
// order is redelivered and evaluated again.
func (g *PreTradeGate) Evaluate(ctx context.Context, d OrderDelta) (Decision, error) {
	dec, err := g.decide(ctx, d)
	if err != nil {
		return dec, err
	}
	g.record(ctx, g.decisionRecord(d, dec))
	return dec, nil
}

// decisionRecord renders one terminal decision for the audit trail, SYNTHESISING
// the result for the paths that short-circuited before any rule ran.
//
// A nil Result is the marker: the engine sets one on every path that actually
// evaluated a mandate, so its absence means no rule was applied. The record then
// carries WHY, on NotEvaluated, rather than an invented Violation — a Violation
// says "a rule did not pass", and claiming one fired would put a rule id in the
// audit trail that never ran. Violations stays empty and the reason is its own
// field.
//
// THE STATUS IS THE VERDICT THE ORDER GOT, not a verdict about the rules. BREACH
// where the gate refused, WARN where it admitted without evaluating anything —
// advisory is exactly what an admission nobody checked is. Neither is
// UNSPECIFIED: that is the zero value, and "nobody set it" and "we could not
// evaluate" must not be the same observable state.
func (g *PreTradeGate) decisionRecord(d OrderDelta, dec Decision) DecisionRecord {
	rec := DecisionRecord{
		Phase:        PhasePreTrade,
		TenantID:     d.TenantID,
		Result:       dec.Result,
		OrderID:      d.OrderID,
		Issuer:       d.Issuer,
		Allowed:      dec.Allowed,
		WorkedSlices: d.WorkedSlices,
	}
	if rec.Result != nil {
		return rec
	}
	rec.NotEvaluated = notEvaluatedCode(dec)
	status := compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH
	if dec.Allowed {
		status = compliancepb.ComplianceStatus_COMPLIANCE_STATUS_WARN
	}
	rec.Result = &compliancepb.ComplianceResult{
		Status:      status,
		PortfolioId: d.PortfolioID,
		EvaluatedAt: timestamppb.New(d.AsOf),
	}
	return rec
}

// NotEvaluatedUnclassified is the code recorded when a decision reached the
// audit trail with no rule evaluated and no flag saying why.
//
// IT IS DELIBERATELY NOT A SILENT DEFAULT. A switch whose last arm guessed the
// most likely reason would file a new short-circuit under an existing one, and
// the record would be wrong in the direction nobody checks — an audit trail that
// answers confidently is worse than one that says it does not know. This value
// is greppable, and test/arch fails when a Decision flag exists that the switch
// below does not name.
const NotEvaluatedUnclassified = "UNCLASSIFIED"

// THE CODES A REFUSAL IS FILED UNDER, AND THEY ARE EXPORTED BECAUSE THERE WAS A
// SECOND COPY OF THEM (#803).
//
// notEvaluatedCode below writes the decision RECORD's code. The OMS gate wrote
// the code the CLIENT sees, as its own chain of string literals, and the
// optimization bridge wrote a third set of strings for the same flags. Three
// hand-maintained copies of one table, and the copies were how a row went
// missing: Unreadable was mapped here and nowhere else, so an order refused
// because its mandate could not be decoded reached the client and the audit
// trail as "mandate breach" — a rule breach that never fired, on an order
// against which no rule was evaluated.
//
// One table. A sixth flag added to the family now has exactly one place to be
// named, and test/arch/every_decision_consumer_handles_every_refusal_test.go
// fails until every consumer names it.
const (
	// CodeNotionalUnrepresentable: the price was fine and quantity × price is
	// beyond any Decimal, so no rule was evaluated. The ORDER SIZE is what to
	// look at — deliberately not PRICE_UNAVAILABLE, which would send an operator
	// to wire a price source that is already wired.
	CodeNotionalUnrepresentable = "NOTIONAL_UNREPRESENTABLE"
	// CodeMandateTenantUnresolved: a mandate exists and the platform cannot say
	// whose it is. The action is to disambiguate the tenants, not to write a
	// mandate.
	CodeMandateTenantUnresolved = "MANDATE_TENANT_UNRESOLVED"
	// CodeMandateUnreadable: a mandate for this portfolio WAS PUBLISHED and could
	// not be applied. The action is to REPUBLISH it — and only that: the mandate
	// stream is compacted, so the undecodable message is the last one on the
	// subject and every consumer that boots re-reads it forever.
	CodeMandateUnreadable = "MANDATE_UNREADABLE"
	// CodeMandateMissing: nobody has written a mandate for this portfolio. The
	// action is to write one.
	CodeMandateMissing = "MANDATE_MISSING"
	// CodePriceUnavailable: the order could not be valued, so no rule was
	// evaluated. The action is to wire a reference-price source.
	CodePriceUnavailable = "PRICE_UNAVAILABLE"
	// CodeMandateHasNoRules: a mandate exists and constrains nothing — somebody
	// decided that, explicitly. The order is ADMITTED; this names why the record
	// shows no rule having run.
	CodeMandateHasNoRules = "MANDATE_HAS_NO_RULES"
)

// notEvaluatedCode names why no rule ran, from the flags the decision already
// carries. Every arm corresponds to one short-circuit in decide().
func notEvaluatedCode(dec Decision) string {
	switch {
	case dec.Unvaluable:
		return CodeNotionalUnrepresentable
	case dec.Unscoped:
		return CodeMandateTenantUnresolved
	case dec.Unreadable:
		return CodeMandateUnreadable
	case dec.Ungoverned:
		return CodeMandateMissing
	case dec.Unpriced:
		return CodePriceUnavailable
	case dec.Unconstrained:
		return CodeMandateHasNoRules
	default:
		return NotEvaluatedUnclassified
	}
}

// decide is the pre-trade decision itself. Every terminal return lives here; the
// record lives in Evaluate. See Evaluate for why they are separated.
func (g *PreTradeGate) decide(ctx context.Context, d OrderDelta) (Decision, error) {
	// INPUT VALIDATION, BEFORE ANYTHING COMPUTES WITH THESE NUMBERS — including
	// before the mandate lookup, so an out-of-domain order against an UNGOVERNED
	// portfolio is refused rather than admitted. That is deliberate: a malformed
	// exponent is not a compliance question.
	if !deltaInDomain(d) {
		g.noteUnvaluable(d.TenantID, d.PortfolioID, d.InstrumentID)
		return Decision{Allowed: false, Unvaluable: true}, nil
	}
	mandate, governance, err := g.mandates.Mandate(ctx, d.TenantID, d.PortfolioID, d.AsOf)
	if err != nil {
		// TERMINAL vs TRANSIENT, and getting this backwards is why the
		// distinction is spelled out. A transient load failure is returned so the
		// command is redelivered. ErrMandateTenantUnresolved never resolves on a
		// retry — the ambiguity is in the mandate data, not in the read — so
		// returning it would redeliver the order forever while the trader waits.
		// Refuse it, once, under its own flag.
		if errors.Is(err, ErrMandateTenantUnresolved) {
			g.noteUnscoped(d.TenantID, d.PortfolioID, err)
			return Decision{Allowed: false, Unscoped: true}, nil
		}
		// ALSO TERMINAL, and for a sharper reason than the one above: the mandate
		// stream is compacted, so the message that failed to apply is the last one
		// this portfolio's subject will serve. A redelivery replays the identical
		// bytes. Refuse it once, under its own flag, rather than redeliver the order
		// forever against a mandate that will never parse.
		if errors.Is(err, ErrMandateUnreadable) {
			g.noteUnreadable(d.TenantID, d.PortfolioID, err)
			return Decision{Allowed: false, Unreadable: true}, nil
		}
		return Decision{}, err
	}
	// TWO DIFFERENT STATES, and collapsing them is what made this silent.
	//
	//   no mandate at all      → NOBODY HAS DECIDED what governs this portfolio.
	//   a mandate, zero rules  → somebody decided, explicitly, to constrain nothing.
	//
	// The second is a choice and needs no noise. The first is a GAP, and it must not
	// be indistinguishable from passing compliance (EXEC-M14).
	if governance.NoMandate() {
		g.noteUngoverned(d.TenantID, d.PortfolioID, governance)
		if g.requireMandate {
			return Decision{Allowed: false, Ungoverned: true}, nil
		}
		return Decision{Allowed: true, Ungoverned: true}, nil
	}
	if len(mandate.GetRules()) == 0 {
		// GOVERNED BY A MANDATE THAT CONSTRAINS NOTHING — a choice somebody made,
		// and distinct from the ungoverned gap above. Unconstrained is what carries
		// that distinction into the audit record: without a flag this path would
		// reach notEvaluatedCode as a bare Decision and be filed under whatever the
		// switch's last arm guessed.
		return Decision{Allowed: true, Unconstrained: true}, nil
	}
	// An order the gate cannot value must not reach project()/heldPositions: a
	// nil, zero, or negative price projects at zero market value, and
	// heldPositions (rules.go) treats a zero-value position as FLAT — the exact
	// EXEC-M14 collapse above, one door over (COMP-M1). Refused here, before any
	// I/O, so a market/stop order can never silently erase an existing breach on
	// the same instrument from the check.
	if !decutil.IsPositive(d.Price) {
		g.noteUnpriced(d.TenantID, d.PortfolioID, d.InstrumentID)
		return Decision{Allowed: false, Unpriced: true}, nil
	}
	book, err := g.books.Book(ctx, d.PortfolioID)
	if err != nil {
		return Decision{}, err
	}
	if !bookInDomain(book) {
		g.noteUnvaluable(d.TenantID, d.PortfolioID, d.InstrumentID)
		return Decision{Allowed: false, Unvaluable: true}, nil
	}
	// The price was fine; the VALUATION did not fit. Refusing under its own
	// flag keeps the two apart: Unpriced sends an operator to go wire a price
	// source, which would be a wild goose chase here.
	proj, ok := project(book, d)
	if !ok {
		g.noteUnvaluable(d.TenantID, d.PortfolioID, d.InstrumentID)
		return Decision{Allowed: false, Unvaluable: true}, nil
	}
	cand := &Candidate{
		Book:       proj,
		Classifier: g.classifier,
		AsOf:       d.AsOf,
		// ClassifyAsOf IS DELIBERATELY UNSET, and this gate is why the split was safe
		// to make (#930). d.AsOf here is the gate's OWN clock at admission
		// (services/oms/internal/compliance/comp01.go stamps it from g.now()), so the
		// classifier question is already a current one and the fallback to AsOf is
		// exact. It is the paths whose AsOf is an OBSERVATION rather than now — the
		// post-trade monitor replaying a month-old position FACT — that have to say
		// which instant their reference data is read at. Should d.AsOf ever become an
		// observation, this line is the one to change.
		// THE ORDER, ALONGSIDE THE PROJECTION AND NOT INSTEAD OF IT. Every existing
		// rule keeps reading the projected book; this exists so a margin control can
		// tell an order that TAKES a position from one that closes it, which the
		// projection alone cannot express.
		Order: &CandidateOrder{
			InstrumentID:   d.InstrumentID,
			SignedQuantity: d.SignedQuantity,
			Venue:          d.Venue,
		},
	}
	// MARGIN IS RESOLVED PER VENUE, LAZILY, AND SCOPED TO THIS ORDER'S TENANT
	// (#408, control 3). Bound as a closure for the reason Book.Risk is: the
	// several ways to be UNKNOWN must stay one answer, and a value resolved
	// eagerly would be resolved for every order under every mandate — including
	// the portfolios that do not trade on margin at all.
	//
	// A nil source leaves Candidate.Margin nil, which VenueMarginRule refuses on
	// rather than passes.
	if g.margins != nil {
		src, tenant, pf := g.margins, d.TenantID, d.PortfolioID
		cand.Margin = func(venue string) (MarginState, bool) { return src.Margin(tenant, pf, venue) }
	}
	res := g.engine.Evaluate(ctx, cand, mandate)
	allowed := res.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH

	// THE ADMIT PATH SAYS WHAT IT COULD NOT ACCOUNT FOR (#671). Only on an
	// admission: a refusal already carries balance_completeness and balance_omits
	// as evidence (attributeCash), and firing here too would double-report the
	// same fact in two shapes.
	unaccounted := allowed && !book.CashCompleteness.Vouched()
	if unaccounted {
		g.noteUnaccounted(d.TenantID, d.PortfolioID, book.CashCompleteness)
	}
	return Decision{Allowed: allowed, Result: res, Unaccounted: unaccounted}, nil
}

// record sends the decision to the recorder, best-effort: an audit-sink outage
// must not become a trading outage (the AUTH-01d stance).
func (g *PreTradeGate) record(ctx context.Context, rec DecisionRecord) {
	if g.recorder == nil {
		return
	}
	if err := g.recorder.Record(ctx, rec); err != nil {
		g.logger.ErrorContext(ctx, "compliance decision record failed", "err", err, "portfolio_id", rec.Result.GetPortfolioId())
	}
}

// project applies the order delta to a copy of the book, producing the
// hypothetical post-trade book. The affected position is re-marked at the order
// price (new quantity × price); other positions are unchanged. NAV is carried
// forward — a trade swaps cash for position value, leaving net asset value
// approximately unchanged (the FX/cash-impact refinement is deferred with the
// rest of the FX layer, RISK-06).
// ok=false means the notional could not be represented at all (see mulDecimal);
// the caller must REFUSE, never fall through with a fabricated market value.
func project(book *Book, d OrderDelta) (*Book, bool) {
	proj := book.clone()
	idx := -1
	for i := range proj.Positions {
		if proj.Positions[i].InstrumentID == d.InstrumentID {
			idx = i
			break
		}
	}
	var existingQty *commonpb.Decimal
	if idx >= 0 {
		existingQty = proj.Positions[idx].Quantity
	}
	newQty, ok := addDecimal(existingQty, d.SignedQuantity)
	if !ok {
		return nil, false
	}
	notional, ok := mulDecimal(newQty, d.Price)
	if !ok {
		return nil, false
	}
	pos := Position{
		InstrumentID: d.InstrumentID,
		Quantity:     newQty,
		MarketValue:  &commonpb.Money{Amount: notional, CurrencyCode: d.Currency},
	}
	if idx >= 0 {
		proj.Positions[idx] = pos
	} else {
		proj.Positions = append(proj.Positions, pos)
	}

	// THE TRADE IS PAID FOR (#415). A buy spends cash, a sell raises it, so the
	// hypothetical book's cash moves by -(signed_quantity x price).
	//
	// WITHOUT THIS THE BUYING-POWER RULE IS INERT AND LOOKS LIKE A CONTROL. It
	// would compare the PRE-trade cash against the floor, so every order the
	// portfolio could already afford would pass — including the one that spends
	// the last of it and the one after that. The rule would never fire, and its
	// silence would read as compliance.
	//
	// NAV is still carried forward unchanged, and that remains right for the
	// reason written above: a trade swaps cash for position value, so net asset
	// value is approximately unchanged. Cash is the half that MOVES, which is
	// exactly why a NAV-based rule cannot answer "can we afford this".
	//
	// ok=false on an unrepresentable cash delta, for the same reason the notional
	// does: the caller must REFUSE rather than evaluate a fabricated balance.
	if proj.Cash != nil {
		spend, ok := mulDecimal(d.SignedQuantity, d.Price)
		if !ok {
			return nil, false
		}
		after, ok := addDecimal(proj.Cash.GetAmount(), negateDecimal(spend))
		if !ok {
			return nil, false
		}
		proj.Cash = &commonpb.Money{Amount: after, CurrencyCode: proj.Cash.GetCurrencyCode()}
	}
	return proj, true
}

// negateDecimal flips a Decimal's sign. The coefficient is an int64 and math.MinInt64
// has no positive counterpart, so that one value is refused by returning it
// unchanged — it cannot arise from a real order (decutil.InDomainDeep bounds the
// exponent, and a coefficient that large is not a tradeable size), and inventing
// a wrapped positive would be the #94 defect in a new place.
func negateDecimal(d *commonpb.Decimal) *commonpb.Decimal {
	if d == nil || d.GetCoefficient() == math.MinInt64 {
		return d
	}
	return &commonpb.Decimal{Coefficient: -d.GetCoefficient(), Exponent: d.GetExponent()}
}

// addDecimal and mulDecimal MOVED to internal/dec (arith.go) for #216.
//
// They were written here, for the pre-trade gate, and then hand-copied into
// internal/risk/compute — where the copy was never repaired. A -20% stress on a
// $200,000 position came back as +$160,000's opposite, -$24,467, because that
// copy still multiplied raw int64 coefficients. One implementation per concept
// is not a style preference here: it is the difference between fixing the wrap
// once and fixing it once per package that noticed.
//
// The whole analysis — why math/big, why the exponent is RAISED rather than
// wrapped, why the alignment window is clamped, and what ok=false obliges the
// caller to do — travelled with the code and now lives on decutil.Add / decutil.Mul.
//
// These two names stay as one-line wrappers on purpose. project() reads in the
// gate's own vocabulary, and — the reason that matters — mul_test.go and
// arithmetic_sweep_test.go call them by these names. Leaving those entry points
// untouched means the COMP-01 test suite, written against the original, is what
// proves the move changed no behaviour. Rewriting the tests alongside the code
// would have proved only that they agree with each other.

// addDecimal sums two Decimals exactly, or refuses. ok=false obliges project()
// to refuse the order rather than value it at a fabricated quantity.
func addDecimal(a, b *commonpb.Decimal) (*commonpb.Decimal, bool) { return decutil.Add(a, b) }

// mulDecimal returns the order's notional, or refuses. ok=false means the
// notional is genuinely unrepresentable and the gate must REFUSE — never a
// wrapped product, which the concentration rules would read as a tiny position.
func mulDecimal(a, b *commonpb.Decimal) (*commonpb.Decimal, bool) { return decutil.Mul(a, b) }
