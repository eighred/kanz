package order

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/services/oms/internal/approval"
	"github.com/eighred/kanz/services/oms/internal/compliance"
)

// AN AMEND MUST NOT BE A WAY AROUND THE CONTROL PLANE (#799).
//
// handleAmend called no control at all. s.gate.Check, halt.Refusal and
// s.dualControl each appeared exactly once in the service and all three were
// inside submit, while the aggregate bounded an amended quantity only BELOW,
// against filled_quantity. The only brake in front of it — venueMayBeWorking —
// is a venue-divergence guard (#740) that says nothing about compliance and does
// not cover an order resting in PENDING_NEW or ACCEPTED, which is where work()
// leaves every order in a paper deployment and every order whose route nacked.
//
// So an order for 100 cleared the mandate and rested; an amend raised it to
// 1,000,000; a venue was configured later and the startup sweep routed the
// amended size. No mandate rule, no second signature and no kill switch ever saw
// the number that traded.
//
// EVERY ASSERTION BELOW READS THE STORE. A handler returns nil for a refusal
// exactly as it does for a success — it acks and publishes REJECTED — so
// checking the error would read a refusal as an amend that worked, and checking
// only the outcome would miss a REJECTED published over a mutated order, which
// is the original defect wearing a refusal's label.

// stagedGate is a pre-trade gate that admits until it is armed. The order under
// test has to be ADMITTED before it can be amended, so a gate that refuses
// everything (denyGate) cannot set the scene these tests need.
type stagedGate struct {
	armed  bool
	breach *compliance.Breach
	err    error
	saw    []*orderpb.SubmitOrder
	tenant []string
}

func (g *stagedGate) Check(_ context.Context, tenantID string, cmd *orderpb.SubmitOrder) (*compliance.Breach, error) {
	g.saw = append(g.saw, cmd)
	g.tenant = append(g.tenant, tenantID)
	if !g.armed {
		return nil, nil
	}
	return g.breach, g.err
}

// amendEnvFor is amendEnv with a tenant on it — the field the pre-trade gate is
// scoped by, and the one the amend path had no access to at all before #799.
func amendEnvFor(tenant string) *envelopepb.Envelope {
	return &envelopepb.Envelope{EventType: SubjectAmend, TenantId: tenant}
}

// amendableService admits one order for 100 @ 10.25 and leaves it resting where
// NO venue holds it — no router, so work() returns early and the order stays
// PENDING_NEW. That is the population #799 is about: venueMayBeWorking does not
// cover it, so it is the state in which an amend was applied unchecked.
func amendableService(t *testing.T, fb *fakeBus, gate compliance.Gate, opts ...ServiceOption) *Service {
	t.Helper()
	svc, err := NewService(testTenant, NewMemoryStore(), NewEmitter(fb), gate, nil, nil, nil,
		append([]ServiceOption{WithHaltGate(halt.OpenGate(nil))}, opts...)...)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, limitOrder(d(100, 0), d(1025, -2)))); err != nil {
		t.Fatalf("submit: %v", err)
	}
	st := loadOrder(t, svc)
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW {
		t.Fatalf("status = %v, want PENDING_NEW — this fixture must leave the order at NO venue, "+
			"or venueMayBeWorking refuses the amend and every case below proves nothing",
			st.GetStatus())
	}
	return svc
}

// amendPriced is an amend from the order's own entitled owner carrying either or
// both terms. Entitlement is deliberately satisfied: the refusals under test
// must not be reachable through the entitlement gate that refuses on a different
// axis.
func amendPriced(qty, price *commonpb.Decimal) *orderpb.AmendOrder {
	return &orderpb.AmendOrder{
		OrderId:       "o1",
		NewQuantity:   qty,
		NewLimitPrice: price,
		Metadata: &commandpb.CommandMetadata{
			Issuer: "user:owner", TargetId: "o1", PrincipalPortfolios: []string{"pf1"},
		},
	}
}

