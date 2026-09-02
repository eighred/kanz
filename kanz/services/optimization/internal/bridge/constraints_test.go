package bridge

// Constraint-envelope tests (#972).
//
// The envelope stands between an approved rebalance and an unbounded MARKET
// order, so the cases that matter are the refusals and the two translations —
// a slippage bound becoming a limit PRICE, and a window becoming an EXPIRY. Each
// test names the capital consequence of it not happening.

import (
	"errors"
	"math/big"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/optimization"
)

// bounded returns the fixture proposal with an envelope the test can shape.
func bounded(mut func(*optimization.ProposalConstraints)) optimization.RebalanceProposal {
	p := sampleProposal()
	c := &optimization.ProposalConstraints{
		MaxNotional: dec.ToProto(big.NewRat(1_000_000, 1)),
	}
	if mut != nil {
		mut(c)
	}
	p.Constraints = c
	// Priced trades, so a slippage bound has a reference price to work from:
	// 100 units at 10 and 50 at 20.
	p.Trades = []optimization.ProposedTrade{
		{InstrumentID: "AAA", Side: optimization.Buy, Quantity: 100, Notional: 1000},
		{InstrumentID: "BBB", Side: optimization.Sell, Quantity: 50, Notional: 1000},
	}
	return p
}

// AN UNBOUNDED PROPOSAL DOES NOT MATERIALIZE. This is the defect: a proposal said
// what to trade and never what it may cost, and every child went out as an
// unbounded MARKET order good for the rest of the day.
func TestAProposalWithNoEnvelopeIsRefused(t *testing.T) {
	p := sampleProposal()
	p.Constraints = nil
	cmds, err := ToOrders(p, "alice", testFreshness())
	if !errors.Is(err, ErrConstraintsUnstated) {
		t.Fatalf("an unbounded proposal materialized (err=%v) — nothing said what it may cost", err)
	}
	if len(cmds) != 0 {
		t.Fatalf("%d command(s) built for a refused proposal", len(cmds))
	}
	if got := EnvelopeRefusalCode(err); got != "PROPOSAL_UNBOUNDED" {
		t.Fatalf("refusal code = %q, want PROPOSAL_UNBOUNDED", got)
	}
}

// AN ENVELOPE WITH NO CEILING IS NOT AN ENVELOPE. max_notional is the circuit
// breaker between an optimizer bug — a weight error, a NAV read from the wrong
// instant — and the market, and there is no legitimate unbounded rebalance.
func TestAnEnvelopeWithNoNotionalCeilingIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*optimization.ProposalConstraints)
	}{
		{"nil ceiling", func(c *optimization.ProposalConstraints) { c.MaxNotional = nil }},
		{"zero ceiling", func(c *optimization.ProposalConstraints) { c.MaxNotional = dec.ToProto(new(big.Rat)) }},
		{"negative ceiling", func(c *optimization.ProposalConstraints) { c.MaxNotional = dec.ToProto(big.NewRat(-1, 1)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ToOrders(bounded(tc.mut), "alice", testFreshness())
			if !errors.Is(err, ErrNotionalUnbounded) {
				t.Fatalf("a %s materialized: %v", tc.name, err)
			}
		})
	}
}

// THE CEILING IS THE AGGREGATE, NOT THE CHILD. A per-child ceiling is evaded by
// splitting one trade into two, and the risk being bounded is the rebalance's
// whole footprint — a bad NAV read scales every leg at once.
func TestTheNotionalCeilingBindsOnTheWholeTradeList(t *testing.T) {
	// Two trades of 1000 each. A ceiling of 1500 admits neither individually
	// over-sized trade and must still refuse: the aggregate is 2000.
	p := bounded(func(c *optimization.ProposalConstraints) {
		c.MaxNotional = dec.ToProto(big.NewRat(1500, 1))
	})
	_, err := ToOrders(p, "alice", testFreshness())
	if !errors.Is(err, ErrNotionalExceeded) {
		t.Fatalf("a trade list of 2000 against a 1500 ceiling materialized (err=%v) — the ceiling "+
			"is being applied per child, which is evaded by splitting a trade in two", err)
	}
	if got := EnvelopeRefusalCode(err); got != "NOTIONAL_EXCEEDED" {
		t.Fatalf("refusal code = %q, want NOTIONAL_EXCEEDED — it is a different operator problem "+
			"from an unbounded proposal", got)
	}
	// And the aggregate exactly at the ceiling is admitted: the bound is inclusive.
	ok := bounded(func(c *optimization.ProposalConstraints) {
		c.MaxNotional = dec.ToProto(big.NewRat(2000, 1))
	})
	if _, err := ToOrders(ok, "alice", testFreshness()); err != nil {
		t.Fatalf("a trade list exactly at the ceiling was refused: %v", err)
	}
}

