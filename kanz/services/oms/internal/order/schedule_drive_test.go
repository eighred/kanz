package order

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/services/oms/internal/schedule"
)

// AN ORDER IS WORKED AS A SCHEDULE INSTEAD OF SENT WHOLE (#435), end to end.
//
// services/oms/internal/schedule proves the DECISION and internal/execution/algo
// proves the ARITHMETIC, both as pure functions. This file proves the part
// neither can: that admission actually rests a parent, that the driver actually
// creates children through the real admission path, and that cancelling a parent
// actually stops the rest — against the same MemoryStore and the same Service
// every other test in this package uses.
//
// #435's Verified-when, in full:
//
//	(a) child orders sum to exactly N       — TestScheduleE2E_ChildrenSumToTheParent
//	(b) no child exceeds the participation cap — TestScheduleE2E_ACapTheSlicesCannotHonourIsRefusedAtAdmission
//	(c) a cancelled parent cancels every unsent child — TestScheduleE2E_CancellingAParentStopsEveryUnsentChild

var (
	schedStart = time.Date(2026, 8, 15, 14, 0, 0, 0, time.UTC)
	schedEnd   = time.Date(2026, 8, 15, 15, 0, 0, 0, time.UTC)
)

// scheduledOrder is a 60-unit parent worked in 6 slices — one every 10 minutes.
func scheduledOrder(id string, slices uint32, maxSlice *commonpb.Decimal) *orderpb.SubmitOrder {
	return &orderpb.SubmitOrder{
		Metadata:     &commandpb.CommandMetadata{TargetId: id},
		OrderId:      id,
		PortfolioId:  "fund-alpha",
		InstrumentId: "BTC-USD",
		Side:         orderpb.Side_SIDE_BUY,
		Quantity:     d(60, 0),
		OrderType:    orderpb.OrderType_ORDER_TYPE_LIMIT,
		LimitPrice:   d(1025, -2),
		TimeInForce:  orderpb.TimeInForce_TIME_IN_FORCE_GTC,
		ExecutionSchedule: &orderpb.ExecutionSchedule{
			Algo:             orderpb.ExecutionAlgo_EXECUTION_ALGO_TWAP,
			WindowStart:      timestamppb.New(schedStart),
			WindowEnd:        timestamppb.New(schedEnd),
			SliceCount:       slices,
			MaxSliceQuantity: maxSlice,
		},
	}
}

// scheduledService is newService with a controllable clock, so a whole trading
// window can be walked in a test without waiting an hour.
func scheduledService(t *testing.T, fb *fakeBus, at *time.Time) (*Service, *MemoryStore) {
	t.Helper()
	svc, store := newService(t, fb, nil)
	svc.now = func() time.Time { return *at }
	return svc, store
}

// newServiceNoVenue is the harness for the cases that need an order to REST.
// With no router, work() returns before routing and the order stays PENDING_NEW
// — the sim venue fills every priced limit whatever its price, so this is the
// only way to hold a child live.
func newServiceNoVenue(t *testing.T, fb *fakeBus) (*Service, *MemoryStore) {
	t.Helper()
	store := NewMemoryStore()
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, store
}

// driveCtx is what the ticker in cmd/oms/main.go supplies: a tenant, because the
// driver runs on a timer and has no inbound envelope to inherit one from.
func driveCtx() context.Context { return testCtx() }

// ===== THE PARENT RESTS =====

// A SCHEDULED ORDER IS ADMITTED AND THEN RESTS — IT REACHES NO VENUE.
//
// This is the whole premise. Before #435 every order was routed the moment it was
// admitted, in one message, whatever its size. If this assertion ever fails, a
// parent is being sent to a venue WHOLE and the feature is not merely broken —
// it is doing the exact thing it exists to prevent, at the full parent size.
func TestScheduleE2E_AParentIsAdmittedAndRestsAtNoVenue(t *testing.T) {
	at := schedStart.Add(-time.Hour) // before the window opens
	fb := &fakeBus{}
	svc, store := scheduledService(t, fb, &at)

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, scheduledOrder("p1", 6, nil))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	st, _, err := store.Load(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_WORKING_SCHEDULED {
		t.Fatalf("stored status = %v, want WORKING_SCHEDULED", st.GetStatus())
	}
	// NO ORDER_ROUTED, AND NO FILL. The SimVenue in this harness fills a
	// marketable limit immediately, so a parent that reached it would come back
	// FILLED for the whole 60 units — which is precisely the pre-#435 behaviour.
	for _, unwanted := range []string{EventTypeRouted, EventTypeFilled} {
		if contains(fb.types(), unwanted) {
			t.Fatalf("a scheduled parent emitted %s — it went to a venue WHOLE, which is the "+
				"behaviour #435 exists to end. Emitted: %v", unwanted, fb.types())
		}
	}
	if !contains(fb.types(), EventTypeAccepted) {
		t.Errorf("no ORDER_ACCEPTED for the parent: it must be announced like any admitted order, "+
			"or nothing downstream knows the fund has taken this decision. Emitted: %v", fb.types())
	}
	// AND THE SCHEDULE IS DURABLE ON THE ORDER. Everything about how this parent
	// is worked has to survive a restart, because a pod that boots holding only
	// this row must derive the same children as the one it replaced.
	if st.GetExecutionSchedule().GetSliceCount() != 6 {
		t.Errorf("the stored parent does not carry its schedule: %v — a restarted pod could not "+
			"know how this order was meant to be worked", st.GetExecutionSchedule())
	}
}

