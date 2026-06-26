package compliance

import (
	"context"
	"log/slog"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	compliancepb "github.com/kanz-eng/kanz-schemas-go/compliance/v1"
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
}

// NewPreTradeGate wires the gate. engine defaults to NewEngine(nil); a nil
// recorder disables decision logging (the decision still flows).
func NewPreTradeGate(engine *Engine, books BookSource, mandates MandateSource, classifier Classifier, recorder DecisionRecorder, logger *slog.Logger) *PreTradeGate {
	if engine == nil {
		engine = NewEngine(nil)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &PreTradeGate{engine: engine, books: books, mandates: mandates, classifier: classifier, recorder: recorder, logger: logger}
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
	if !ok || len(mandate.GetRules()) == 0 {
		return Decision{Allowed: true}, nil // no constraints
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
