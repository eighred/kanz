package order

// MAKER-CHECKER ON ORDER SUBMISSION, ON THE LIVE ADMISSION PATH (#410).
//
// These are the tests that matter for the control rather than for the arithmetic:
// the unit tests in services/oms/internal/approval prove the threshold and the
// digest, and these prove the OMS actually consults them, for the orders it is
// supposed to, at the moment it is supposed to.

import (
	"context"
	"math/big"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/prometheus/client_golang/prometheus"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/services/oms/internal/approval"
	"github.com/eighred/kanz/services/oms/internal/schedule"
)

func dualRat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		panic("bad rational " + s)
	}
	return r
}

// dualService wires an OMS with a dual-control gate at the given threshold and
// NO compliance gate at all — which is what nil means here, and is the same
// posture a deployment with no mandate for a portfolio runs in.
func dualService(t *testing.T, fb *fakeBus, threshold *big.Rat) (*Service, *prometheus.Registry, *MemoryStore) {
	t.Helper()
	reg := prometheus.NewRegistry()
	g, err := approval.NewGate(false, threshold, nil, reg)
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}
	store := NewMemoryStore()
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil, nil, nil, nil,
		WithDualControl(g))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, reg, store
}

func signatures(t *testing.T, reg *prometheus.Registry, sig, posture string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "kanz_oms_order_signatures_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			var s, p string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "signatures":
					s = l.GetValue()
				case "posture":
					p = l.GetValue()
				}
			}
			if s == sig && p == posture {
				return m.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("no kanz_oms_order_signatures_total series for %s/%s", sig, posture)
	return 0
}

// totalSignatures sums EVERY series, including any the closed label set does not
// know about.
//
// A PER-LABEL ASSERTION IS NOT ENOUGH, and this is not a hypothetical: dropping
// the `if parent == nil` guard around the count made every child increment a
// series with posture="" — an empty label value, invisible to a test that only
// reads the four known postures, and a junk series in production. The mutation
// SURVIVED a per-label check and dies against this one. It also reports the
// unknown label by name, because "a count appeared under posture=”" is a
// different bug from "one too many at_or_above".
func totalSignatures(t *testing.T, reg *prometheus.Registry) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	known := map[string]bool{
		string(approval.PostureAbsent): true, string(approval.PostureBelow): true,
		string(approval.PostureAtOrAbove): true, string(approval.PostureUnvaluable): true,
	}
	total := 0.0
	for _, f := range families {
		if f.GetName() != "kanz_oms_order_signatures_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "posture" && !known[l.GetValue()] {
					t.Errorf("a count landed on posture=%q, which is outside the closed set — the "+
						"counter was incremented from a Decision that never came out of Decide",
						l.GetValue())
				}
			}
			total += m.GetCounter().GetValue()
		}
	}
	return total
}

// TestALargeOrderOnAnUngovernedPortfolioIsStillClassified IS THE TRAP THIS
// CONTROL WAS NEARLY BUILT INTO.
//
// internal/compliance/gate.go returns Allowed BEFORE resolving a price when a
// portfolio has no mandate (OMS_REQUIRE_MANDATE defaults to false) or when its
// mandate has zero rules. A dual-control threshold implemented as a rule inside
// that gate would inherit the short-circuit exactly — so the largest orders on
// UNGOVERNED portfolios, which is the population a maker-checker rule exists for,
// would never be compared against the threshold at all.
//
// This service is constructed with NO compliance gate, which is that state at its
// most extreme: nothing values the order except the dual-control gate itself.
func TestALargeOrderOnAnUngovernedPortfolioIsStillClassified(t *testing.T) {
	fb := &fakeBus{}
	svc, reg, _ := dualService(t, fb, dualRat("1000"))

	// 100 at 10.25 = 1025, comfortably above the threshold.
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, limitOrder(d(100, 0), d(1025, -2)))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if got := signatures(t, reg, approval.SingleSigned, string(approval.PostureAtOrAbove)); got != 1 {
		t.Fatalf("single_signed/at_or_above_threshold = %v, want 1 — a large order on a portfolio no "+
			"mandate governs was not measured against the dual-control threshold, which is exactly the "+
			"population the control exists for", got)
	}
}

// TestASmallOrderIsCountedBelowTheThreshold — the other half, so the test above
// cannot pass by counting everything as large.
func TestASmallOrderIsCountedBelowTheThreshold(t *testing.T) {
	fb := &fakeBus{}
	svc, reg, _ := dualService(t, fb, dualRat("1000000"))

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, limitOrder(d(100, 0), d(1025, -2)))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if got := signatures(t, reg, approval.SingleSigned, string(approval.PostureBelow)); got != 1 {
		t.Fatalf("single_signed/below_threshold = %v, want 1", got)
	}
	if got := signatures(t, reg, approval.SingleSigned, string(approval.PostureAtOrAbove)); got != 0 {
		t.Fatalf("single_signed/at_or_above_threshold = %v, want 0 — a small order was counted as "+
			"large, so the number the arming decision rests on is not the number it claims to be", got)
	}
}