// THE SWEEP MUST NOT ROUTE A RESTING PARENT.
//
// The guard against routing lives in work(), which the sweep's re-drive path also
// calls. Had it been placed at the admission call site instead, a resting parent
// would survive until the first pod restart and then be routed to a venue as one
// whole order — the defect reintroduced by the machinery that exists to recover
// from crashes, which is exactly the kind of failure that reaches production.
func TestScheduleE2E_TheSweepDoesNotRouteARestingParent(t *testing.T) {
	at := schedStart.Add(-time.Hour)
	fb := &fakeBus{}
	svc, _ := scheduledService(t, fb, &at)

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, scheduledOrder("p1", 6, nil))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// The startup sweep, as cmd/oms/main.go runs it.
	if _, err := svc.SweepInterrupted(driveCtx()); err != nil {
		t.Fatalf("SweepInterrupted: %v", err)
	}
	if contains(fb.types(), EventTypeRouted) || contains(fb.types(), EventTypeFilled) {
		t.Fatalf("the sweep routed a resting parent to a venue: %v", fb.types())
	}
}

// ===== THE DRIVER SENDS CHILDREN =====

// (a) THE CHILDREN SUM TO EXACTLY THE PARENT, having gone through real admission.
func TestScheduleE2E_ChildrenSumToTheParent(t *testing.T) {
	at := schedStart.Add(-time.Hour)
	fb := &fakeBus{}
	svc, store := scheduledService(t, fb, &at)

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, scheduledOrder("p1", 6, nil))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Walk the whole window, one tick per slice boundary.
	at = schedEnd.Add(time.Minute)
	created, err := svc.DriveSchedules(driveCtx())
	if err != nil {
		t.Fatalf("DriveSchedules: %v", err)
	}
	if created != 6 {
		t.Fatalf("driver created %d children, want 6", created)
	}

	children, err := store.ListByParent(context.Background(), "p1")
	if err != nil {
		t.Fatalf("ListByParent: %v", err)
	}
	if len(children) != 6 {
		t.Fatalf("stored %d children, want 6", len(children))
	}
	sum := dec.FromProto(&commonpb.Decimal{})
	for _, c := range children {
		sum.Add(sum, dec.FromProto(c.GetOrderedQuantity()))
		if c.GetParentOrderId() != "p1" {
			t.Errorf("child %s carries parent %q, want p1 — the relation must be durable in the "+
				"direction that can be indexed, or the driver cannot find its own work",
				c.GetOrderId(), c.GetParentOrderId())
		}
	}
	if sum.Cmp(dec.FromProto(d(60, 0))) != 0 {
		t.Fatalf("children sum to %s, want exactly 60 — the parent can never complete, and it "+
			"will rest part-filled with nothing left to send", sum.FloatString(12))
	}
}

// THE DRIVER SENDS ONLY WHAT IS DUE, and picks up where it left off.
func TestScheduleE2E_TheDriverSendsOnlyWhatIsDue(t *testing.T) {
	at := schedStart.Add(-time.Hour)
	fb := &fakeBus{}
	svc, store := scheduledService(t, fb, &at)

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, scheduledOrder("p1", 6, nil))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Before the window opens: nothing.
	if n, err := svc.DriveSchedules(driveCtx()); err != nil || n != 0 {
		t.Fatalf("before the window the driver created %d children (err %v), want 0 — the whole "+
			"point of a window is that the quantity reaches the market spread over it", n, err)
	}

	// 14:25 — slices 0, 1 and 2 are due.
	at = schedStart.Add(25 * time.Minute)
	if n, err := svc.DriveSchedules(driveCtx()); err != nil || n != 3 {
		t.Fatalf("at +25m the driver created %d children (err %v), want 3", n, err)
	}

	// A SECOND TICK AT THE SAME INSTANT CREATES NOTHING. Without this the driver
	// re-trades the whole parent quantity on every tick, forever.
	if n, err := svc.DriveSchedules(driveCtx()); err != nil || n != 0 {
		t.Fatalf("a second tick created %d more children (err %v), want 0 — the driver would "+
			"re-trade the parent's whole quantity on every pass", n, err)
	}

	// And it resumes from where it stopped rather than starting over.
	at = schedEnd.Add(time.Minute)
	if n, err := svc.DriveSchedules(driveCtx()); err != nil || n != 3 {
		t.Fatalf("after the window the driver created %d more children (err %v), want the "+
			"remaining 3", n, err)
	}
	children, _ := store.ListByParent(context.Background(), "p1")
	if len(children) != 6 {
		t.Fatalf("stored %d children after the full window, want exactly 6", len(children))
	}
}

