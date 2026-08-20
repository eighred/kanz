package order

// THE HOLD (#410) — an order that needs two people does not become an order.
//
// service_dual_control_test.go proves the OMS CLASSIFIES orders against the
// threshold. These prove what it does with the answer once the control is ARMED,
// and the load-bearing assertion in every one of them is the NEGATIVE: the order
// is absent from the order store. Asserting only that a proposal exists would
// pass against a build that held the order AND admitted it, which is the worst
// possible outcome — a control that appears to work and trades anyway.

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/services/oms/internal/approval"
)

// armedService wires an OMS whose dual-control gate is ARMED at threshold — the
// posture config.Load refused outright until the proposals table existed.
func armedService(t *testing.T, fb *fakeBus, threshold *big.Rat) (*Service, *prometheus.Registry, *MemoryStore) {
	t.Helper()
	reg := prometheus.NewRegistry()
	g, err := approval.NewGate(true, threshold, nil, reg)
	if err != nil {
		t.Fatalf("NewGate armed: %v", err)
	}
	store := NewMemoryStore()
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil, nil, nil, nil,
		WithDualControl(g),
		WithHaltGate(halt.OpenGate(nil)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, reg, store
}

// largeOrderFrom is limitOrder with an authenticated issuer on it, which is what
// the api-gateway stamps from the verified principal (AUTH-01c). Without one
// there is no proposer, and the hold refuses rather than holding — see
// TestAnOrderWithNoAuthenticatedIssuerIsRefusedRatherThanHeld.
func largeOrderFrom(issuer string) *orderpb.SubmitOrder {
	cmd := limitOrder(d(100, 0), d(1025, -2)) // 1025 notional
	cmd.Metadata = &commandpb.CommandMetadata{
		Issuer:   issuer,
		TargetId: cmd.GetOrderId(),
		// A "user:" issuer is a DELEGATED command, so the entitlement gate reads
		// this snapshot; without it every order here would be refused
		// NOT_ENTITLED long before the hold ever ran.
		PrincipalPortfolios: []string{cmd.GetPortfolioId()},
	}
	return cmd
}

// TestAnOrderAtOrAboveTheThresholdDOESNOTBECOMEANADMITTEDORDER is the test this
// whole change exists to make pass.
//
// IT ASSERTS ABSENCE FROM THE ORDER STORE, not the presence of a proposal. A
// build that wrote the proposal and then fell through to store.Create would
// satisfy "a proposal exists" while the fund traded on one signature — and the
// order book would carry a live order somebody was told was awaiting approval.
func TestAnOrderAtOrAboveTheThresholdDOESNOTBECOMEANADMITTEDORDER(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))

	cmd := largeOrderFrom("user:alice@kanz")
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if _, _, err := store.Load(context.Background(), cmd.GetOrderId()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the order IS IN THE ORDER STORE (err=%v) — an order requiring a second signature "+
			"was admitted. Everything downstream of admission treats it as a live order: work() will "+
			"route it to a venue, the projector will carry exposure for it, and the trader was told "+
			"it needs approval", err)
	}
	if fb.last(EventTypeAccepted) != nil {
		t.Fatal("ORDER_ACCEPTED was published for a held order — every consumer that folds it now " +
			"believes the fund has a live order nobody approved")
	}
	if fb.last(EventTypeRejected) != nil {
		t.Fatal("ORDER_REJECTED was published for a held order — the order is not dead, it is " +
			"awaiting a signature, and telling the estate otherwise is the silent drop with a " +
			"misleading label on it")
	}

	held, ok, err := store.Proposals().Get(context.Background(), cmd.GetOrderId())
	if err != nil || !ok {
		t.Fatalf("no proposal was recorded (ok=%v err=%v) — the order was neither admitted nor "+
			"held, which is the silent drop #410 exists to end", ok, err)
	}
	if held.Proposer != "user:alice@kanz" {
		t.Errorf("proposer = %q, want the command's authenticated issuer", held.Proposer)
	}
	if held.Act != dualcontrol.ActOrderSubmission {
		t.Errorf("act = %q, want %q — an approval for another act could otherwise cover this order",
			held.Act, dualcontrol.ActOrderSubmission)
	}
	// The digest must be the one an approver re-derives from the terms in hand.
	want, err := approval.TermsOfSubmit(cmd).Digest()
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if held.Digest != want {
		t.Errorf("stored digest %q does not match the terms of the command it holds (%q) — every "+
			"approval would fail as a payload change", held.Digest, want)
	}
}