// outcomeCode returns the code on the last CommandOutcome, so a test asserts
// what the CLIENT was told rather than what the handler returned.
func outcomeCode(t *testing.T, fb *fakeBus) (commandpb.CommandOutcomeStatus, string) {
	t.Helper()
	oc, ok := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if !ok {
		t.Fatal("no CommandOutcome was published at all — the caller is waiting on an answer " +
			"that never arrives")
	}
	return oc.GetStatus(), oc.GetErrorCode()
}

// THE HEADLINE CASE. The size the mandate would refuse must not reach the book.
func TestAmend_RaisingQuantityIsRefusedByThePreTradeGate(t *testing.T) {
	fb := &fakeBus{}
	gate := &stagedGate{breach: &compliance.Breach{Code: "CONCENTRATION", Reason: "over sector cap"}}
	svc := amendableService(t, fb, gate)
	gate.armed = true

	if err := svc.Handle(testCtx(), amendEnvFor("acme"), mustMarshal(t, amendPriced(d(1000000, 0), nil))); err != nil {
		t.Fatalf("amend: %v", err)
	}

	status, code := outcomeCode(t, fb)
	if status != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED ||
		code != "COMPLIANCE_CONCENTRATION" {
		t.Fatalf("outcome = %v/%q, want REJECTED/COMPLIANCE_CONCENTRATION", status, code)
	}
	if got := loadOrder(t, svc).GetOrderedQuantity().GetCoefficient(); got != 100 {
		t.Fatalf("ordered quantity = %d, want 100 — the mandate refused the amended size and the "+
			"platform stored it anyway", got)
	}
}

// The gate is asked about the ORDER THAT WOULD EXIST, not about the one that
// does. A candidate carrying the pre-amend quantity would pass every rule the
// original order already passed, so the check would run and decide nothing.
func TestAmend_ThePreTradeCandidateCarriesTheAmendedTerms(t *testing.T) {
	fb := &fakeBus{}
	gate := &stagedGate{}
	svc := amendableService(t, fb, gate)
	gate.armed = true // admits (nil breach), but records what it was asked

	if err := svc.Handle(testCtx(), amendEnvFor("acme"), mustMarshal(t, amendPriced(d(900, 0), d(2000, -2)))); err != nil {
		t.Fatalf("amend: %v", err)
	}

	if len(gate.saw) != 2 {
		t.Fatalf("the gate was called %d time(s), want 2 (the submit and the amend)", len(gate.saw))
	}
	cand := gate.saw[1]
	if cand.GetQuantity().GetCoefficient() != 900 {
		t.Fatalf("candidate quantity = %v, want the AMENDED 900 — the gate was asked about the "+
			"order that already passed it", cand.GetQuantity())
	}
	if cand.GetLimitPrice().GetCoefficient() != 2000 {
		t.Fatalf("candidate limit price = %v, want the AMENDED 20.00", cand.GetLimitPrice())
	}
	// The terms the amend does NOT touch must survive into the candidate, or the
	// gate evaluates a different order than the one that would rest.
	if cand.GetPortfolioId() != "pf1" || cand.GetInstrumentId() != "AAPL" ||
		cand.GetSide() != orderpb.Side_SIDE_BUY ||
		cand.GetOrderType() != orderpb.OrderType_ORDER_TYPE_LIMIT ||
		cand.GetTimeInForce() != orderpb.TimeInForce_TIME_IN_FORCE_DAY {
		t.Fatalf("candidate lost untouched terms: %v", cand)
	}
	if cand.GetOrderId() != "o1" {
		t.Fatalf("candidate order id = %q, want o1 — a decision recorded against another id is "+
			"unattributable", cand.GetOrderId())
	}
	if cand.GetMetadata().GetIssuer() != "user:owner" {
		t.Fatalf("candidate issuer = %q, want the amending principal — the compliance decision "+
			"records WHO asked", cand.GetMetadata().GetIssuer())
	}
}