// A CHILD REACHES A VENUE — the half a resting parent must not do.
func TestScheduleE2E_AChildIsRoutedAndFills(t *testing.T) {
	at := schedStart.Add(-time.Hour)
	fb := &fakeBus{}
	svc, store := scheduledService(t, fb, &at)

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, scheduledOrder("p1", 6, nil))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	at = schedStart
	if _, err := svc.DriveSchedules(driveCtx()); err != nil {
		t.Fatalf("DriveSchedules: %v", err)
	}
	if !contains(fb.types(), EventTypeRouted) {
		t.Fatalf("no child reached a venue: %v — the parent rests and the children are what "+
			"trade, so if neither goes anywhere the order does nothing at all", fb.types())
	}
	child, _, err := store.Load(context.Background(), schedule.ChildID("p1", 0))
	if err != nil {
		t.Fatalf("the first child was not stored: %v", err)
	}
	if child.GetStatus() == orderpb.OrderStatus_ORDER_STATUS_WORKING_SCHEDULED {
		t.Fatal("a child rests as if it were itself a parent — nesting has no bound and nothing " +
			"would ever trade")
	}
}

// A CHILD INHERITS ITS PARENT'S ARRIVAL MARK, AND THIS IS WHAT KEEPS TWAP HONEST.
//
// Implementation shortfall is measured against the mark at the moment somebody
// DECIDED to trade. That decision was the parent's. A child stamped with the
// market as it stood twenty minutes into the window would be benchmarked against
// a price that already contains the impact of the earlier slices — so every child
// would report roughly zero shortfall and the parent's true cost would vanish.
//
// The failure flatters and is invisible: the platform would report that working
// orders on a schedule costs nothing, which is the very claim #435 exists to let
// somebody TEST.
func TestScheduleE2E_AChildInheritsTheParentsArrivalMark(t *testing.T) {
	at := schedStart.Add(-time.Hour)
	fb := &fakeBus{}
	// A MARK THAT MOVES WITH THE CLOCK, which is what makes this test capable of
	// failing at all. With no mark source the parent and the child would BOTH
	// carry nothing, and "they match" would be true of two empty fields — a
	// vacuous pass that survives deleting the inheritance entirely.
	marks := &movingMarks{at: &at}
	svc, store := newService(t, fb, nil)
	svc.now = func() time.Time { return at }
	WithArrivalMarks(marks)(svc)

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, scheduledOrder("p1", 6, nil))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	parent, _, err := store.Load(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Load parent: %v", err)
	}
	// NON-VACUITY: the parent must actually HAVE a mark, or everything below
	// compares nothing to nothing.
	if parent.GetArrivalPrice() == nil || parent.GetArrivalAt() == nil {
		t.Fatalf("the parent carries no arrival mark (price=%v at=%v) — this test cannot "+
			"distinguish inheritance from two empty fields",
			parent.GetArrivalPrice(), parent.GetArrivalAt())
	}

	// The clock moves a long way into the window before the child is created —
	// which is exactly when a per-child mark diverges from the parent's.
	at = schedStart.Add(40 * time.Minute)
	if _, err := svc.DriveSchedules(driveCtx()); err != nil {
		t.Fatalf("DriveSchedules: %v", err)
	}
	child, _, err := store.Load(context.Background(), schedule.ChildID("p1", 0))
	if err != nil {
		t.Fatalf("Load child: %v", err)
	}

	if !parent.GetArrivalAt().AsTime().Equal(child.GetArrivalAt().AsTime()) {
		t.Errorf("child arrival_at = %v, parent's = %v — a child benchmarked against a mark from "+
			"inside its own working window reports near-zero shortfall, and the parent's real "+
			"cost disappears from the measure",
			child.GetArrivalAt().AsTime(), parent.GetArrivalAt().AsTime())
	}
	if !proto_equalDecimal(parent.GetArrivalPrice(), child.GetArrivalPrice()) {
		t.Errorf("child arrival_price = %v, parent's = %v — the child was benchmarked against a "+
			"price that already contains the impact of the earlier slices",
			child.GetArrivalPrice(), parent.GetArrivalPrice())
	}
}

// movingMarks is a mark source whose price and observation time track the test
// clock, so a mark taken at admission differs from one taken later. A CONSTANT
// mark would make the inheritance assertion pass whether or not it happened.
type movingMarks struct{ at *time.Time }

func (m *movingMarks) Mark(string) *big.Rat {
	// 100 at the window start, +1 per elapsed minute.
	return new(big.Rat).SetInt64(100 + int64(m.at.Sub(schedStart)/time.Minute))
}

func (m *movingMarks) Lookup(string) (*big.Rat, time.Time, bool) {
	return m.Mark(""), *m.at, true
}

