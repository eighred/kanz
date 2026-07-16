package compliance

import (
	"context"
	"log/slog"
	"sync"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	compliancepb "github.com/kanz-eng/kanz-schemas-go/compliance/v1"

	// decutil: this package's own tests declare a local `dec(...)` Decimal-literal
	// helper (engine_test.go), so the platform decimal package is aliased here to
	// avoid the name clash rather than renaming a helper used across every test
	// file in the package.
	decutil "github.com/kanz-eng/kanz/internal/dec"
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

// Evaluate runs the pre-trade check for one order. A returned error is TRANSIENT
// (book/mandate load failure) and the caller should retry; a clean Decision with
// Allowed=false is a terminal compliance rejection. An order against a portfolio
// with no mandate is allowed.
func (g *PreTradeGate) Evaluate(ctx context.Context, d OrderDelta) (Decision, error) {
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
	proj := project(book, d)
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
func project(book *Book, d OrderDelta) *Book {
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
	newQty := addDecimal(existingQty, d.SignedQuantity)
	pos := Position{
		InstrumentID: d.InstrumentID,
		Quantity:     newQty,
		MarketValue:  &commonpb.Money{Amount: mulDecimal(newQty, d.Price), CurrencyCode: d.Currency},
	}
	if idx >= 0 {
		proj.Positions[idx] = pos
	} else {
		proj.Positions = append(proj.Positions, pos)
	}
	return proj
}

// addDecimal returns a+b, exact, by aligning to the smaller exponent. nil is
// treated as zero.
func addDecimal(a, b *commonpb.Decimal) *commonpb.Decimal {
	if a == nil {
		a = &commonpb.Decimal{}
	}
	if b == nil {
		b = &commonpb.Decimal{}
	}
	exp := a.GetExponent()
	if b.GetExponent() < exp {
		exp = b.GetExponent()
	}
	ac := a.GetCoefficient() * pow10(a.GetExponent()-exp)
	bc := b.GetCoefficient() * pow10(b.GetExponent()-exp)
	return &commonpb.Decimal{Coefficient: ac + bc, Exponent: exp}
}

// mulDecimal returns a×b, exact (coefficients multiply, exponents add). nil is
// treated as zero.
func mulDecimal(a, b *commonpb.Decimal) *commonpb.Decimal {
	if a == nil || b == nil {
		return &commonpb.Decimal{}
	}
	return &commonpb.Decimal{
		Coefficient: a.GetCoefficient() * b.GetCoefficient(),
		Exponent:    a.GetExponent() + b.GetExponent(),
	}
}

// pow10 returns 10^n for n ≥ 0 (n is a non-negative exponent gap here).
func pow10(n int32) int64 {
	p := int64(1)
	for ; n > 0; n-- {
		p *= 10
	}
	return p
}