// A RAISED LIMIT PRICE IS A RAISED COMMITMENT, on either side. quantity x price
// is what every control on this path sizes an order by, so a quantity-shaped
// check would let the same increase through the other factor.
func TestAmend_RaisingTheLimitPriceIsGatedToo(t *testing.T) {
	fb := &fakeBus{}
	gate := &stagedGate{breach: &compliance.Breach{Code: "CONCENTRATION", Reason: "over sector cap"}}
	svc := amendableService(t, fb, gate)
	gate.armed = true

	// Quantity unchanged; the limit moves 10.25 → 500.00.
	if err := svc.Handle(testCtx(), amendEnvFor("acme"), mustMarshal(t, amendPriced(nil, d(50000, -2)))); err != nil {
		t.Fatalf("amend: %v", err)
	}

	if _, code := outcomeCode(t, fb); code != "COMPLIANCE_CONCENTRATION" {
		t.Fatalf("error code = %q, want COMPLIANCE_CONCENTRATION", code)
	}
	if got := loadOrder(t, svc).GetLimitPrice().GetCoefficient(); got != 1025 {
		t.Fatalf("limit price = %d, want 1025 — the amend was refused and applied anyway", got)
	}
}

// THE CARVE-OUT, AND IT IS LOAD-BEARING RATHER THAN A CONVENIENCE. A de-risking
// amend is not re-checked, because refusing it leaves the LARGER order resting —
// strictly the worse book. It is the same reasoning submit gives for not
// re-checking a schedule's children ("IT BREAKS ORDERS THE MANDATE ALLOWED") and
// handleCancel gives for not being halt-gated.
//
// It is also this file's non-vacuity arm: without it every test above would pass
// on a handler that simply refused all amends.
func TestAmend_ReducingExposureIsNotRefusedByABreachingGate(t *testing.T) {
	fb := &fakeBus{}
	gate := &stagedGate{breach: &compliance.Breach{Code: "CONCENTRATION", Reason: "over sector cap"}}
	svc := amendableService(t, fb, gate)
	gate.armed = true
	calls := len(gate.saw)

	if err := svc.Handle(testCtx(), amendEnvFor("acme"), mustMarshal(t, amendPriced(d(50, 0), nil))); err != nil {
		t.Fatalf("amend: %v", err)
	}

	status, code := outcomeCode(t, fb)
	if status != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED {
		t.Fatalf("outcome = %v/%q, want EXECUTED — a reduction was refused, which leaves the "+
			"bigger order resting", status, code)
	}
	if got := loadOrder(t, svc).GetOrderedQuantity().GetCoefficient(); got != 50 {
		t.Fatalf("ordered quantity = %d, want 50", got)
	}
	if len(gate.saw) != calls {
		t.Fatalf("the gate was consulted %d extra time(s) for a reduction — it must not be asked "+
			"a question whose only possible answer strands the order", len(gate.saw)-calls)
	}
}

// Lowering the limit while holding the size is a reduction on the same terms.
func TestAmend_LoweringTheLimitPriceIsNotRefused(t *testing.T) {
	fb := &fakeBus{}
	gate := &stagedGate{breach: &compliance.Breach{Code: "CONCENTRATION", Reason: "over sector cap"}}
	svc := amendableService(t, fb, gate)
	gate.armed = true

	if err := svc.Handle(testCtx(), amendEnvFor("acme"), mustMarshal(t, amendPriced(nil, d(900, -2)))); err != nil {
		t.Fatalf("amend: %v", err)
	}
	if status, code := outcomeCode(t, fb); status != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED {
		t.Fatalf("outcome = %v/%q, want EXECUTED", status, code)
	}
	if got := loadOrder(t, svc).GetLimitPrice().GetCoefficient(); got != 900 {
		t.Fatalf("limit price = %d, want 900", got)
	}
}