// A CHILD INHERITS THE PARENT'S COLLATERAL ANSWER rather than resolving its own.
// Re-resolving mid-schedule would silently move the remaining slices onto
// different collateral than the ones already filled.
func TestScheduleE2E_AChildInheritsTheParentsVenueAccount(t *testing.T) {
	at := schedStart
	fb := &fakeBus{}
	svc, store := scheduledService(t, fb, &at)

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, scheduledOrder("p1", 6, nil))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, err := svc.DriveSchedules(driveCtx()); err != nil {
		t.Fatalf("DriveSchedules: %v", err)
	}
	parent, _, _ := store.Load(context.Background(), "p1")
	child, _, err := store.Load(context.Background(), schedule.ChildID("p1", 0))
	if err != nil {
		t.Fatalf("Load child: %v", err)
	}
	if child.GetVenueAccountId() != parent.GetVenueAccountId() {
		t.Errorf("child spends account %q, parent resolved %q — one order must have one "+
			"collateral answer, decided once",
			child.GetVenueAccountId(), parent.GetVenueAccountId())
	}
}

// ===== THE PARENT FINISHES =====

// A PARENT WHOSE SCHEDULE IS WORKED OUT IS RETIRED.
//
// Without this it rests at WORKING_SCHEDULED forever — which is the failure this
// whole feature was designed against, reintroduced at the end of it. The order
// shows live on every screen, the driver rescans it on every tick for the life of
// the pod, and nothing anywhere says its work is done.
func TestScheduleE2E_AWorkedOutParentIsRetired(t *testing.T) {
	at := schedStart.Add(-time.Hour)
	fb := &fakeBus{}
	svc, store := scheduledService(t, fb, &at)

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, scheduledOrder("p1", 6, nil))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	at = schedEnd.Add(time.Minute)
	if n, err := svc.DriveSchedules(driveCtx()); err != nil || n != 6 {
		t.Fatalf("precondition: created %d children (err %v), want 6", n, err)
	}
	// THE PARENT IS STILL WORKING ON THE TICK THAT SENT ITS LAST SLICE. Retiring
	// in the same pass would call an order finished in the same breath as putting
	// its final quantity in front of a venue.
	if st, _, _ := store.Load(context.Background(), "p1"); IsTerminal(st) {
		t.Fatal("the parent was retired on the very tick that sent its last slice — its children " +
			"had not yet reached a venue")
	}

	// The next tick: every slice sent, every child finished.
	if n, err := svc.DriveSchedules(driveCtx()); err != nil || n != 0 {
		t.Fatalf("second pass created %d children (err %v), want 0", n, err)
	}

	st, _, err := store.Load(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !IsTerminal(st) {
		t.Fatalf("a fully-worked parent is still %s — it rests forever, the driver rescans it on "+
			"every tick for the life of the pod, and every screen shows it live", st.GetStatus())
	}

	// A PARENT IS NEVER MARKED FILLED, and this is the assertion that keeps the
	// fund's traded volume honest. Its children reported every fill, each with its
	// own fill_id, and those are what the position book and the ledger folded.
	// Copying their quantity onto the parent would be a SECOND record of the same
	// execution, so any consumer summing filled_quantity would double it.
	if st.GetStatus() == orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Error("the parent is marked FILLED — it has no fills of its own, and a consumer summing " +
			"filled quantities across orders would now report the fund as having traded twice " +
			"what it traded")
	}
	if q := dec.FromProto(st.GetFilledQuantity()); q.Sign() != 0 {
		t.Errorf("the parent reports filled_quantity %s — its children already reported those "+
			"fills, so this is a duplicate record of the same execution", q.FloatString(12))
	}

	// AND IT IS ANNOUNCED. A terminal state nothing published leaves every
	// downstream projection showing the order live forever.
	if !contains(fb.types(), EventTypeExpired) {
		t.Errorf("no terminal FACT for the retired parent: %v — the store says finished and every "+
			"projection still says working", fb.types())
	}
}

// A PARENT IS NOT RETIRED WHILE A CHILD IS STILL WORKING.
//
// "Sent" is not "done". A terminal parent is skipped by resume() and both sweeps
// forever after, so retiring early would stop anything from ever revisiting this
// order — while a slice of it is still live and still fillable at an exchange.
func TestScheduleE2E_AParentIsNotRetiredWhileAChildIsStillWorking(t *testing.T) {
	at := schedStart.Add(-time.Hour)
	fb := &fakeBus{}
	// No venue, so every child RESTS at PENDING_NEW rather than filling.
	svc, store := newServiceNoVenue(t, fb)
	svc.now = func() time.Time { return at }

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, scheduledOrder("p1", 6, nil))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	at = schedEnd.Add(time.Minute)
	if n, err := svc.DriveSchedules(driveCtx()); err != nil || n != 6 {
		t.Fatalf("precondition: created %d children (err %v), want 6", n, err)
	}
	// Every slice has been SENT, and every one is still working.
	if _, err := svc.DriveSchedules(driveCtx()); err != nil {
		t.Fatalf("DriveSchedules: %v", err)
	}

	st, _, err := store.Load(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if IsTerminal(st) {
		t.Fatalf("the parent was retired to %s while all six of its children are still working at "+
			"a venue — a terminal parent is skipped by resume() and both sweeps forever after, so "+
			"nothing will revisit this order while its slices go on filling", st.GetStatus())
	}
}