// AN OVER-SIZED PROPOSAL IS REFUSED WHOLE, NEVER TRUNCATED. Dropping legs until
// the total fits leaves a partially rebalanced book: one side of a pair trade on,
// weights further from target than before the run, and no record of which legs
// went.
func TestAnOversizedProposalIsRefusedWholeNotTrimmed(t *testing.T) {
	p := bounded(func(c *optimization.ProposalConstraints) {
		c.MaxNotional = dec.ToProto(big.NewRat(1500, 1))
	})
	cmds, err := ToOrders(p, "alice", testFreshness())
	if err == nil {
		t.Fatal("an over-sized proposal was admitted")
	}
	if len(cmds) != 0 {
		t.Fatalf("%d command(s) survived an over-sized proposal — half a rebalance is not a "+
			"smaller rebalance", len(cmds))
	}
}

// A SLIPPAGE BOUND BECOMES A LIMIT PRICE, which is what actually stops a fill. A
// bound checked only at proposal time bounds nothing: the proposal is not where
// the money moves.
func TestASlippageBoundBecomesALimitPriceInTheCostlyDirection(t *testing.T) {
	// 100bp on a buy at 10 ⇒ 10.10; on a sell at 20 ⇒ 19.80.
	p := bounded(func(c *optimization.ProposalConstraints) { c.MaxSlippageBPS = 100 })
	cmds, err := ToOrders(p, "alice", testFreshness())
	if err != nil {
		t.Fatalf("ToOrders: %v", err)
	}
	if len(cmds) != 2 {
		t.Fatalf("built %d commands, want 2", len(cmds))
	}
	for _, cmd := range cmds {
		if cmd.GetOrderType() != orderpb.OrderType_ORDER_TYPE_LIMIT {
			t.Fatalf("%s is %s, want LIMIT — a slippage bound that leaves a MARKET order bounds "+
				"nothing", cmd.GetInstrumentId(), cmd.GetOrderType())
		}
		if cmd.GetLimitPrice() == nil {
			t.Fatalf("%s is a LIMIT order with no price", cmd.GetInstrumentId())
		}
	}
	// The DIRECTION is the half that is easy to get backwards: a buy may pay UP,
	// a sell may take LESS. Inverted, the "bound" would be better than the
	// reference price and would never bind at all.
	buy, sell := cmds[0], cmds[1]
	if got := dec.FromProto(buy.GetLimitPrice()); got.Cmp(big.NewRat(101, 10)) != 0 {
		t.Fatalf("buy limit = %s, want 10.10 (ref 10 + 100bp)", got.FloatString(4))
	}
	if got := dec.FromProto(sell.GetLimitPrice()); got.Cmp(big.NewRat(198, 10)) != 0 {
		t.Fatalf("sell limit = %s, want 19.80 (ref 20 − 100bp)", got.FloatString(4))
	}
}

// ZERO SLIPPAGE IS A DECISION, NOT AN ABSENCE. It says "cross whatever the book
// offers", which is legitimate for an unwind that must complete — and it is
// distinguishable from an unset envelope because the envelope itself is required.
func TestAZeroSlippageBoundKeepsMarketOrders(t *testing.T) {
	cmds, err := ToOrders(bounded(nil), "alice", testFreshness())
	if err != nil {
		t.Fatalf("ToOrders: %v", err)
	}
	for _, cmd := range cmds {
		if cmd.GetOrderType() != orderpb.OrderType_ORDER_TYPE_MARKET {
			t.Fatalf("%s is %s, want MARKET — zero slippage means cross, explicitly",
				cmd.GetInstrumentId(), cmd.GetOrderType())
		}
		if cmd.GetLimitPrice() != nil {
			t.Fatalf("%s carries a limit price with no slippage bound", cmd.GetInstrumentId())
		}
	}
}