// TestAHeldOrderIsAnnouncedAsPendingApproval. The proposal row is durable and
// INVISIBLE: it is not in the book, so no blotter, no projection and no outcome
// consumer would ever hear of it. The FACT is the only thing that says so.
func TestAHeldOrderIsAnnouncedAsPendingApproval(t *testing.T) {
	fb := &fakeBus{}
	svc, _, _ := armedService(t, fb, dualRat("1000"))

	cmd := largeOrderFrom("user:alice@kanz")
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	msg := fb.last(EventTypePendingApproval)
	if msg == nil {
		t.Fatalf("no ORDER_PENDING_APPROVAL was published (published: %v) — the order is held in a "+
			"table nothing outside this service reads, so as far as the estate is concerned it was "+
			"dropped", fb.types())
	}
	fact, ok := msg.(*orderpb.OrderPendingApproval)
	if !ok {
		t.Fatalf("payload is %T, not OrderPendingApproval", msg)
	}
	if fact.GetOrderId() != cmd.GetOrderId() {
		t.Errorf("order_id = %q, want %q", fact.GetOrderId(), cmd.GetOrderId())
	}
	if fact.GetProposer() != "user:alice@kanz" {
		t.Errorf("proposer = %q — an approver cannot be shown who they are countersigning",
			fact.GetProposer())
	}
	if fact.GetDigest() == "" {
		t.Error("the FACT carries no digest, so nothing on the wire says WHAT is being approved")
	}
	// THE COMMAND TRAVELS WITH IT. An approval bound to an id the approver cannot
	// inspect is a signature on something unread.
	if fact.GetCommand().GetInstrumentId() != cmd.GetInstrumentId() ||
		fact.GetCommand().GetOrderType() != cmd.GetOrderType() {
		t.Errorf("the FACT does not carry the order that was proposed: %v", fact.GetCommand())
	}
	if !fact.GetExpiresAt().AsTime().After(fact.GetProposedAt().AsTime()) {
		t.Error("expires_at is not after proposed_at — a proposal born expired can never be " +
			"approved and would sit in the queue looking like work nobody did")
	}
}

// TestAHeldOrderIsNotCountedAsHavingGoneThroughOnOneSignature.
//
// kanz_oms_order_signatures_total answers "how much of the order flow went
// through, and on how many signatures". A held order went through on nobody's.
// Counting it as single_signed would make the metric get WORSE as the control
// started working, which is the opposite of what the arming decision reads.
func TestAHeldOrderIsNotCountedAsHavingGoneThroughOnOneSignature(t *testing.T) {
	fb := &fakeBus{}
	svc, reg, _ := armedService(t, fb, dualRat("1000"))

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, largeOrderFrom("user:alice@kanz"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := totalSignatures(t, reg); got != 0 {
		t.Fatalf("a HELD order was counted (%v across all series) — it was not admitted, so it did "+
			"not go through on one signature or any other number", got)
	}
}

// TestAnOrderBelowTheThresholdIsStillAdmittedWithTheControlArmed — the other
// half, so the tests above cannot pass by holding everything. Arming dual
// control must not become a trading outage for ordinary flow.
func TestAnOrderBelowTheThresholdIsStillAdmittedWithTheControlArmed(t *testing.T) {
	fb := &fakeBus{}
	svc, reg, store := armedService(t, fb, dualRat("1000000"))

	cmd := largeOrderFrom("user:alice@kanz") // 1025, far below 1000000
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, _, err := store.Load(context.Background(), cmd.GetOrderId()); err != nil {
		t.Fatalf("a SMALL order was not admitted with the control armed (%v) — arming dual control "+
			"has turned into a trading outage for ordinary flow", err)
	}
	if _, ok, _ := store.Proposals().Get(context.Background(), cmd.GetOrderId()); ok {
		t.Fatal("a below-threshold order was HELD — the threshold is not being applied, and every " +
			"order on the book now waits for a second person")
	}
	if got := signatures(t, reg, approval.SingleSigned, string(approval.PostureBelow)); got != 1 {
		t.Errorf("single_signed/below_threshold = %v, want 1", got)
	}
}

// TestAnUnvaluableOrderIsHeldRatherThanAdmitted. An order the platform could not
// SIZE must not slip under a threshold it was never compared against; the safe
// direction on a control is more signatures, not fewer. A MARKET order with no
// mark source is exactly that case.
func TestAnUnvaluableOrderIsHeldRatherThanAdmitted(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))

	cmd := largeOrderFrom("user:alice@kanz")
	cmd.OrderType = orderpb.OrderType_ORDER_TYPE_MARKET
	cmd.LimitPrice = nil

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, _, err := store.Load(context.Background(), cmd.GetOrderId()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an order NOBODY COULD VALUE was admitted (err=%v) — it was treated as small "+
			"rather than as unmeasured, which is how the largest orders escape the control", err)
	}
	if _, ok, _ := store.Proposals().Get(context.Background(), cmd.GetOrderId()); !ok {
		t.Fatal("an unvaluable order was neither admitted nor held")
	}
}