// A PARENT WITH A HOLE IS NOT RETIRED. A count of children cannot tell a hole
// from a short tail; retiring on one would abandon the missing slice's quantity
// permanently, with the parent reported finished.
func TestScheduleE2E_AParentWithAMissingSliceIsNotRetired(t *testing.T) {
	at := schedStart.Add(-time.Hour)
	fb := &fakeBus{}
	svc, store := scheduledService(t, fb, &at)

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, scheduledOrder("p1", 6, nil))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// Only the first three slices are due, so 3, 4 and 5 do not exist yet — which
	// is the same shape as a hole from this decision's point of view.
	at = schedStart.Add(25 * time.Minute)
	if n, err := svc.DriveSchedules(driveCtx()); err != nil || n != 3 {
		t.Fatalf("precondition: created %d children (err %v), want 3", n, err)
	}
	if _, err := svc.DriveSchedules(driveCtx()); err != nil {
		t.Fatalf("DriveSchedules: %v", err)
	}
	st, _, _ := store.Load(context.Background(), "p1")
	if IsTerminal(st) {
		t.Fatalf("a parent with three of six slices sent was retired to %s — the remaining "+
			"quantity is abandoned and the order reads as finished", st.GetStatus())
	}
}

// ===== (c) CANCELLATION =====

// A CANCELLED PARENT CANCELS EVERY UNSENT CHILD — #435's own assertion, wired.
//
// Three slices are working at a venue, an operator pulls the parent, and the
// remaining three must never be created. If this fails, the platform keeps
// putting orders in front of a venue for an order somebody has already withdrawn
// — and a cancel is what people reach for when something is already going wrong.
func TestScheduleE2E_CancellingAParentStopsEveryUnsentChild(t *testing.T) {
	at := schedStart.Add(25 * time.Minute) // slices 0,1,2 due
	fb := &fakeBus{}
	svc, store := scheduledService(t, fb, &at)

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, scheduledOrder("p1", 6, nil))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if n, err := svc.DriveSchedules(driveCtx()); err != nil || n != 3 {
		t.Fatalf("precondition: created %d children (err %v), want 3", n, err)
	}

	// THE OPERATOR PULLS THE PARENT.
	cancel := &orderpb.CancelOrder{
		Metadata: &commandpb.CommandMetadata{
			TargetId: "p1",
			// A real cancel arrives from the gateway carrying the authenticated
			// principal's portfolios. Omitting them is refused NOT_ENTITLED —
			// PortfolioEntitled reads an empty list as entitled to NOTHING.
			PrincipalPortfolios: []string{"fund-alpha"},
		},
		OrderId: "p1",
	}
	if err := svc.Handle(testCtx(), &envelopepb.Envelope{EventType: SubjectCancel},
		mustMarshal(t, cancel)); err != nil {
		t.Fatalf("cancel the parent: %v", err)
	}

	parent, _, err := store.Load(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Load parent: %v", err)
	}
	if parent.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatalf("parent status = %v, want CANCELLED", parent.GetStatus())
	}

	// THE REST OF THE WINDOW ELAPSES AND THE DRIVER RUNS AGAIN. This is the
	// assertion: slices 3, 4 and 5 are now due by the clock, and must never exist.
	at = schedEnd.Add(time.Hour)
	n, err := svc.DriveSchedules(driveCtx())
	if err != nil {
		t.Fatalf("DriveSchedules after the cancel: %v", err)
	}
	if n != 0 {
		t.Fatalf("the driver created %d MORE children after the parent was cancelled — every one "+
			"of them is an order placed at a venue for an order the operator already withdrew", n)
	}
	children, _ := store.ListByParent(context.Background(), "p1")
	if len(children) != 3 {
		t.Fatalf("parent has %d children after the cancel, want the 3 that were already sent", len(children))
	}
}

