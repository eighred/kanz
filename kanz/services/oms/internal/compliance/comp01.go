package compliance

import (
	"context"
	"math/big"
	"strings"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	compliancepb "github.com/kanz-eng/kanz-schemas-go/compliance/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"

	comp "github.com/kanz-eng/kanz/internal/compliance"
	"github.com/kanz-eng/kanz/internal/dec"
)

// COMP01Gate adapts the COMP-01 pre-trade engine to the OMS-01f Gate seam: it
// maps a SubmitOrder into the engine's neutral OrderDelta, runs the hypothetical
// post-trade check, and maps a BREACH back into a *Breach the order handler
// rejects on. This is the composition-time bridge that keeps the OMS decoupled
// from the compliance internals — the handler still sees only the Gate
// interface.
type COMP01Gate struct {
	gate     *comp.PreTradeGate
	currency string
	now      func() time.Time
	marks    MarkSource
}

// MarkSource supplies a reference price for an order that carries none. It is
// satisfied by internal/marketdata/mark.Source. nil means "no usable price" —
// never a zero, never a guess.
type MarkSource interface {
	Mark(instrument string) *big.Rat
}

// COMP01Option configures the adapter.
type COMP01Option func(*COMP01Gate)

// WithMarkSource supplies the reference price used to value MARKET and STOP
// orders, which carry no limit price of their own (COMP-M2). Without it those
// orders are refused PRICE_UNAVAILABLE, which is COMP-M1's behaviour and
// remains the behaviour whenever the source has no fresh mark to give.
func WithMarkSource(m MarkSource) COMP01Option {
	return func(g *COMP01Gate) { g.marks = m }
}