// A BOUND THAT CANNOT BE PRICED REFUSES THE PROPOSAL rather than emitting the
// child unprotected. A proposal that ASKED for 20bp and produced an unbounded
// MARKET order is worse than one that asked for nothing: the envelope is on the
// record and an auditor reads a protection the order never had.
func TestAnUnpriceableSlippageBoundRefusesRatherThanDegrades(t *testing.T) {
	p := bounded(func(c *optimization.ProposalConstraints) { c.MaxSlippageBPS = 20 })
	// A trade with no quantity has no reference price.
	p.Trades = append(p.Trades, optimization.ProposedTrade{
		InstrumentID: "CCC", Side: optimization.Buy, Quantity: 0, Notional: 500,
	})
	cmds, err := ToOrders(p, "alice", testFreshness())
	if !errors.Is(err, ErrUnpriceableSlippageBound) {
		t.Fatalf("a trade with no reference price was admitted under a slippage bound (err=%v) — "+
			"it would have gone out as an unprotected MARKET order", err)
	}
	if len(cmds) != 0 {
		t.Fatalf("%d command(s) survived — the whole proposal is refused, not the one trade", len(cmds))
	}
	if got := EnvelopeRefusalCode(err); got != "SLIPPAGE_BOUND_UNPRICEABLE" {
		t.Fatalf("refusal code = %q, want SLIPPAGE_BOUND_UNPRICEABLE", got)
	}
}

// AN EXECUTION WINDOW BECOMES AN EXPIRY. TIME_IN_FORCE_DAY let a child rejected,
// requeued or slow to route still fill hours later against a book the proposal
// never saw.
func TestAnExecutionWindowBecomesGTDWithAnExpiry(t *testing.T) {
	p := bounded(func(c *optimization.ProposalConstraints) { c.ExecutionWindow = 15 * time.Minute })
	cmds, err := ToOrders(p, "alice", testFreshness())
	if err != nil {
		t.Fatalf("ToOrders: %v", err)
	}
	for _, cmd := range cmds {
		if cmd.GetTimeInForce() != orderpb.TimeInForce_TIME_IN_FORCE_GTD {
			t.Fatalf("%s is %s, want GTD — a window that leaves DAY lets the instruction outlive "+
				"the thesis", cmd.GetInstrumentId(), cmd.GetTimeInForce())
		}
		want := testNow().Add(15 * time.Minute)
		if got := cmd.GetExpireAt().AsTime(); !got.Equal(want) {
			t.Fatalf("%s expires %s, want %s", cmd.GetInstrumentId(), got, want)
		}
	}
}

// ZERO WINDOW KEEPS DAY — the same explicit-choice reading as zero slippage.
func TestAZeroWindowKeepsDay(t *testing.T) {
	cmds, err := ToOrders(bounded(nil), "alice", testFreshness())
	if err != nil {
		t.Fatalf("ToOrders: %v", err)
	}
	for _, cmd := range cmds {
		if cmd.GetTimeInForce() != orderpb.TimeInForce_TIME_IN_FORCE_DAY {
			t.Fatalf("%s is %s, want DAY", cmd.GetInstrumentId(), cmd.GetTimeInForce())
		}
		if cmd.GetExpireAt() != nil {
			t.Fatalf("%s carries an expiry with no window", cmd.GetInstrumentId())
		}
	}
}

// THE PROPOSAL'S OWN EXPIRY TIGHTENS THE ESTATE'S BOUND (#972 on #970). An
// approver saying "good for ten minutes" is making a narrower claim than
// OPTIMIZATION_PROPOSAL_MAX_AGE, and the tighter of the two binds.
func TestAProposalsOwnExpiryTightensTheFreshnessBound(t *testing.T) {
	p := bounded(func(c *optimization.ProposalConstraints) {
		c.ExpiresAt = testNow().Add(-time.Minute) // already passed
	})
	// The estate bound is an hour and the proposal is freshly dated, so only the
	// proposal's own expiry can refuse this.
	_, err := ToOrders(p, "alice", testFreshness())
	if !errors.Is(err, ErrProposalStale) {
		t.Fatalf("a proposal past its OWN expires_at materialized (err=%v) — the tighter of the "+
			"two bounds must bind", err)
	}
}