// AND THE CHILDREN ALREADY AT A VENUE ARE WITHDRAWN TOO.
//
// Clause (c) names UNSENT children, and stopping those is satisfied by the test
// above. But a cancel that stopped only the future slices would leave an operator
// who pulled a 60-unit order with quantity still working at an exchange and
// nothing saying why. Pulling a parent means pulling the whole decision.
func TestScheduleE2E_CancellingAParentWithdrawsItsLiveChildren(t *testing.T) {
	at := schedStart.Add(25 * time.Minute)
	fb := &fakeBus{}
	svc, store := scheduledService(t, fb, &at)

	// A limit far from the market so the SimVenue rests it rather than filling —
	// a filled child is terminal and has nothing left to withdraw, which would
	// make this assertion pass vacuously.
	cmd := scheduledOrder("p1", 6, nil)
	cmd.LimitPrice = d(1, -2)
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if n, err := svc.DriveSchedules(driveCtx()); err != nil || n != 3 {
		t.Fatalf("precondition: created %d children (err %v), want 3", n, err)
	}
	live := 0
	children, _ := store.ListByParent(context.Background(), "p1")
	for _, c := range children {
		if !IsTerminal(c) {
			live++
		}
	}
	if live == 0 {
		t.Skip("the sim venue filled every child immediately; nothing is left resting to withdraw")
	}

	if err := svc.Handle(testCtx(), &envelopepb.Envelope{EventType: SubjectCancel},
		mustMarshal(t, &orderpb.CancelOrder{
			Metadata: &commandpb.CommandMetadata{
				TargetId:            "p1",
				PrincipalPortfolios: []string{"fund-alpha"},
			},
			OrderId: "p1",
		})); err != nil {
		t.Fatalf("cancel the parent: %v", err)
	}

	children, _ = store.ListByParent(context.Background(), "p1")
	for _, c := range children {
		if !IsTerminal(c) {
			t.Errorf("child %s is still %v after its parent was cancelled — the operator pulled "+
				"this order and part of it is still working at an exchange",
				c.GetOrderId(), c.GetStatus())
		}
	}
}

// A CHILD THAT CANNOT BE WITHDRAWN STOPS THE PARENT'S CANCEL.
//
// THIS IS THE ASSERTION THAT A NIL RETURN IS NOT A WITHDRAWAL. handleCancel
// ANSWERS a refusal — NOT_ENTITLED, ORDER_QUARANTINED, ORDER_TERMINAL — by
// publishing a REJECTED outcome and returning nil, because from the bus's point
// of view the command was handled. If cancelChildren trusted that nil, the parent
// would be saved CANCELLED while this slice went on filling at an exchange: a
// successful-looking cancel in the log, an operator told their order was pulled,
// and money still moving. A control that reports success.
//
// A quarantined child is the realistic way to reach it: quarantine means the
// platform could not establish what the venue did, which is exactly when it must
// not claim to have withdrawn anything.
func TestScheduleE2E_AChildThatCannotBeWithdrawnStopsTheParentsCancel(t *testing.T) {
	at := schedStart
	fb := &fakeBus{}
	// NO ROUTER, so the child RESTS instead of being filled on the spot.
	//
	// The sim venue fills every priced limit immediately whatever the price, so an
	// out-of-the-money limit does not produce a resting child — it produces a
	// FILLED one, which is terminal and has nothing left to withdraw. An earlier
	// version of this test skipped in that case, and a skip is how a test that
	// asserts nothing looks exactly like one that passes.
	svc, store := newServiceNoVenue(t, fb)
	svc.now = func() time.Time { return at }

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, scheduledOrder("p1", 6, nil))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, err := svc.DriveSchedules(driveCtx()); err != nil {
		t.Fatalf("DriveSchedules: %v", err)
	}

	childID := schedule.ChildID("p1", 0)
	child, ver, err := store.Load(context.Background(), childID)
	if err != nil {
		t.Fatalf("Load child: %v", err)
	}
	// NON-VACUITY: there must be something live to fail to withdraw.
	if IsTerminal(child) {
		t.Fatalf("the child is already %s — there is nothing live to withdraw, so this test "+
			"could not detect a cancel that silently did nothing", child.GetStatus())
	}
	// FREEZE IT: the platform cannot establish what the venue did with this slice.
	if err := svc.quarantine(testCtx(), child, ver, "test: venue state indeterminate"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}

	cerr := svc.Handle(testCtx(), &envelopepb.Envelope{EventType: SubjectCancel},
		mustMarshal(t, &orderpb.CancelOrder{
			Metadata: &commandpb.CommandMetadata{
				TargetId:            "p1",
				PrincipalPortfolios: []string{"fund-alpha"},
			},
			OrderId: "p1",
		}))
	if cerr == nil {
		t.Fatal("cancelling a parent whose child COULD NOT be withdrawn returned success — the " +
			"operator has been told their order was pulled while a slice of it is still live at " +
			"a venue, and nothing anywhere says otherwise")
	}

	// AND THE PARENT IS NOT MARKED CANCELLED. The whole point of failing is that
	// the redelivery finds the order still live and tries again.
	parent, _, err := store.Load(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Load parent: %v", err)
	}
	if parent.GetStatus() == orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatal("the parent was saved CANCELLED over a child that is still working — a terminal " +
			"parent is skipped by resume() and the sweep forever after, so nothing will ever " +
			"revisit this order")
	}
}