// THE PRE-TRADE GATE IS SCOPED TO THE ORDER'S TENANT, NOT THIS OMS'S (#243,
// #799). handleAmend did not even receive the envelope; scoping to s.tenant
// would ask for the platform's mandate for every customer amend, find none, and
// admit them unconstrained.
func TestAmend_ThePreTradeGateIsScopedToTheEnvelopesTenant(t *testing.T) {
	fb := &fakeBus{}
	gate := &stagedGate{}
	svc := amendableService(t, fb, gate)
	gate.armed = true

	if err := svc.Handle(testCtx(), amendEnvFor("acme"), mustMarshal(t, amendPriced(d(1000, 0), nil))); err != nil {
		t.Fatalf("amend: %v", err)
	}
	if len(gate.tenant) != 2 {
		t.Fatalf("the gate was called %d time(s), want 2", len(gate.tenant))
	}
	if got := gate.tenant[1]; got != "acme" {
		t.Fatalf("the amend was checked under tenant %q, want %q — %q is this OMS's own serving "+
			"tenant and resolves the platform's mandate, not the customer's",
			got, "acme", testTenant)
	}
}

// A GATE THAT CANNOT ANSWER MUST REDELIVER, NEVER REFUSE. Turning a transient
// book or mandate load failure into ORDER_REJECTED would tell the estate a
// control decided something it never decided.
func TestAmend_ATransientGateFailureRedeliversRatherThanRefusing(t *testing.T) {
	fb := &fakeBus{}
	boom := errors.New("mandate store unreachable")
	gate := &stagedGate{err: boom}
	svc := amendableService(t, fb, gate)
	gate.armed = true
	before := len(fb.types())

	err := svc.Handle(testCtx(), amendEnvFor("acme"), mustMarshal(t, amendPriced(d(1000, 0), nil)))
	if !errors.Is(err, boom) {
		t.Fatalf("Handle = %v, want the gate's error so the delivery is nacked and retried", err)
	}
	if got := len(fb.types()); got != before {
		t.Fatalf("%d event(s) were published for a check that never completed", got-before)
	}
	if got := loadOrder(t, svc).GetOrderedQuantity().GetCoefficient(); got != 100 {
		t.Fatalf("ordered quantity = %d, want 100", got)
	}
}

// THE PLATFORM KILL-SWITCH REACHES THE AMEND (#635, #799). An operator who has
// stopped the platform on a risk breach must not have an order's size raised
// underneath them.
func TestAmend_RaisingExposureIsRefusedWhileThePlatformIsHalted(t *testing.T) {
	fb := &fakeBus{}
	gate := halt.OpenGate(nil)
	svc := amendableService(t, fb, nil, WithHaltGate(gate))
	if err := gate.Handle(context.Background(), nil, operatorHalt(t)); err != nil {
		t.Fatalf("halt: %v", err)
	}

	if err := svc.Handle(testCtx(), amendEnvFor("acme"), mustMarshal(t, amendPriced(d(1000, 0), nil))); err != nil {
		t.Fatalf("amend: %v", err)
	}
	status, code := outcomeCode(t, fb)
	if status != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED ||
		code != ReasonPlatformHalted {
		t.Fatalf("outcome = %v/%q, want REJECTED/%s", status, code, ReasonPlatformHalted)
	}
	if got := loadOrder(t, svc).GetOrderedQuantity().GetCoefficient(); got != 100 {
		t.Fatalf("ordered quantity = %d, want 100 — the platform was stopped and the order grew", got)
	}
}

// AND THE HALT DOES NOT TRAP A TRADER IN THE BOOK. handleCancel is deliberately
// ungated so an operator halting on a risk breach can still get out; a reducing
// amend is the same act by a smaller step, and gating it would mean the only way
// to shrink an order during a halt is to cancel it outright.
func TestAmend_ReducingExposureStillAppliesWhileThePlatformIsHalted(t *testing.T) {
	fb := &fakeBus{}
	gate := halt.OpenGate(nil)
	svc := amendableService(t, fb, nil, WithHaltGate(gate))
	if err := gate.Handle(context.Background(), nil, operatorHalt(t)); err != nil {
		t.Fatalf("halt: %v", err)
	}

	if err := svc.Handle(testCtx(), amendEnvFor("acme"), mustMarshal(t, amendPriced(d(10, 0), nil))); err != nil {
		t.Fatalf("amend: %v", err)
	}
	if status, code := outcomeCode(t, fb); status != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED {
		t.Fatalf("outcome = %v/%q, want EXECUTED", status, code)
	}
	if got := loadOrder(t, svc).GetOrderedQuantity().GetCoefficient(); got != 10 {
		t.Fatalf("ordered quantity = %d, want 10 — a halt must not trap a trader in the book", got)
	}
}

