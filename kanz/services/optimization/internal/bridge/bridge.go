// Package bridge materializes an approved RebalanceProposal into OMS-01
// order.v1.SubmitOrder commands (OPT-01e). The proposal is human-in-the-loop
// (AUTO-01 conservative stance): the optimizer proposes, a human approves, and
// ONLY THEN does this bridge emit commands — each issuer-bound (the approving
// principal on CommandMetadata.issuer, the AUTH-01c forged-issuer guard at the
// gateway/producer) and re-checked against the COMP-01 pre-trade gate
// (deny-by-default) so a target that was feasible at optimization time but has
// since drifted is caught before it becomes a live order.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/optimization"
)

// Gate is the COMP-01 pre-trade check the bridge re-runs per order.
// compliance.PreTradeGate satisfies it.
type Gate interface {
	Evaluate(ctx context.Context, d compliance.OrderDelta) (compliance.Decision, error)
}

// Publisher emits a SubmitOrder command onto the bus. The concrete bus producer
// is wired at the composition root (the DEBT-02 inject-the-side-effect stance);
// the bridge depends only on this seam.
type Publisher interface {
	Publish(ctx context.Context, cmd *orderpb.SubmitOrder) error
}

// MaterializeResult is the outcome of materializing a proposal.
type MaterializeResult struct {
	Submitted []*orderpb.SubmitOrder
	Rejected  []RejectedOrder
}

// RejectedOrder is a command the pre-trade gate refused, with the reason.
type RejectedOrder struct {
	Command *orderpb.SubmitOrder
	Reason  string
}

// qtyExp is the Decimal scale order quantities are emitted at (1e-4 units).
const qtyExp int32 = -4

// ErrMandateUnchecked and ErrMandateInfeasible are the two ways a proposal fails
// to become orders. They are DISTINCT ERRORS rather than an empty slice because
// they are different operator problems: one means a control did not run, the
// other means it ran and said no. ToOrders used to answer both — and a proposal
// with no trades — with nil, so "refused" and "nothing to do" arrived at the
// caller identically (#646).
var (
	ErrMandateUnchecked  = errors.New("no mandate check has run on this proposal")
	ErrMandateInfeasible = errors.New("this proposal breaches its mandate")
)

// ToOrders maps a proposal's trades to SubmitOrder commands — a market order per
// trade, issuer bound to CommandMetadata.issuer, order_id deterministic per
// (portfolio, instrument) so a re-submit of the same proposal is idempotent on
// the OMS bus dedup (EVT-17d). Pure: no gate, no publish (Materialize adds
// those).
//
// THE MANDATE VERDICT IS THE GATE, AND ONLY MandateFeasible OPENS IT (#646).
// The check used to be `if !p.MandateFeasible`, over a bool whose zero value was
// overwritten with true by RebalanceProposal's own constructor — so an unchecked
// proposal and a checked-and-clean one were the same value here, and this is the
// line that decides whether a rebalance becomes live capital commands. An
// unchecked proposal is now refused by name rather than admitted.
//
// THE FRESHNESS BOUND IS CHECKED HERE, BESIDE THE MANDATE VERDICT (#970), and
// for the same reason it is here rather than in Materialize: this is the single
// function every materialization path passes through, so a caller cannot reach
// order construction without answering both questions. A check in Materialize
// alone would be skipped by server.materialize, which calls ToOrders directly.
//
// IT RUNS BEFORE THE MANDATE SWITCH. Both refuse the whole proposal, and the
// order between them decides which reason an operator is given for a proposal
// that is both stale AND infeasible. Staleness first is the more useful answer:
// an infeasibility computed against a book from four hours ago is not a fact
// about today's mandate, and reporting it as one would send somebody to
// investigate a limit that may never have been breached.
func ToOrders(p optimization.RebalanceProposal, issuer string, f Freshness) ([]*orderpb.SubmitOrder, error) {
	if _, err := f.Check(p); err != nil {
		return nil, err
	}
	switch p.MandateStatus {
	case optimization.MandateFeasible:
	case optimization.MandateInfeasible:
		return nil, fmt.Errorf("%w: %s", ErrMandateInfeasible, strings.Join(p.Violations, "; "))
	default:
		return nil, fmt.Errorf("%w (verdict: %s)", ErrMandateUnchecked, p.MandateStatus)
	}
	out := make([]*orderpb.SubmitOrder, 0, len(p.Trades))
	for _, tr := range p.Trades {
		out = append(out, &orderpb.SubmitOrder{
			Metadata: &commandpb.CommandMetadata{
				Issuer:   issuer,
				TargetId: orderID(p.PortfolioID, tr.InstrumentID),
				Reason:   fmt.Sprintf("rebalance %s→%s", pct(tr.CurrentWeight), pct(tr.TargetWeight)),
			},
			OrderId:      orderID(p.PortfolioID, tr.InstrumentID),
			PortfolioId:  p.PortfolioID,
			InstrumentId: tr.InstrumentID,
			Side:         orderSide(tr.Side),
			Quantity:     &commonpb.Decimal{Coefficient: int64(math.Round(tr.Quantity * 1e4)), Exponent: qtyExp},
			OrderType:    orderpb.OrderType_ORDER_TYPE_MARKET,
			TimeInForce:  orderpb.TimeInForce_TIME_IN_FORCE_DAY,
		})
	}
	return out, nil
}

