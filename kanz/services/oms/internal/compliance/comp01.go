package compliance

import (
	"context"
	"math/big"
	"strings"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/dec"
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
func (g *COMP01Gate) Check(ctx context.Context, tenantID string, cmd *orderpb.SubmitOrder) (*Breach, error) {
	decision, err := g.gate.Evaluate(ctx, comp.OrderDelta{
		TenantID:       tenantID,
		PortfolioID:    cmd.GetPortfolioId(),
		InstrumentID:   cmd.GetInstrumentId(),
		SignedQuantity: signedQuantity(cmd.GetSide(), cmd.GetQuantity()),
		Price:          g.price(cmd),
		Currency:       g.currency,
		// WHICH EXCHANGE ACCOUNT'S COLLATERAL THIS ORDER SPENDS FROM (#408). It is
		// the same venue string resolveAccount routes on, so the margin the gate
		// checks is the margin of the account the order will actually reach.
		Venue:   cmd.GetVenue(),
		OrderID: cmd.GetOrderId(),
		// ONE DECISION, THIS MANY ORDERS (#435, #484). A scheduled parent is
		// checked once for the whole notional and its children are admitted
		// without re-checking, so the fills land on N order ids while only this
		// record exists. Saying so here is what lets the audit log be read
		// backwards from a child.
		WorkedSlices: cmd.GetExecutionSchedule().GetSliceCount(),
		Issuer:       cmd.GetMetadata().GetIssuer(),
		AsOf:         g.now().UTC(),
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
			Code:   comp.CodeMandateMissing,
			Reason: "no mandate governs portfolio " + cmd.GetPortfolioId(),
		}, nil
	}
	// An UNPRICED order is refused under its own code, not a rule violation:
	// nothing was breached, because nothing could be evaluated. A reviewer
	// reading PRICE_UNAVAILABLE knows to go wire a reference-price source
	// (COMP-M2) — not to go look for the rule that fired (COMP-M1).
	if decision.Unpriced {
		return &Breach{
			Code:   comp.CodePriceUnavailable,
			Reason: "no usable price to value order for instrument " + cmd.GetInstrumentId(),
		}, nil
	}
	// An order whose NOTIONAL could not be represented is refused under its own
	// code too. It is not PRICE_UNAVAILABLE — the price was fine, and that code
	// would send a reviewer to go wire a price source that is already wired.
	// NOTIONAL_UNREPRESENTABLE says what actually happened: quantity × price is
	// beyond any Decimal, so no rule was evaluated. The order size is the thing
	// to look at.
	if decision.Unvaluable {
		return &Breach{
			Code:   comp.CodeNotionalUnrepresentable,
			Reason: "order notional (quantity × price) cannot be represented for instrument " + cmd.GetInstrumentId(),
		}, nil
	}
	// The platform could not say WHOSE mandate governs this portfolio, so it
	// evaluated none (#243). Its own code, because the operator action is neither
	// "write a mandate" (MANDATE_MISSING) nor "look for the rule that fired" — it
	// is "two tenants share this portfolio name, disambiguate them".
	//
	// The Reason deliberately does NOT name the other tenants. This string is
	// returned to the submitting client on the ORDER_REJECTED FACT; the tenants
	// involved belong in the OMS's log, which is where the gate puts them, not in
	// another customer's rejection message.
	if decision.Unscoped {
		return &Breach{
			Code: comp.CodeMandateTenantUnresolved,
			Reason: "cannot determine which tenant's mandate governs portfolio " + cmd.GetPortfolioId() +
				" — the order was not evaluated against any rule",
		}, nil
	}
	// A mandate for this portfolio WAS PUBLISHED and could not be applied, so —
	// a fifth time — NO RULE WAS EVALUATED (#803). Without this branch the
	// decision fell through to breachFromResult with a NIL Result and came back
	// as the defensive tail's "MANDATE" / "mandate breach": a rule breach that
	// never fired, reported to the client and filed in the audit trail.
	//
	// The action this names is the ONLY one that resolves it, and it is neither
	// of the two a reviewer would otherwise try. The mandate stream is COMPACTED,
	// so the message that failed to decode is the last one on that portfolio's
	// subject: every consumer that boots re-reads it and fails identically, and
	// this portfolio refuses every order until somebody republishes. Retrying
	// does nothing and there is no rule to go and read.
	if decision.Unreadable {
		return &Breach{
			Code: comp.CodeMandateUnreadable,
			Reason: "the mandate governing portfolio " + cmd.GetPortfolioId() + " could not be " +
				"applied, so the order was not evaluated against any rule — the mandate must be " +
				"republished; this does not resolve on retry",
		}, nil
	}
	return breachFromResult(decision.Result), nil
}

// price values the order's notional. See OrderPrice — this is the gate's own
// mark source applied to it.
func (g *COMP01Gate) price(cmd *orderpb.SubmitOrder) *commonpb.Decimal {
	return OrderPrice(cmd, g.marks)
}

// OrderPrice is THE answer to "what price does this order carry".
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
// IT IS EXPORTED BECAUSE A SECOND CALLER APPEARED (#410). The dual-control
// threshold values an order to compare it against OMS_DUAL_CONTROL_MIN_NOTIONAL,
// and an operator who sets one number must not get two behaviours from it — a
// threshold that valued a MARKET order differently from the pre-trade gate would
// mean the compliance record and the four-eyes record disagree about how large
// the same order was. Copying the switch was the alternative, and this
// repository has already paid that bill: 17 services each had their own
// secret().
//
// marks may be nil, and nil is not a failure: it means MARKET and STOP orders
// cannot be valued here. nil is returned when there is no fresh mark, and nil is
// what the gate already refuses (Decision.Unpriced). There is no new rejection
// path here and no way to value an order without a real price for it.
func OrderPrice(cmd *orderpb.SubmitOrder, marks MarkSource) *commonpb.Decimal {
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
	if marks == nil {
		return nil
	}
	m := marks.Mark(cmd.GetInstrumentId())
	if m == nil {
		return nil
	}
	// A mark too large for the fixed scale is RESCALED, not refused: the
	// magnitude is what the gate values the order on, and eight decimal places
	// on a very large price buy nothing. Refusing here would turn a real price
	// into a refused order. Only a value that cannot be represented at ANY
	// exponent yields nil, which the gate already treats as Unpriced.
	d, ok := dec.ToProtoScaled(m)
	if !ok {
		return nil
	}
	return d
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