// MAKER-CHECKER CANNOT BE OUT-AMENDED (#410, #799). An order admitted below the
// threshold on one signature must not be raised above it on the same one.
func TestAmend_AcrossTheDualControlThresholdIsRefused(t *testing.T) {
	fb := &fakeBus{}
	// Armed, threshold 5000. The order below is 100 x 10.25 = 1025 and admits on
	// one signature; the amend takes it to 1000 x 10.25 = 10250.
	dualGate, err := approval.NewGate(true, dualRat("5000"), nil, prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}
	svc := amendableService(t, fb, nil, WithDualControl(dualGate))

	if err := svc.Handle(testCtx(), amendEnvFor("acme"), mustMarshal(t, amendPriced(d(1000, 0), nil))); err != nil {
		t.Fatalf("amend: %v", err)
	}
	status, code := outcomeCode(t, fb)
	if status != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED ||
		code != ReasonAmendRequiresApproval {
		t.Fatalf("outcome = %v/%q, want REJECTED/%s", status, code, ReasonAmendRequiresApproval)
	}
	if got := loadOrder(t, svc).GetOrderedQuantity().GetCoefficient(); got != 100 {
		t.Fatalf("ordered quantity = %d, want 100 — a single signature raised an order past the "+
			"threshold two people are supposed to agree on", got)
	}
}

// NON-VACUITY FOR THE THRESHOLD. An amend that stays below it is untouched by
// the control, or the test above would pass on a gate that refused every amend.
func TestAmend_BelowTheDualControlThresholdStillApplies(t *testing.T) {
	fb := &fakeBus{}
	dualGate, err := approval.NewGate(true, dualRat("5000"), nil, prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}
	svc := amendableService(t, fb, nil, WithDualControl(dualGate))

	// 400 x 10.25 = 4100, still under 5000.
	if err := svc.Handle(testCtx(), amendEnvFor("acme"), mustMarshal(t, amendPriced(d(400, 0), nil))); err != nil {
		t.Fatalf("amend: %v", err)
	}
	if status, code := outcomeCode(t, fb); status != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED {
		t.Fatalf("outcome = %v/%q, want EXECUTED", status, code)
	}
	if got := loadOrder(t, svc).GetOrderedQuantity().GetCoefficient(); got != 400 {
		t.Fatalf("ordered quantity = %d, want 400", got)
	}
}