// TestAnOrderThatIsREFUSEDIsNotCountedAsHavingGoneThrough.
//
// The counter answers "how much of the order flow went through with one
// signature". An order refused at admission never went through at all, and
// counting it would inflate the number the arming decision rests on with orders
// nobody ever traded.
func TestAnOrderThatIsREFUSEDIsNotCountedAsHavingGoneThrough(t *testing.T) {
	fb := &fakeBus{}
	svc, reg, _ := dualService(t, fb, dualRat("1000"))

	// A quantity of zero is refused by Accept — after the gate has classified it.
	bad := limitOrder(d(0, 0), d(1025, -2))
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, bad)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := fb.last(EventTypeRejected); got == nil {
		t.Fatal("the order was not rejected, so this test is not exercising the refusal path")
	}

	for _, posture := range []approval.Posture{
		approval.PostureAtOrAbove, approval.PostureBelow, approval.PostureUnvaluable, approval.PostureAbsent,
	} {
		if got := signatures(t, reg, approval.SingleSigned, string(posture)); got != 0 {
			t.Fatalf("a REFUSED order was counted under posture=%q (%v) — it never went through, on "+
				"one signature or any other number", posture, got)
		}
	}
}

// TestChildrenOfAScheduledParentAreNotCountedAgain. One decision is N orders
// (#435): a scheduled parent plus one child per slice. The parent is the
// decision, and counting each slice would report a single approval-worthy act as
// slice_count single-signed orders.
func TestChildrenOfAScheduledParentAreNotCountedAgain(t *testing.T) {
	fb := &fakeBus{}
	svc, reg, store := dualService(t, fb, dualRat("1000"))

	parent := limitOrder(d(100, 0), d(1025, -2))
	parent.ExecutionSchedule = &orderpb.ExecutionSchedule{
		Algo:        orderpb.ExecutionAlgo_EXECUTION_ALGO_TWAP,
		WindowStart: timestamppb.New(t0),
		WindowEnd:   timestamppb.New(t0.Add(time.Hour)),
		SliceCount:  4,
	}
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, parent)); err != nil {
		t.Fatalf("Handle parent: %v", err)
	}
	if got := signatures(t, reg, approval.SingleSigned, string(approval.PostureAtOrAbove)); got != 1 {
		t.Fatalf("the parent was counted %v times, want 1", got)
	}
	// TOTAL, not just the at_or_above series. A child counted under any label at
	// all — including the empty posture a zero Decision produces — is one decision
	// reported as slice_count+1 single-signed orders.
	before := totalSignatures(t, reg)

	// THE CHILD'S ID IS DERIVED, not chosen: authorizeChild refuses any other, so
	// a hand-written id would be REFUSED and this test would prove nothing about
	// counting. That is how the first version of it passed against a mutant.
	child := limitOrder(d(25, 0), d(1025, -2))
	child.OrderId = schedule.ChildID("o1", 0)
	child.ParentOrderId = "o1"
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, child)); err != nil {
		t.Fatalf("Handle child: %v", err)
	}
	if st, _, err := store.Load(context.Background(), child.GetOrderId()); err != nil || st == nil {
		t.Fatalf("the child was not admitted (%v), so nothing on the counting path ran", err)
	}
	if got := totalSignatures(t, reg); got != before {
		t.Fatalf("a child of an already-decided parent was counted (%v → %v across all series), so "+
			"one decision reports as slice_count+1 single-signed orders", before, got)
	}
}

// TestWithNoThresholdEveryAdmittedOrderIsStillCounted. Absent is a real answer:
// with no threshold configured the control is ABSENT, and that fact has to be
// visible or "this deployment has no dual control" and "this build has no gate"
// look identical.
func TestWithNoThresholdEveryAdmittedOrderIsStillCounted(t *testing.T) {
	fb := &fakeBus{}
	svc, reg, _ := dualService(t, fb, nil)

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, limitOrder(d(100, 0), d(1025, -2)))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := signatures(t, reg, approval.SingleSigned, string(approval.PostureAbsent)); got != 1 {
		t.Fatalf("single_signed/absent = %v, want 1 — with no threshold the metric is the only thing "+
			"saying the control is not there", got)
	}
}

// TestAServiceWithNoDualControlGateStillAdmits. The gate is a test-default-nil
// option, and a control whose absence broke admission would be the outage it
// exists to avoid.
func TestAServiceWithNoDualControlGateStillAdmits(t *testing.T) {
	fb := &fakeBus{}
	svc, _ := newService(t, fb, nil)

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, limitOrder(d(100, 0), d(1025, -2)))); err != nil {
		t.Fatalf("Handle with no dual-control gate: %v", err)
	}
}