// A CHILD ARRIVING AFTER THE CANCEL IS REFUSED AT ADMISSION.
//
// The driver's check and this one are separated by a network. A child command
// already in flight when the parent is cancelled would otherwise land afterwards
// and trade. This is the check that catches it — the reason clause (c) is
// enforced twice rather than once.
func TestScheduleE2E_AChildArrivingAfterTheCancelIsRefused(t *testing.T) {
	at := schedStart
	fb := &fakeBus{}
	svc, store := scheduledService(t, fb, &at)

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, scheduledOrder("p1", 6, nil))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if err := svc.Handle(testCtx(), &envelopepb.Envelope{EventType: SubjectCancel},
		mustMarshal(t, &orderpb.CancelOrder{
			Metadata: &commandpb.CommandMetadata{
				TargetId:            "p1",
				PrincipalPortfolios: []string{"fund-alpha"},
			},
			OrderId: "p1",
		})); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// The in-flight child, arriving now — spelled exactly as the driver would
	// have spelled it, so nothing but the parent's state can refuse it.
	late := &orderpb.SubmitOrder{
		Metadata:      &commandpb.CommandMetadata{TargetId: schedule.ChildID("p1", 0), Issuer: ScheduleIssuer},
		OrderId:       schedule.ChildID("p1", 0),
		ParentOrderId: "p1",
		PortfolioId:   "fund-alpha",
		InstrumentId:  "BTC-USD",
		Side:          orderpb.Side_SIDE_BUY,
		Quantity:      d(10, 0),
		OrderType:     orderpb.OrderType_ORDER_TYPE_LIMIT,
		LimitPrice:    d(1025, -2),
		TimeInForce:   orderpb.TimeInForce_TIME_IN_FORCE_GTC,
	}
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, late)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, _, err := store.Load(context.Background(), schedule.ChildID("p1", 0)); err == nil {
		t.Fatal("a child of a CANCELLED parent was admitted — it would have been routed to a " +
			"venue for an order the operator already withdrew")
	}
}

// ===== FORGERY =====

// A FORGED CHILD IS REFUSED, and the schedule is what refuses it.
//
// parent_order_id is on the command because children go through the ordinary
// admission path. Nothing stops a client setting it — and nothing needs to: a
// child is admitted only if it is EXACTLY a slice the parent's own durable
// schedule derives. The most valuable forgery, a slice for far more than its
// share, is the case that matters.
func TestScheduleE2E_AForgedChildIsRefusedByTheParentsOwnSchedule(t *testing.T) {
	at := schedStart
	fb := &fakeBus{}
	svc, store := scheduledService(t, fb, &at)

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, scheduledOrder("p1", 6, nil))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	base := func() *orderpb.SubmitOrder {
		return &orderpb.SubmitOrder{
			Metadata:      &commandpb.CommandMetadata{TargetId: schedule.ChildID("p1", 0)},
			OrderId:       schedule.ChildID("p1", 0),
			ParentOrderId: "p1",
			PortfolioId:   "fund-alpha",
			InstrumentId:  "BTC-USD",
			Side:          orderpb.Side_SIDE_BUY,
			Quantity:      d(10, 0),
			OrderType:     orderpb.OrderType_ORDER_TYPE_LIMIT,
			LimitPrice:    d(1025, -2),
			TimeInForce:   orderpb.TimeInForce_TIME_IN_FORCE_GTC,
		}
	}

	tests := []struct {
		name string
		harm string
		mut  func(*orderpb.SubmitOrder)
	}{
		{
			"the whole parent quantity in one slice",
			"the order would go to a venue WHOLE under the authorization of a schedule that said " +
				"to work it in six parts — the exact defect #435 exists to end",
			func(c *orderpb.SubmitOrder) { c.Quantity = d(60, 0) },
		},
		{
			"a slice index the schedule does not have",
			"a seventh slice of a six-slice order is quantity nobody authorized",
			func(c *orderpb.SubmitOrder) {
				c.OrderId = schedule.ChildID("p1", 99)
				c.Metadata.TargetId = c.OrderId
			},
		},
		{
			"a different instrument",
			"the parent's compliance check said nothing about this instrument",
			func(c *orderpb.SubmitOrder) { c.InstrumentId = "ETH-USD" },
		},
		{
			"the opposite side",
			"a sell wearing a buy's authorization",
			func(c *orderpb.SubmitOrder) { c.Side = orderpb.Side_SIDE_SELL },
		},
		{
			"a different portfolio",
			"one fund's order spending another's authorization",
			func(c *orderpb.SubmitOrder) { c.PortfolioId = "fund-beta" },
		},
		{
			"a parent that does not exist",
			"an order claiming authorization from nothing at all",
			func(c *orderpb.SubmitOrder) { c.ParentOrderId = "no-such-parent" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := base()
			tt.mut(cmd)
			if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if _, _, err := store.Load(context.Background(), cmd.GetOrderId()); err == nil {
				t.Fatalf("a forged child was ADMITTED: %s", tt.harm)
			}
		})
	}
}

