package compliance

import (
	"context"
	"log/slog"
	"sync"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"

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
// BREACH — a WARN passes (advisory). Every decision is recorded (COMP-01e),
// pass or reject, so the audit trail is complete.
type PreTradeGate struct {
	engine     *Engine
	books      BookSource
	mandates   MandateSource
	classifier Classifier
	recorder   DecisionRecorder
	logger     *slog.Logger

	requireMandate bool
	onUngoverned   func(portfolioID string)
	onUnpriced     func(portfolioID, instrumentID string)

	mu     sync.Mutex
	warned map[string]bool // portfolios already named in a WARN — say it once, count always
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

// WithUngovernedObserver is called for EVERY order against a portfolio no mandate
// governs — the composition root wires it to a counter, so "how much of the book is
// ungoverned" is a number on a dashboard rather than a thing nobody has asked.
func WithUngovernedObserver(fn func(portfolioID string)) PreTradeOption {
	return func(g *PreTradeGate) { g.onUngoverned = fn }
}

// WithUnpricedObserver is called for EVERY order the gate refuses for lack of a
// usable price — the composition root wires it to a counter (COMP-M1), the same
// way WithUngovernedObserver makes the ungoverned gap a number on a dashboard
// instead of a thing nobody has asked about.
func WithUnpricedObserver(fn func(portfolioID, instrumentID string)) PreTradeOption {
	return func(g *PreTradeGate) { g.onUnpriced = fn }
}

// BookSource loads the current book for a portfolio — the holdings the order is
// projected onto. In production this reads the risk-engine/position read model;
// tests supply an in-memory source.
type BookSource interface {
	Book(ctx context.Context, portfolioID string) (*Book, error)
}

// MandateSource resolves the mandate version in effect for a portfolio at a
// point in time (the COMP-01f point-in-time resolution). ok=false ⇒ no mandate
// governs the portfolio, which admits the order (no constraints declared).
type MandateSource interface {
	Mandate(ctx context.Context, portfolioID string, asOf time.Time) (*compliancepb.Mandate, bool, error)
}

// OrderDelta is the order's effect on a book, in terms compliance understands —
// the neutral shape the OMS adapter maps order.v1.SubmitOrder into, so this
// package stays decoupled from the order schema. SignedQuantity is +buy/−sell;
// Price values the order (limit price, or a reference/last price for a market
// order — the adapter resolves it).
type OrderDelta struct {
	PortfolioID    string
	InstrumentID   string
	SignedQuantity *commonpb.Decimal
	Price          *commonpb.Decimal
	Currency       string
	// OrderID and Issuer correlate the decision in the audit log (COMP-01e).
	OrderID string
	Issuer  string
	AsOf    time.Time
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
}

// NewPreTradeGate wires the gate. engine defaults to NewEngine(nil); a nil
// recorder disables decision logging (the decision still flows).
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
		warned: map[string]bool{},
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
func (g *PreTradeGate) noteUngoverned(portfolioID string) {
	if g.onUngoverned != nil {
		g.onUngoverned(portfolioID)
	}
	g.mu.Lock()
	first := !g.warned[portfolioID]
	g.warned[portfolioID] = true
	g.mu.Unlock()
	if !first {
		return
	}
	if g.requireMandate {
		g.logger.Warn("REFUSING orders: no mandate governs this portfolio",
			"portfolio_id", portfolioID,
			"fix", "put it under mandate with kanz-mandate, or unset OMS_REQUIRE_MANDATE")
		return
	}
	g.logger.Warn("UNGOVERNED: no mandate governs this portfolio — its orders are being ADMITTED WITH NO COMPLIANCE CONSTRAINTS",
		"portfolio_id", portfolioID,
		"fix", "put it under mandate with kanz-mandate, or set OMS_REQUIRE_MANDATE=true to refuse instead")
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
func (g *PreTradeGate) noteUnpriced(portfolioID, instrumentID string) {
	if g.onUnpriced != nil {
		g.onUnpriced(portfolioID, instrumentID)
	}
	key := "unpriced:" + portfolioID + ":" + instrumentID
	g.mu.Lock()
	first := !g.warned[key]
	g.warned[key] = true
	g.mu.Unlock()
	if !first {
		return
	}
	g.logger.Warn("REFUSING order: no usable price to evaluate compliance against",
		"portfolio_id", portfolioID,
		"instrument_id", instrumentID,
		"fix", "wire a reference-price source for market/stop orders (COMP-M2)")
}

// noteUnvaluable makes the unvaluable-notional refusal audible, mirroring
// noteUnpriced: once per (portfolio, instrument), loud enough to be seen. This
// should never fire in practice — it takes a notional beyond any representable
// Decimal — so if it does, somebody needs to look at the order, not the price
// feed.
func (g *PreTradeGate) noteUnvaluable(portfolioID, instrumentID string) {
	key := "unvaluable:" + portfolioID + ":" + instrumentID
	g.mu.Lock()
	first := !g.warned[key]
	g.warned[key] = true
	g.mu.Unlock()
	if !first {
		return
	}
	g.logger.Warn("REFUSING order: its notional (quantity × price) cannot be represented — the order was NOT evaluated",
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
func (g *PreTradeGate) Evaluate(ctx context.Context, d OrderDelta) (Decision, error) {
	// INPUT VALIDATION, BEFORE ANYTHING COMPUTES WITH THESE NUMBERS — including
	// before the mandate lookup, so an out-of-domain order against an UNGOVERNED
	// portfolio is refused rather than admitted. That is deliberate: a malformed
	// exponent is not a compliance question.
	if !deltaInDomain(d) {
		g.noteUnvaluable(d.PortfolioID, d.InstrumentID)
		return Decision{Allowed: false, Unvaluable: true}, nil
	}
	mandate, ok, err := g.mandates.Mandate(ctx, d.PortfolioID, d.AsOf)
	if err != nil {
		return Decision{}, err
	}
	// TWO DIFFERENT STATES, and collapsing them is what made this silent.
	//
	//   no mandate at all      → NOBODY HAS DECIDED what governs this portfolio.
	//   a mandate, zero rules  → somebody decided, explicitly, to constrain nothing.
	//
	// The second is a choice and needs no noise. The first is a GAP, and it must not
	// be indistinguishable from passing compliance (EXEC-M14).
	if !ok {
		g.noteUngoverned(d.PortfolioID)
		if g.requireMandate {
			return Decision{Allowed: false, Ungoverned: true}, nil
		}
		return Decision{Allowed: true, Ungoverned: true}, nil
	}
	if len(mandate.GetRules()) == 0 {
		return Decision{Allowed: true}, nil // governed by a mandate that constrains nothing
	}
	// An order the gate cannot value must not reach project()/heldPositions: a
	// nil, zero, or negative price projects at zero market value, and
	// heldPositions (rules.go) treats a zero-value position as FLAT — the exact
	// EXEC-M14 collapse above, one door over (COMP-M1). Refused here, before any
	// I/O, so a market/stop order can never silently erase an existing breach on
	// the same instrument from the check.
	if !decutil.IsPositive(d.Price) {
		g.noteUnpriced(d.PortfolioID, d.InstrumentID)
		return Decision{Allowed: false, Unpriced: true}, nil
	}
	book, err := g.books.Book(ctx, d.PortfolioID)
	if err != nil {
		return Decision{}, err
	}
	if !bookInDomain(book) {
		g.noteUnvaluable(d.PortfolioID, d.InstrumentID)
		return Decision{Allowed: false, Unvaluable: true}, nil
	}
	// The price was fine; the VALUATION did not fit. Refusing under its own
	// flag keeps the two apart: Unpriced sends an operator to go wire a price
	// source, which would be a wild goose chase here.
	proj, ok := project(book, d)
	if !ok {
		g.noteUnvaluable(d.PortfolioID, d.InstrumentID)
		return Decision{Allowed: false, Unvaluable: true}, nil
	}
	res := g.engine.Evaluate(ctx, &Candidate{Book: proj, Classifier: g.classifier, AsOf: d.AsOf}, mandate)
	allowed := res.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH

	g.record(ctx, DecisionRecord{
		Phase:   PhasePreTrade,
		Result:  res,
		OrderID: d.OrderID,
		Issuer:  d.Issuer,
		Allowed: allowed,
	})
	return Decision{Allowed: allowed, Result: res}, nil
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
	return proj, true
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
// caller to do — travelled with the code and now lives on dec.Add / dec.Mul.
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