// TestAnOrderWithNoAuthenticatedIssuerIsRefusedRatherThanHeld.
//
// A proposal with no proposer makes the self-approval check VACUOUS — every
// approver differs from "" — so the one rule this control rests on would pass
// for anyone. Holding it would put an unapprovable order in a queue where every
// attempt fails for a reason the approver cannot fix: a silent drop with extra
// steps. The caller is told now.
func TestAnOrderWithNoAuthenticatedIssuerIsRefusedRatherThanHeld(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))

	cmd := largeOrderFrom("") // no issuer at all

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	rej, _ := fb.last(EventTypeRejected).(*orderpb.OrderRejected)
	if rej == nil {
		t.Fatalf("an order with no authenticated issuer was not refused (published: %v)", fb.types())
	}
	if rej.GetErrorCode() != ReasonUnsignable {
		t.Errorf("error_code = %q, want %q so an operator can tell this from a compliance breach",
			rej.GetErrorCode(), ReasonUnsignable)
	}
	if _, ok, _ := store.Proposals().Get(context.Background(), cmd.GetOrderId()); ok {
		t.Fatal("an unapprovable order was put in the pending queue, where it can only expire")
	}
	if _, _, err := store.Load(context.Background(), cmd.GetOrderId()); !errors.Is(err, ErrNotFound) {
		t.Fatal("the order was admitted after being refused")
	}
}

// TestARedeliveredHeldOrderIsAnnouncedExactlyOnce. Broker redelivery is normal.
// Two proposals for one order would be two entries in the approver's queue for
// one decision, and two announcements of it.
func TestARedeliveredHeldOrderIsAnnouncedExactlyOnce(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))

	cmd := largeOrderFrom("user:alice@kanz")
	payload := mustMarshal(t, cmd)
	for i := 0; i < 3; i++ {
		if err := svc.Handle(testCtx(), submitEnv(), payload); err != nil {
			t.Fatalf("Handle #%d: %v", i, err)
		}
	}

	announced := 0
	for _, tp := range fb.types() {
		if tp == EventTypePendingApproval {
			announced++
		}
	}
	if announced != 1 {
		t.Fatalf("ORDER_PENDING_APPROVAL published %d times for one order — a redelivery announced "+
			"the hold again, so the approver's queue and the audit trail both double-count one "+
			"decision", announced)
	}
	pending, err := store.Proposals().Pending(context.Background(), t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending queue holds %d proposals for one order", len(pending))
	}
}

// TestAHeldOrderIsVisibleOnThePendingQueue — #410's acceptance clause. An
// unapproved act must be VISIBLY PENDING rather than silently dropped.
func TestAHeldOrderIsVisibleOnThePendingQueue(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, largeOrderFrom("user:alice@kanz"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	pending, err := store.Proposals().Pending(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("the pending queue holds %d orders, want 1 — a held order that appears on no list "+
			"is indistinguishable from one that was dropped", len(pending))
	}
	if pending[0].Command.GetInstrumentId() != "AAPL" {
		t.Errorf("the queued proposal does not carry the order an approver has to read: %v",
			pending[0].Command)
	}
}

// TestAHoldWithNoTenantOnTheDeliveryHoldsNOTHING — the #498 trap, checked rather
// than assumed.
//
// outbox.From takes the tenant from the INBOUND DELIVERY. An override arrives
// over HTTP, so it has none, and because the enqueue is inside the override
// transaction EVERY override on the durable path would have failed — the defect
// that crash-looped the OMS once, arriving by the same route.
//
// A SubmitOrder arrives on the BUS, so there IS a delivery and there IS a
// tenant: every other test in this file holds an order successfully, which is
// that fact demonstrated rather than asserted. This is the negative half — the
// FACT cannot be captured, so the hold rolls back rather than leaving an order
// held that nobody will ever hear about.
func TestAHoldWithNoTenantOnTheDeliveryHoldsNOTHING(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))

	cmd := largeOrderFrom("user:alice@kanz")
	// context.Background(), not testCtx(): no bus.WithTenantID, which is what a
	// publish outside an inbound delivery looks like.
	err := svc.Handle(context.Background(), submitEnvFor(testTenant), mustMarshal(t, cmd))
	if err == nil {
		t.Fatal("the hold succeeded with no tenant on the delivery — the ORDER_PENDING_APPROVAL " +
			"record could never be published, so the order would sit held and unannounced")
	}
	if _, ok, _ := store.Proposals().Get(context.Background(), cmd.GetOrderId()); ok {
		t.Fatal("an order was held after its announcement failed to be captured — the two are " +
			"supposed to be one transaction")
	}
	if _, _, lerr := store.Load(context.Background(), cmd.GetOrderId()); !errors.Is(lerr, ErrNotFound) {
		t.Fatal("the order was admitted after the hold failed")
	}
}