// AND IT CANNOT LOOSEN IT. expires_at arrives in the request body, so a
// per-proposal field that could extend the deployment's ceiling would let any
// caller opt out of the control — the same reason the mandate verdict is refused
// from the body (#409/#646).
func TestAProposalsOwnExpiryCannotLoosenTheFreshnessBound(t *testing.T) {
	p := bounded(func(c *optimization.ProposalConstraints) {
		c.ExpiresAt = testNow().Add(24 * time.Hour) // generous
	})
	p.AsOf = testNow().Add(-4 * time.Hour) // but the inputs are four hours old
	if _, err := ToOrders(p, "alice", fixedFreshness(time.Hour)); !errors.Is(err, ErrProposalStale) {
		t.Fatalf("a far-future expires_at overrode the estate's one-hour bound (err=%v) — any "+
			"caller could then opt out of the freshness control", err)
	}
}

// EVERY REFUSAL HAS A DISTINCT, STABLE CODE, and a non-envelope error has none.
func TestEnvelopeRefusalCodesAreClosedAndDistinct(t *testing.T) {
	seen := map[string]error{}
	for _, err := range []error{ErrConstraintsUnstated, ErrNotionalUnbounded, ErrNotionalExceeded, ErrUnpriceableSlippageBound} {
		code := EnvelopeRefusalCode(err)
		if code == "" {
			t.Fatalf("%v has no refusal code", err)
		}
		if !IsEnvelopeRefusal(err) {
			t.Fatalf("IsEnvelopeRefusal did not recognise %v", err)
		}
		seen[code] = err
	}
	// Unstated and Unbounded share a code deliberately — same operator action.
	if len(seen) != 3 {
		t.Fatalf("got %d distinct codes, want 3", len(seen))
	}
	if EnvelopeRefusalCode(ErrMandateInfeasible) != "" {
		t.Fatal("a mandate error was claimed as an envelope refusal")
	}
	if EnvelopeRefusalCode(ErrProposalStale) != "" {
		t.Fatal("a freshness error was claimed as an envelope refusal")
	}
}

// THE FIRST LAYER, TESTED ON ITS OWN.
//
// FOUND BY MUTATION: making checkEnvelope admit a nil envelope left
// TestAProposalWithNoEnvelopeIsRefused green, because applyConstraints refuses
// nil too and ToOrders returns on the first trade either way. That redundancy is
// deliberate defence in depth — but it meant no test could tell which layer was
// doing the work, so removing the outer one was invisible.
//
// The distinction matters: checkEnvelope refuses BEFORE any command is
// constructed and validates the CEILING, which applyConstraints never looks at.
// An unbounded-notional proposal reaching applyConstraints would be built and
// then... admitted, because a zero slippage bound and a zero window are both
// legitimate. Only the outer check stops it.
func TestCheckEnvelopeRefusesOnItsOwn(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    optimization.RebalanceProposal
		want error
	}{
		{"nil envelope", func() optimization.RebalanceProposal {
			p := sampleProposal()
			p.Constraints = nil
			return p
		}(), ErrConstraintsUnstated},
		{"no ceiling", bounded(func(c *optimization.ProposalConstraints) { c.MaxNotional = nil }), ErrNotionalUnbounded},
		{"over the ceiling", bounded(func(c *optimization.ProposalConstraints) {
			c.MaxNotional = dec.ToProto(big.NewRat(100, 1))
		}), ErrNotionalExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := checkEnvelope(tc.p); !errors.Is(err, tc.want) {
				t.Fatalf("checkEnvelope(%s) = %v, want %v — this is the layer that refuses before "+
					"any command is built, and the only one that reads the ceiling", tc.name, err, tc.want)
			}
		})
	}
	// And it admits a bounded proposal, so the refusals above are discrimination
	// rather than a function that refuses everything.
	gross, err := checkEnvelope(bounded(nil))
	if err != nil {
		t.Fatalf("checkEnvelope refused a bounded proposal: %v", err)
	}
	if gross != 2000 {
		t.Fatalf("aggregate notional = %g, want 2000 — the ceiling is compared against this", gross)
	}
}

// AND THE SECOND LAYER, likewise on its own: applyConstraints must refuse a nil
// envelope rather than silently emitting an unconstrained child, which is what
// makes the redundancy above worth having.
func TestApplyConstraintsRefusesANilEnvelope(t *testing.T) {
	cmd := &orderpb.SubmitOrder{InstrumentId: "AAA"}
	err := applyConstraints(cmd, optimization.ProposedTrade{InstrumentID: "AAA", Quantity: 1, Notional: 10}, nil, testNow())
	if !errors.Is(err, ErrConstraintsUnstated) {
		t.Fatalf("applyConstraints(nil) = %v, want ErrConstraintsUnstated", err)
	}
}
