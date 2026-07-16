package compliance

import (
	"context"
	"strings"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	compliancepb "github.com/kanz-eng/kanz-schemas-go/compliance/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"

	comp "github.com/kanz-eng/kanz/internal/compliance"
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
}

// NewCOMP01Gate wires the adapter. currency stamps the projected order's Money
// (the OMS has no per-instrument currency join yet, OMS-01e); it is the
// portfolio base currency.
func NewCOMP01Gate(gate *comp.PreTradeGate, currency string) *COMP01Gate {
	return &COMP01Gate{gate: gate, currency: currency, now: time.Now}
}

var _ Gate = (*COMP01Gate)(nil)

// Check implements Gate. A transient engine error (book/mandate load) is
// returned so the handler retries; a BREACH returns a *Breach (terminal
// rejection); PASS/WARN return nil (admit).
func (g *COMP01Gate) Check(ctx context.Context, cmd *orderpb.SubmitOrder) (*Breach, error) {
	dec, err := g.gate.Evaluate(ctx, comp.OrderDelta{
		PortfolioID:    cmd.GetPortfolioId(),
		InstrumentID:   cmd.GetInstrumentId(),
		SignedQuantity: signedQuantity(cmd.GetSide(), cmd.GetQuantity()),
		// Price values the order's notional. The limit price is the only price a
		// SubmitOrder carries; a MARKET/STOP order has none, and the gate refuses
		// those pre-trade (COMP-M1) rather than valuing them at zero. Admitting
		// market orders again needs a reference-price source wired here — COMP-M2.
		Price:    cmd.GetLimitPrice(),
		Currency: g.currency,
		OrderID:  cmd.GetOrderId(),
		Issuer:   cmd.GetMetadata().GetIssuer(),
		AsOf:     g.now().UTC(),
	})
	if err != nil {
		return nil, err
	}
	if dec.Allowed {
		return nil, nil
	}
	// An UNGOVERNED portfolio is refused under its own code, not a rule violation:
	// nothing was breached, because nothing governs it. A reviewer reading
	// MANDATE_MISSING knows to go and write a mandate — not to go and look for the
	// rule that fired (EXEC-M14).
	if dec.Ungoverned {
		return &Breach{
			Code:   "MANDATE_MISSING",
			Reason: "no mandate governs portfolio " + cmd.GetPortfolioId(),
		}, nil
	}
	// An UNPRICED order is refused under its own code, not a rule violation:
	// nothing was breached, because nothing could be evaluated. A reviewer
	// reading PRICE_UNAVAILABLE knows to go wire a reference-price source
	// (COMP-M2) — not to go look for the rule that fired (COMP-M1).
	if dec.Unpriced {
		return &Breach{
			Code:   "PRICE_UNAVAILABLE",
			Reason: "no usable price to value order for instrument " + cmd.GetInstrumentId(),
		}, nil
	}
	return breachFromResult(dec.Result), nil
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