// A CHILD MAY NOT ITSELF BE SCHEDULED. Nesting has no bound, and it would make
// child ids ambiguous.
func TestScheduleE2E_AChildCannotItselfCarryASchedule(t *testing.T) {
	at := schedStart
	fb := &fakeBus{}
	svc, store := scheduledService(t, fb, &at)

	cmd := scheduledOrder(schedule.ChildID("p1", 0), 6, nil)
	cmd.ParentOrderId = "p1"
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, _, err := store.Load(context.Background(), cmd.GetOrderId()); err == nil {
		t.Fatal("an order carrying BOTH a parent and a schedule was admitted — schedules of " +
			"schedules multiply without bound")
	}
}

// ===== (b) AND THE OTHER ADMISSION REFUSALS =====

// A SCHEDULE THAT CANNOT BE WORKED IS REFUSED AT ADMISSION, not discovered by a
// driver hours later. Every one of these would otherwise be admitted, announced,
// and shown working on every screen while nothing ever traded it.
func TestScheduleE2E_ACapTheSlicesCannotHonourIsRefusedAtAdmission(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*orderpb.SubmitOrder)
	}{
		{
			// (b) OF THE VERIFIED-WHEN: 60 in 6 slices is 10 each, over a cap of 5.
			"a cap the slices cannot honour",
			func(c *orderpb.SubmitOrder) { c.ExecutionSchedule.MaxSliceQuantity = d(5, 0) },
		},
		{
			"no slices",
			func(c *orderpb.SubmitOrder) { c.ExecutionSchedule.SliceCount = 0 },
		},
		{
			// NOT "send it all now" — the two are opposite instructions.
			"a window that does not move forward",
			func(c *orderpb.SubmitOrder) { c.ExecutionSchedule.WindowEnd = c.ExecutionSchedule.WindowStart },
		},
		{
			"an algorithm nobody implemented",
			func(c *orderpb.SubmitOrder) {
				c.ExecutionSchedule.Algo = orderpb.ExecutionAlgo_EXECUTION_ALGO_UNSPECIFIED
			},
		},
		{
			// A client-supplied id containing the child separator: its slices could
			// not be told apart from another parent's, and the store would MERGE
			// them rather than refuse them.
			"an order id that cannot derive unambiguous children",
			func(c *orderpb.SubmitOrder) {
				c.OrderId = "acct:1"
				c.Metadata.TargetId = c.OrderId
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			at := schedStart
			fb := &fakeBus{}
			svc, store := scheduledService(t, fb, &at)

			cmd := scheduledOrder("p1", 6, nil)
			tt.mut(cmd)
			if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if _, _, err := store.Load(context.Background(), cmd.GetOrderId()); err == nil {
				t.Fatal("an unworkable schedule was ADMITTED — it would rest at WORKING_SCHEDULED " +
					"looking live on every screen while nothing ever sliced it")
			}
			// AND THE CALLER IS TOLD WHY. A refusal nobody receives is a hang.
			if !contains(fb.types(), EventTypeRejected) {
				t.Errorf("no ORDER_REJECTED for an unworkable schedule: %v", fb.types())
			}
		})
	}
}

// ===== THE DRIVER'S OWN PRECONDITIONS =====

// THE DRIVER REFUSES TO RUN WITHOUT A TENANT, loudly, rather than failing deep
// inside a child's admission where the symptom would be a parent that never
// progresses.
func TestScheduleE2E_TheDriverRefusesWithoutATenant(t *testing.T) {
	at := schedStart
	svc, _ := scheduledService(t, &fakeBus{}, &at)

	_, err := svc.DriveSchedules(context.Background())
	if err == nil {
		t.Fatal("DriveSchedules ran with no tenant on ctx")
	}
	if !strings.Contains(err.Error(), "bus.WithTenantID") {
		t.Errorf("error = %q, want it to name the fix", err.Error())
	}
}

// THE TICK'S OWN ADEQUACY IS REPORTABLE. A driver ticking more slowly than the
// tightest schedule it works silently coarsens that order, with no other symptom:
// the quantities still sum, no error is raised, and it looks exactly like a slow
// venue.
func TestScheduleE2E_TheTightestSliceIntervalIsReported(t *testing.T) {
	at := schedStart.Add(-time.Hour)
	svc, _ := scheduledService(t, &fakeBus{}, &at)

	// NO PARENTS IS NOT A GAP OF ZERO. A gauge that read zero here would report
	// every deployment as too slow.
	if _, _, ok := svc.TightestSliceInterval(driveCtx()); ok {
		t.Error("TightestSliceInterval reported a gap with no parents being worked")
	}

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, scheduledOrder("p1", 6, nil))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	gap, parents, ok := svc.TightestSliceInterval(driveCtx())
	if !ok || parents != 1 {
		t.Fatalf("got gap=%v parents=%d ok=%v, want one parent", gap, parents, ok)
	}
	if gap != 10*time.Minute {
		t.Errorf("gap = %v, want 10m (an hour in 6 slices)", gap)
	}
}

// --- helpers ---

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func proto_equalDecimal(a, b *commonpb.Decimal) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return dec.FromProto(a).Cmp(dec.FromProto(b)) == 0
}