// NewCOMP01Gate wires the adapter. currency stamps the projected order's Money
// (the OMS has no per-instrument currency join yet, OMS-01e); it is the
// portfolio base currency.
func NewCOMP01Gate(gate *comp.PreTradeGate, currency string, opts ...COMP01Option) *COMP01Gate {
	g := &COMP01Gate{gate: gate, currency: currency, now: time.Now}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

var _ Gate = (*COMP01Gate)(nil)

// Check implements Gate. A transient engine error (book/mandate load) is
// returned so the handler retries; a BREACH returns a *Breach (terminal
// rejection); PASS/WARN return nil (admit).
func (g *COMP01Gate) Check(ctx context.Context, cmd *orderpb.SubmitOrder) (*Breach, error) {
	decision, err := g.gate.Evaluate(ctx, comp.OrderDelta{
		PortfolioID:    cmd.GetPortfolioId(),
		InstrumentID:   cmd.GetInstrumentId(),
		SignedQuantity: signedQuantity(cmd.GetSide(), cmd.GetQuantity()),
		Price:          g.price(cmd),
		Currency:       g.currency,
		OrderID:        cmd.GetOrderId(),
		Issuer:         cmd.GetMetadata().GetIssuer(),
		AsOf:           g.now().UTC(),
	})
	if err != nil {
		return nil, err
	}
	if decision.Allowed {
		return nil, nil
	}
	// An UNGOVERNED portfolio is refused under its own code, not a rule violation:
	// nothing was breached, because nothing governs it. A reviewer reading
	// MANDATE_MISSING knows to go and write a mandate — not to go and look for the
	// rule that fired (EXEC-M14).
	if decision.Ungoverned {
		return &Breach{
			Code:   "MANDATE_MISSING",
			Reason: "no mandate governs portfolio " + cmd.GetPortfolioId(),
		}, nil
	}
	// An UNPRICED order is refused under its own code, not a rule violation:
	// nothing was breached, because nothing could be evaluated. A reviewer
	// reading PRICE_UNAVAILABLE knows to go wire a reference-price source
	// (COMP-M2) — not to go look for the rule that fired (COMP-M1).
	if decision.Unpriced {
		return &Breach{
			Code:   "PRICE_UNAVAILABLE",
			Reason: "no usable price to value order for instrument " + cmd.GetInstrumentId(),
		}, nil
	}
	return breachFromResult(decision.Result), nil
}

// price values the order's notional.
//
// A LIMIT or STOP_LIMIT order carries its own limit price and is valued at it —
// that is the price the fund has committed to, and repricing it at the market
// would evaluate a different order than the one submitted. A MARKET or STOP
// order carries none, so it is valued at the reference mark.
//
// The switch mirrors execution.SimVenue.executionPrice, deliberately: one
// question ("what price does this order type carry?") should not have two
// different answers in one codebase.
//
// nil is returned when there is no fresh mark, and nil is what the gate already
// refuses (Decision.Unpriced). There is no new rejection path here and no way
// to admit an order without a real price for it.
func (g *COMP01Gate) price(cmd *orderpb.SubmitOrder) *commonpb.Decimal {
	switch cmd.GetOrderType() {
	case orderpb.OrderType_ORDER_TYPE_LIMIT, orderpb.OrderType_ORDER_TYPE_STOP_LIMIT:
		return cmd.GetLimitPrice()
	case orderpb.OrderType_ORDER_TYPE_MARKET, orderpb.OrderType_ORDER_TYPE_STOP:
		// falls through to mark pricing below
	default:
		// Anything else — including ORDER_TYPE_UNSPECIFIED — is refused, not
		// mark-priced. This gate runs BEFORE order-type validation (Accept, in
		// services/oms/internal/order/service.go), so an unrecognised order type
		// reaching this switch must not be admitted here and recorded as a
		// compliance PASS only to be rejected later by validation.
		return nil
	}
	if g.marks == nil {
		return nil
	}
	m := g.marks.Mark(cmd.GetInstrumentId())
	if m == nil {
		return nil
	}
	// A mark that dec.ToProto cannot represent exactly must refuse, not admit at
	// a wrong price. dec.ToProto scales the rational by a fixed 10^8, rounds
	// half-up to a big.Int, and returns big.Int.Int64() as the Decimal
	// coefficient — but Int64() is UNDEFINED (silently wraps, per math/big) when
	// that scaled value does not fit in an int64. A mark just past 2^64/10^8
	// would wrap to an arbitrary small (even negative) coefficient, and the gate
	// would then evaluate a fabricated notional instead of the real one — the
	// opposite of refusing an order it cannot value. Failing closed here (nil,
	// which the gate already treats as Unpriced) is correct: we would rather
	// refuse a real order than admit one at a fabricated price. dec.ToProto
	// itself is not changed — it is shared by many other callers, and widening
	// its contract is out of scope here.
	if !markRepresentable(m) {
		return nil
	}
	return dec.ToProto(m)
}

// markRepresentable mirrors dec.ToProto's exact scaling and half-up rounding
// (internal/dec/dec.go) to determine, BEFORE calling it, whether the resulting
// coefficient fits in an int64. It must reproduce that arithmetic precisely —
// checking the input's rough magnitude, or checking ToProto's output after the
// fact, cannot distinguish a correctly rounded small coefficient from one that
// already wrapped.
func markRepresentable(r *big.Rat) bool {
	const scale = 8 // dec.ToProto's fixed scale; duplicated only to detect
	// non-representable input ahead of its unexported rounding step.
	pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(scale), nil)
	scaledNum := new(big.Int).Mul(r.Num(), pow)
	q, rem := new(big.Int).QuoRem(scaledNum, r.Denom(), new(big.Int))
	twice := new(big.Int).Mul(new(big.Int).Abs(rem), big.NewInt(2))
	if twice.Cmp(new(big.Int).Abs(r.Denom())) >= 0 {
		if r.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	return q.IsInt64()
}

// signedQuantity returns +quantity for a buy and −quantity for a sell, so the
// projected position moves in the right direction.
func signedQuantity(side orderpb.Side, qty *commonpb.Decimal) *commonpb.Decimal {
	if qty == nil {
		return nil
	}
	if side == orderpb.Side_SIDE_SELL {
		return &commonpb.Decimal{Coefficient: -qty.GetCoefficient(), Exponent: qty.GetExponent()}
	}
	return qty
}

// breachFromResult maps the first BREACH-severity violation to a *Breach. The
// Code is the rule type (e.g. "CONCENTRATION") — the OMS handler prefixes it
// with "COMPLIANCE_" for the rejection error code.
func breachFromResult(res *compliancepb.ComplianceResult) *Breach {
	for _, v := range res.GetViolations() {
		if v.GetSeverity() == compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
			return &Breach{Code: ruleCode(v.GetRuleType()), Reason: v.GetMessage()}
		}
	}
	// Status is BREACH but no breach violation found — defensive: refuse anyway.
	return &Breach{Code: "MANDATE", Reason: "mandate breach"}
}

// ruleCode turns RULE_TYPE_CONCENTRATION into "CONCENTRATION".
func ruleCode(t compliancepb.RuleType) string {
	return strings.TrimPrefix(t.String(), "RULE_TYPE_")
}