// submitFromState is the ONE mapping from an order's stored terms to the command
// that would create them, and the field it is most likely to lose is the one a
// hand-listed copy already lost: the schedule driver's own version dropped
// leverage and margin_mode, so a levered parent's slices reached admission as
// spot orders and the audit root disagreed with the venue instruction (#240's
// shape, one layer in).
func TestSubmitFromState_CarriesEveryTermOfTheOrder(t *testing.T) {
	st := &orderpb.OrderState{
		OrderId:         "o9",
		PortfolioId:     "pf1",
		InstrumentId:    "AAPL",
		Side:            orderpb.Side_SIDE_SELL,
		OrderType:       orderpb.OrderType_ORDER_TYPE_STOP_LIMIT,
		TimeInForce:     orderpb.TimeInForce_TIME_IN_FORCE_GTC,
		ExpireAt:        timestamppb.New(t0),
		OrderedQuantity: d(7, 0),
		LimitPrice:      d(1025, -2),
		StopPrice:       d(1000, -2),
		Venue:           "BINANCE",
		ParentOrderId:   "p1",
		Leverage:        d(10, 0),
		MarginMode:      orderpb.MarginMode_MARGIN_MODE_CROSS,
	}
	cmd := submitFromState(st, &commandpb.CommandMetadata{Issuer: "user:owner"})

	if cmd.GetLeverage().GetCoefficient() != 10 {
		t.Errorf("leverage = %v, want 10 — a levered order reaching admission as spot makes the "+
			"audit root and the venue instruction disagree", cmd.GetLeverage())
	}
	if cmd.GetMarginMode() != orderpb.MarginMode_MARGIN_MODE_CROSS {
		t.Errorf("margin mode = %v, want CROSS", cmd.GetMarginMode())
	}
	if cmd.GetStopPrice().GetCoefficient() != 1000 {
		t.Errorf("stop price = %v, want 10.00", cmd.GetStopPrice())
	}
	if cmd.GetQuantity().GetCoefficient() != 7 {
		t.Errorf("quantity = %v, want the ordered quantity 7", cmd.GetQuantity())
	}
	if cmd.GetVenue() != "BINANCE" || cmd.GetParentOrderId() != "p1" ||
		cmd.GetSide() != orderpb.Side_SIDE_SELL ||
		cmd.GetTimeInForce() != orderpb.TimeInForce_TIME_IN_FORCE_GTC {
		t.Errorf("mapping lost a term: %v", cmd)
	}

	// EVERY FIELD, PROVEN BY REFLECTION RATHER THAN BY THIS LIST. A field added
	// to both messages and forgotten here is exactly the drift this function
	// exists to end, and a hand-written assertion list forgets it the same way a
	// hand-written copy does.
	for i := 0; i < cmd.ProtoReflect().Descriptor().Fields().Len(); i++ {
		fd := cmd.ProtoReflect().Descriptor().Fields().Get(i)
		name := string(fd.Name())
		switch name {
		case "metadata", "execution_schedule":
			continue // supplied by the caller; not a term of the stored order
		}
		if !cmd.ProtoReflect().Has(fd) {
			t.Errorf("submitFromState left %q unset while OrderState carries it — the mapping has "+
				"drifted from the message", name)
		}
	}
}

func TestAmendReducesExposure(t *testing.T) {
	base := func(q, p *commonpb.Decimal) *orderpb.OrderState {
		return &orderpb.OrderState{OrderedQuantity: q, LimitPrice: p}
	}
	for _, tc := range []struct {
		name       string
		prev, next *orderpb.OrderState
		want       bool
	}{
		{"unchanged", base(d(100, 0), d(10, 0)), base(d(100, 0), d(10, 0)), true},
		{"quantity down", base(d(100, 0), d(10, 0)), base(d(50, 0), d(10, 0)), true},
		{"price down", base(d(100, 0), d(10, 0)), base(d(100, 0), d(9, 0)), true},
		{"both down", base(d(100, 0), d(10, 0)), base(d(50, 0), d(9, 0)), true},
		{"quantity up", base(d(100, 0), d(10, 0)), base(d(101, 0), d(10, 0)), false},
		{"price up", base(d(100, 0), d(10, 0)), base(d(100, 0), d(11, 0)), false},
		// The trade that looks smaller and is not: 100 x 10 = 1000 becomes
		// 50 x 100 = 5000. A quantity-only test calls this a reduction.
		{"quantity down, price up", base(d(100, 0), d(10, 0)), base(d(50, 0), d(100, 0)), false},
		// Scale, not coefficient: 1.00 (100e-2) is not larger than 10 (10e0).
		{"same value, different exponent", base(d(10, 0), d(10, 0)), base(d(1000, -2), d(10, 0)), true},
		// A price appearing where there was none is a term no control has seen.
		{"price appears", base(d(100, 0), nil), base(d(100, 0), d(1, 0)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := amendReducesExposure(tc.prev, tc.next); got != tc.want {
				t.Fatalf("amendReducesExposure = %v, want %v", got, tc.want)
			}
		})
	}
}