// Materialize maps the proposal to orders, re-checks each against the pre-trade
// gate (deny-by-default — a gate error or BREACH rejects the order), and
// publishes the admitted ones. The admitted/rejected split is returned so the
// caller (and audit) sees exactly what went to the OMS and what the gate
// refused. A nil gate skips the re-check; a nil publisher dry-runs (mapping +
// gate only).
// tenantID is WHOSE proposal this is. It is a parameter and not something read
// off the proposal because RebalanceProposal carries no tenant: without it the
// gate's mandate lookup had only the portfolio name to go on, and two tenants
// calling a portfolio "growth" got each other's limits (#243). An empty one is
// refused by the gate rather than resolved to a guess.
//
// "A NIL GATE IS SAFE BECAUSE CheckMandate ALREADY RAN" used to be the reason
// written on that parameter, and it was FALSE for the whole life of the
// sentence: no caller on the service's route ran the check (#646). ToOrders now
// ENFORCES the premise rather than resting on it — an unchecked proposal returns
// ErrMandateUnchecked before the loop below starts, so nothing is published and
// the caller is told which of the two refusals it was.
func Materialize(ctx context.Context, p optimization.RebalanceProposal, tenantID, issuer, currency string, prices map[string]float64, gate Gate, pub Publisher, f Freshness) (MaterializeResult, error) {
	var res MaterializeResult
	cmds, err := ToOrders(p, issuer, f)
	if err != nil {
		return res, err
	}
	for _, cmd := range cmds {
		if gate != nil {
			decision, err := gate.Evaluate(ctx, orderDelta(cmd, tenantID, currency, prices))
			if err != nil {
				res.Rejected = append(res.Rejected, RejectedOrder{Command: cmd, Reason: "gate error: " + err.Error()})
				continue
			}
			if !decision.Allowed {
				res.Rejected = append(res.Rejected, RejectedOrder{Command: cmd, Reason: gateReason(decision)})
				continue
			}
		}
		if pub != nil {
			if err := pub.Publish(ctx, cmd); err != nil {
				return res, fmt.Errorf("publish %s: %w", cmd.GetOrderId(), err)
			}
		}
		res.Submitted = append(res.Submitted, cmd)
	}
	return res, nil
}

// orderDelta projects a SubmitOrder into the compliance OrderDelta the gate
// evaluates: signed quantity (negative for a sell) at the instrument's price.
func orderDelta(cmd *orderpb.SubmitOrder, tenantID, currency string, prices map[string]float64) compliance.OrderDelta {
	q := cmd.GetQuantity()
	signed := q
	if cmd.GetSide() == orderpb.Side_SIDE_SELL && q != nil {
		signed = &commonpb.Decimal{Coefficient: -q.GetCoefficient(), Exponent: q.GetExponent()}
	}
	var price *commonpb.Decimal
	if p, ok := prices[cmd.GetInstrumentId()]; ok {
		price = &commonpb.Decimal{Coefficient: int64(math.Round(p * 100)), Exponent: -2}
	}
	return compliance.OrderDelta{
		TenantID:       tenantID,
		PortfolioID:    cmd.GetPortfolioId(),
		InstrumentID:   cmd.GetInstrumentId(),
		SignedQuantity: signed,
		Price:          price,
		Currency:       currency,
		OrderID:        cmd.GetOrderId(),
		Issuer:         cmd.GetMetadata().GetIssuer(),
	}
}

// gateReason renders a rejected Decision as a human-readable reason.
//
// EVERY MEMBER OF THE "NOTHING WAS EVALUATED" FAMILY IS NAMED HERE, and none may
// fall through to "pre-trade compliance breach" — nothing breached, because
// nothing was checked (COMP-M1). That used to be true for Ungoverned, and would
// otherwise now be true for the others.
//
// Unreadable was the one that WAS falling through (#803), here and in the OMS
// gate, both for the same reason: this list is written by hand and a fifth flag
// was added to the family without either copy being opened.
// test/arch/every_decision_consumer_handles_every_refusal_test.go now fails the
// build when that happens, and derives the family from decide()'s own returns
// rather than from a third hand-written list.
func gateReason(d compliance.Decision) string {
	if d.Ungoverned {
		return "no mandate governs this portfolio"
	}
	if d.Unpriced {
		return "no usable price to value this order"
	}
	if d.Unvaluable {
		return "order notional cannot be represented — the order was not evaluated"
	}
	if d.Unscoped {
		return "cannot determine which tenant's mandate governs this portfolio — the order was not evaluated"
	}
	if d.Unreadable {
		return "the mandate governing this portfolio could not be applied, so the order was not " +
			"evaluated — the mandate must be republished; this does not resolve on retry"
	}
	if d.Result != nil && len(d.Result.GetViolations()) > 0 {
		return d.Result.GetViolations()[0].GetMessage()
	}
	return "pre-trade compliance breach"
}

func orderSide(s optimization.TradeSide) orderpb.Side {
	if s == optimization.Sell {
		return orderpb.Side_SIDE_SELL
	}
	return orderpb.Side_SIDE_BUY
}

func orderID(portfolioID, instrumentID string) string {
	return fmt.Sprintf("%s:%s:rebal", portfolioID, instrumentID)
}

func pct(w float64) string { return fmt.Sprintf("%.2f%%", w*100) }
