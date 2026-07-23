# OMS Crash-Safe ROUTED Reconciliation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a redelivered or crash-interrupted `SubmitOrder` RESUME against venue truth instead of silently acking, so an order can never be stranded at `ROUTED` with nothing working it — and can never be re-driven into a double trade.

**Architecture:** Three parts, each following a pattern this repo already established. (1) `OrderState` gains `venue_ack_at` — the record of whether the venue ever confirmed it had the order — plus a `quarantine` block for orders whose truth cannot be established. (2) `execution.Querier` becomes a new OPTIONAL venue capability, type-asserted exactly the way `Closer` and `SelfHealing` already are; the unexported `queryOrder` that Binance and OKX reconcilers already call is the proof this shape is right. (3) A pure `Reconcile` policy function maps (venue answer × `venue_ack_at`) to one of four actions — re-drive, adopt, leave, quarantine — and is driven from two places: the `handleSubmit` redelivery path, and a startup sweep that runs before consumers subscribe (the `tv-sync` `Rehydrate` precedent).

**Tech Stack:** Go 1.x, protobuf/buf, NATS JetStream (`pkg/bus`), Postgres (pgx), Prometheus.

## Global Constraints

- **Scope is the SIM PATH.** `SimVenue` gets `QueryOrder`. The Binance/OKX adapters do NOT — their `queryOrder` is an unexported REST method behind an out-of-process gRPC boundary whose `VenueAdapterService` (`kanz-schemas/proto/venue/v1/venue.proto:41-78`) exposes only `Execute`, `CancelOrder`, `Describe`. Surfacing it is a separate, later change.
- **A venue that does not implement `Querier` QUARANTINES.** It never re-drives and never abandons. Refusing to guess is the established stance (ONBOARD-M5).
- **Zero values must be the SAFE answer.** `OrderViewState(0)` is `OrderViewIndeterminate` and `Action(0)` is `ActionQuarantine`. This follows `AccountProof`, whose doc says "The ZERO VALUE IS UNVERIFIED, deliberately. An adapter that never checks reports the safe answer rather than the flattering one." A zero value that means "the venue does not have this order" would make a dropped field into a double trade.
- **No Postgres migration is needed for the new proto fields.** `orders.state` is `BYTEA` holding marshaled `order.v1.OrderState` (`services/oms/migrations/0001_orders.sql:33-54`); new fields ride along. Task 7 adds one column-free query method that uses the EXISTING `orders_status_idx`.
- **`kanz-schemas/gen/go` is generated-not-committed (EVT-15a).** Regenerate with `make generate` from `kanz/` (which runs `cd ../kanz-schemas && buf generate`). `GOFLAGS := -mod=mod` is already pinned in `kanz/Makefile:6-8` for exactly this.
- **Do NOT lift the `bus.WithRetry` ban.** `test/arch/bus_dlq_test.go` bans it estate-wide across 13 consumers in 12 services. This plan makes exactly ONE handler resumable. The guard stays; Task 6 corrects the rationale text so it stops asserting something that is no longer true of the OMS.
- **`proto.Clone` for every state copy.** `cloneState` (`aggregate.go:248`) is deliberately `proto.Clone` because a hand-rolled copy silently dropped `venue_account_id`. Never hand-roll a copy of `OrderState`.
- **Fill identity must be STABLE across a re-query.** `position_fills` dedups on `fill_id` (`services/oms/migrations/0003_positions.sql:58-63`, claimed by `INSERT ... ON CONFLICT DO NOTHING` at `services/oms/internal/position/postgres.go:121-125`). A venue that invents a NEW `fill_id` when re-queried defeats exactly-once and double-counts the position. This is why Task 3 makes `SimVenue` remember.
- **Run all Go commands from `kanz/`.** Test command shape: `cd kanz && go test ./services/oms/internal/order/ -run TestName -v`.

---

### Task 1: Record whether the venue ever saw the order

**Files:**
- Modify: `kanz-schemas/proto/order/v1/order_events.proto:47-97` (the `OrderState` message; highest field number in use today is 15)

**Interfaces:**
- Produces: `OrderState.venue_ack_at` (`*timestamppb.Timestamp`, field 16), `OrderState.quarantine` (`*orderpb.OrderQuarantine`, field 17), and the new message `order.v1.OrderQuarantine` with fields `at`, `reason`, `last_query_at`. Every later task depends on these accessors: `st.GetVenueAckAt()`, `st.GetQuarantine()`, `q.GetReason()`, `q.GetAt()`, `q.GetLastQueryAt()`.

- [ ] **Step 1: Add the two fields to `OrderState`**

In `kanz-schemas/proto/order/v1/order_events.proto`, immediately after the `venue_account_id = 15;` field and before the closing `}` of `message OrderState`, add:

```protobuf
  // venue_ack_at is WHEN THE VENUE CONFIRMED IT HELD THIS ORDER — the one fact
  // that separates a recovering OMS from a guessing one.
  //
  // It is stamped only after Venue.Execute returns WITHOUT error, which is the
  // venue saying "I have it". Unset means the OMS saved the order as ROUTED and
  // never received that confirmation — the venue may hold it, or may never have
  // seen it, and a FAILED Execute does not tell those apart.
  //
  // Reconciliation reads it in exactly ONE place: when the venue answers "I do
  // not know this order". Unset then means the venue genuinely never got it and
  // working it now is safe. SET means the venue confirmed it and is now denying
  // it — not a recoverable state but a contradiction between two authorities —
  // and the order is quarantined rather than re-driven. Re-driving that case is
  // a double trade.
  google.protobuf.Timestamp venue_ack_at = 16;

  // quarantine is set when reconciliation could not safely establish what the
  // venue did with this order. A quarantined order is FROZEN: nothing re-drives
  // it, nothing cancels it, and clearing it takes a human. Unset on every
  // healthy order.
  OrderQuarantine quarantine = 17;
```

- [ ] **Step 2: Add the `OrderQuarantine` message**

In the same file, immediately AFTER the closing `}` of `message OrderState`, add:

```protobuf
// OrderQuarantine records an order whose true state at the venue could not be
// established.
//
// It exists because the only alternative to freezing is guessing, and both
// guesses are unacceptable on the capital path: re-driving may trade the fund
// twice, and abandoning may leave a live exchange order nobody is watching. The
// platform states that it does not know, loudly, and stops.
message OrderQuarantine {
  // at is when the order was quarantined. Required when quarantine is set.
  google.protobuf.Timestamp at = 1;

  // reason is the human-readable account of what could not be established, and
  // it is what an operator reads first. It must name the venue's answer and the
  // venue_ack_at state that made the pair irreconcilable.
  string reason = 2;

  // last_query_at is when the venue was last asked about this order — so an
  // operator can tell a fresh contradiction from a stale one.
  google.protobuf.Timestamp last_query_at = 3;
}
```

- [ ] **Step 3: Regenerate the SDK and confirm the accessors exist**

```bash
cd kanz && make generate
```

Then confirm the generated Go actually carries them:

```bash
grep -n "func (x \*OrderState) GetVenueAckAt\|func (x \*OrderState) GetQuarantine\|type OrderQuarantine struct" ../kanz-schemas/gen/go/order/v1/order_events.pb.go
```

Expected: all three lines present. If `make generate` fails because `buf` is missing, stop and raise it — do not hand-edit `gen/go`, which is generated-not-committed and would be erased by the next regeneration.

- [ ] **Step 4: Confirm nothing broke**

```bash
cd kanz && go build ./... && go test ./services/oms/... 2>&1 | tail -20
```

Expected: build succeeds, existing OMS tests still pass. Adding optional proto fields is additive; a failure here means something reads `OrderState` positionally and must be found before going further.

- [ ] **Step 5: Commit**

```bash
git add kanz-schemas/proto/order/v1/order_events.proto
git commit -m "feat(schema): record whether the venue ever acknowledged an order

An order saved as ROUTED whose Execute then failed is indistinguishable, in the
store, from a limit order resting normally at the exchange. venue_ack_at is the
fact that tells those apart, and it is what makes resuming a redelivered order
safe rather than a double trade. quarantine freezes the case where the venue
confirmed an order and later denies it — a contradiction between two
authorities, which is not recoverable by guessing.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: `execution.Querier` — the optional venue capability to ask what the venue did

**Files:**
- Create: `kanz/internal/execution/query.go`
- Test: `kanz/internal/execution/query_test.go`

**Interfaces:**
- Produces:
  - `type OrderViewState int` with constants `OrderViewIndeterminate` (0), `OrderViewUnknown`, `OrderViewWorking`, `OrderViewPartiallyFilled`, `OrderViewFilled`, `OrderViewRejected`, and method `func (s OrderViewState) String() string`.
  - `type OrderView struct { State OrderViewState; Fills []*orderpb.Fill; Reason string }`
  - `type Querier interface { QueryOrder(ctx context.Context, st *orderpb.OrderState) (OrderView, error) }`

**Why a new optional interface and not a `Venue` method:** `Venue` has exactly three methods (`MIC`, `Account`, `Execute`) and every adapter implements all three. `Closer` (`venue.go:76-78`) and `SelfHealing` (`venue.go:91-94`) are the established pattern for a capability only some venues have, and the OMS already type-asserts for them in `service.go:423`. Querying is exactly that shape: `SimVenue` can answer, and a `GRPCVenue` cannot until the wire contract gains an RPC.

- [ ] **Step 1: Write the failing test**

Create `kanz/internal/execution/query_test.go`:

```go
package execution

import (
	"context"
	"testing"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
)

// THE ZERO VALUE MUST BE THE SAFE ANSWER.
//
// A caller that constructs an OrderView and forgets to set State, or a venue
// that returns OrderView{} on a path nobody thought about, must produce the
// answer that FREEZES the order — never the one that says "the venue does not
// have it", which is the answer that authorizes re-driving it to the exchange.
// This mirrors AccountProof (venue.go:50): the unchecked adapter reports the
// safe answer, not the flattering one.
func TestZeroOrderViewIsIndeterminateNotUnknown(t *testing.T) {
	var v OrderView
	if v.State != OrderViewIndeterminate {
		t.Fatalf("zero OrderView.State is %v, want OrderViewIndeterminate", v.State)
	}
	if OrderViewIndeterminate == OrderViewUnknown {
		t.Fatal("OrderViewIndeterminate and OrderViewUnknown are the same value — " +
			"then a forgotten field means 'the venue never saw this order', which is " +
			"the one answer that authorizes sending it to an exchange again")
	}
}

// Every state must print as something an operator can read in a quarantine
// reason. A bare integer in an incident log costs the reader a trip to the
// source at the worst possible moment.
func TestOrderViewStateStrings(t *testing.T) {
	for _, tc := range []struct {
		state OrderViewState
		want  string
	}{
		{OrderViewIndeterminate, "INDETERMINATE"},
		{OrderViewUnknown, "UNKNOWN"},
		{OrderViewWorking, "WORKING"},
		{OrderViewPartiallyFilled, "PARTIALLY_FILLED"},
		{OrderViewFilled, "FILLED"},
		{OrderViewRejected, "REJECTED"},
		{OrderViewState(99), "OrderViewState(99)"},
	} {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("OrderViewState(%d).String() = %q, want %q", tc.state, got, tc.want)
		}
	}
}

// queryingVenue proves the interface is satisfiable by an ordinary venue and
// that a Venue can be type-asserted to Querier — the assertion the OMS makes.
type queryingVenue struct {
	Venue
	view OrderView
}

func (q queryingVenue) QueryOrder(context.Context, *orderpb.OrderState) (OrderView, error) {
	return q.view, nil
}

func TestQuerierIsTypeAssertableFromVenue(t *testing.T) {
	var v Venue = queryingVenue{Venue: NewSimVenue("XSIM"), view: OrderView{State: OrderViewWorking}}
	q, ok := v.(Querier)
	if !ok {
		t.Fatal("a Venue implementing QueryOrder does not satisfy Querier")
	}
	got, err := q.QueryOrder(context.Background(), &orderpb.OrderState{OrderId: "o-1"})
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if got.State != OrderViewWorking {
		t.Fatalf("view state = %v, want OrderViewWorking", got.State)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd kanz && go test ./internal/execution/ -run 'TestZeroOrderView|TestOrderViewStateStrings|TestQuerierIsTypeAssertable' -v
```

Expected: FAIL to compile — `undefined: OrderView`, `undefined: OrderViewIndeterminate`, `undefined: Querier`.

- [ ] **Step 3: Write the implementation**

Create `kanz/internal/execution/query.go`:

```go
package execution

import (
	"context"
	"fmt"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
)

// OrderViewState is what a venue says it knows about one order.
//
// THE ZERO VALUE IS INDETERMINATE, DELIBERATELY — the same stance AccountProof
// takes. The dangerous answer is "I have no such order", because that is the one
// that authorizes sending the order to the exchange again. It must never be
// something a caller can produce by forgetting to set a field.
type OrderViewState int

const (
	// OrderViewIndeterminate means nothing was established. The venue did not
	// answer, answered in a way this adapter cannot map, or was never asked.
	// It resolves to quarantine, never to an action.
	OrderViewIndeterminate OrderViewState = iota
	// OrderViewUnknown means the venue AFFIRMATIVELY has no record of this
	// order. This is a positive statement by the venue, not the absence of one.
	OrderViewUnknown
	// OrderViewWorking means the venue holds the order and it is live.
	OrderViewWorking
	// OrderViewPartiallyFilled means the venue holds the order and it has
	// partially traded. Fills carries what traded.
	OrderViewPartiallyFilled
	// OrderViewFilled means the order is fully executed. Fills carries it.
	OrderViewFilled
	// OrderViewRejected means the venue terminally refused the order.
	OrderViewRejected
)

func (s OrderViewState) String() string {
	switch s {
	case OrderViewIndeterminate:
		return "INDETERMINATE"
	case OrderViewUnknown:
		return "UNKNOWN"
	case OrderViewWorking:
		return "WORKING"
	case OrderViewPartiallyFilled:
		return "PARTIALLY_FILLED"
	case OrderViewFilled:
		return "FILLED"
	case OrderViewRejected:
		return "REJECTED"
	default:
		return fmt.Sprintf("OrderViewState(%d)", int(s))
	}
}

// OrderView is a venue's answer about one order.
//
// Fills MUST carry the venue's OWN fill identities — the same fill_id the
// original Execute reported, not freshly minted ones. The position book dedups
// folds on fill_id (position_fills), so a venue that renames its fills when
// re-queried turns exactly-once into double-counting.
type OrderView struct {
	// State is the venue's verdict. Zero value quarantines.
	State OrderViewState
	// Fills are the executions the venue attributes to this order, for the
	// PARTIALLY_FILLED and FILLED states. Empty otherwise.
	Fills []*orderpb.Fill
	// Reason is the venue's own words for a REJECTED or INDETERMINATE answer,
	// carried into the quarantine record an operator reads.
	Reason string
}

// Querier is the optional Venue capability to ask "do you hold this order, and
// what did you do with it?".
//
// It is separate from Venue for the same reason Closer is: not every venue can
// answer. SimVenue can, because it remembers what it executed. An out-of-process
// GRPCVenue cannot, because venue.v1.VenueAdapterService exposes only Execute,
// CancelOrder and Describe — the Binance and OKX REST clients each have a
// private queryOrder their own reconcilers call, and nothing surfaces it across
// the process boundary. Until that RPC exists, those venues are not Queriers and
// the OMS quarantines rather than guessing on their behalf.
//
// QueryOrder addresses the order by st.order_id — the same deterministic
// clOrdId the submit used — so it is safe to call repeatedly.
//
// ERROR DISCIPLINE: a returned error means THE QUESTION COULD NOT BE ASKED (the
// venue was unreachable, rate-limited, timed out). That is transient: the caller
// backs off and asks again. It is NOT the same as an OrderViewUnknown answer,
// which is the venue positively stating it has no such order. Collapsing those
// two turns a network blip into a re-driven order.
type Querier interface {
	QueryOrder(ctx context.Context, st *orderpb.OrderState) (OrderView, error)
}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
cd kanz && go test ./internal/execution/ -run 'TestZeroOrderView|TestOrderViewStateStrings|TestQuerierIsTypeAssertable' -v
```

Expected: all three PASS.

- [ ] **Step 5: Commit**

```bash
git add kanz/internal/execution/query.go kanz/internal/execution/query_test.go
git commit -m "feat(execution): add Querier, the optional venue capability to ask what a venue did

Follows the Closer/SelfHealing pattern for a capability only some venues have.
The zero OrderViewState is INDETERMINATE, not UNKNOWN, so a forgotten field
freezes an order rather than authorizing it back to an exchange.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: Make `SimVenue` able to answer — and idempotent at the venue

**Files:**
- Modify: `kanz/internal/execution/venue.go:104-174` (the `SimVenue` struct and `Execute`)
- Test: `kanz/internal/execution/simvenue_query_test.go`

**Interfaces:**
- Consumes: `Querier`, `OrderView`, `OrderViewUnknown`, `OrderViewFilled` (Task 2)
- Produces: `func (v *SimVenue) QueryOrder(ctx context.Context, st *orderpb.OrderState) (OrderView, error)`, and an `Execute` that returns the SAME fills (same `fill_id`) when called twice for one `order_id`.

**Why `SimVenue` must become stateful:** it holds no per-order state today — every `Execute` mints a fresh `fill_id` from `v.newID()`. That makes it incapable of answering `QueryOrder` at all, and it makes a re-drive mint a SECOND fill for the same execution, which `position_fills` cannot dedup because the id differs. A real exchange dedups on `clOrdId`; the simulator must model that, or the sim path proves nothing about the real one.

- [ ] **Step 1: Write the failing test**

Create `kanz/internal/execution/simvenue_query_test.go`:

```go
package execution

import (
	"context"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
)

func simOrder(id string) *orderpb.OrderState {
	return &orderpb.OrderState{
		OrderId:        id,
		InstrumentId:   "BTC-USD",
		Side:           orderpb.Side_SIDE_BUY,
		OrderType:      orderpb.OrderType_ORDER_TYPE_LIMIT,
		LimitPrice:     &commonpb.Decimal{Coefficient: 1025, Exponent: -2},
		LeavesQuantity: &commonpb.Decimal{Coefficient: 100, Exponent: 0},
	}
}

// A venue that has never seen an order must say so AFFIRMATIVELY — that is the
// answer the reconciler needs in order to work the order safely.
func TestSimVenueQueryUnknownOrder(t *testing.T) {
	v := NewSimVenue("XSIM")
	got, err := v.QueryOrder(context.Background(), simOrder("never-sent"))
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if got.State != OrderViewUnknown {
		t.Fatalf("state = %v, want OrderViewUnknown", got.State)
	}
}

// After Execute, the venue must remember the order AND report the very same
// fills — same fill_id — because position_fills dedups on fill_id. A venue that
// renames its fills on re-query double-counts the position book.
func TestSimVenueQueryReturnsTheSameFillsExecuteReported(t *testing.T) {
	ctx := context.Background()
	v := NewSimVenue("XSIM", WithClock(func() time.Time { return time.Unix(0, 0).UTC() }))
	st := simOrder("o-1")

	fills, err := v.Execute(ctx, st)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(fills) != 1 {
		t.Fatalf("Execute returned %d fills, want 1", len(fills))
	}

	got, err := v.QueryOrder(ctx, st)
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if got.State != OrderViewFilled {
		t.Fatalf("state = %v, want OrderViewFilled", got.State)
	}
	if len(got.Fills) != 1 {
		t.Fatalf("QueryOrder returned %d fills, want 1", len(got.Fills))
	}
	if got.Fills[0].GetFillId() != fills[0].GetFillId() {
		t.Fatalf("QueryOrder fill_id %q != Execute fill_id %q — the position book "+
			"dedups on fill_id, so a re-queried fill under a new id is folded twice",
			got.Fills[0].GetFillId(), fills[0].GetFillId())
	}
}

// A real exchange dedups a resubmitted clOrdId rather than opening a second
// order. The simulator must too, or a re-drive on the sim path trades twice
// while the sim path is exactly what we use to prove the real one.
func TestSimVenueExecuteIsIdempotentPerOrderID(t *testing.T) {
	ctx := context.Background()
	v := NewSimVenue("XSIM")
	st := simOrder("o-1")

	first, err := v.Execute(ctx, st)
	if err != nil {
		t.Fatalf("Execute #1: %v", err)
	}
	second, err := v.Execute(ctx, st)
	if err != nil {
		t.Fatalf("Execute #2: %v", err)
	}
	if len(second) != len(first) {
		t.Fatalf("Execute #2 returned %d fills, #1 returned %d", len(second), len(first))
	}
	if second[0].GetFillId() != first[0].GetFillId() {
		t.Fatalf("Execute #2 minted a new fill_id %q (first was %q) — a resubmitted "+
			"order id must not become a second execution", second[0].GetFillId(), first[0].GetFillId())
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd kanz && go test ./internal/execution/ -run TestSimVenue -v
```

Expected: FAIL to compile — `v.QueryOrder undefined (type *SimVenue has no field or method QueryOrder)`.

- [ ] **Step 3: Add the execution record to `SimVenue`**

In `kanz/internal/execution/venue.go`, replace the `SimVenue` struct (currently at lines 104-110) with:

```go
// SimVenue is the in-process simulation venue. It fills a marketable order in
// full, in one fill, at the order's limit price (LIMIT/STOP_LIMIT) or the
// resolved mark price (MARKET). Deterministic given its clock + id generator.
//
// IT REMEMBERS WHAT IT EXECUTED, and that is not a convenience. A real exchange
// dedups a resubmitted clOrdId and can be asked what it did with an order; a
// simulator that does neither cannot stand in for one on the crash-recovery
// path, which is precisely the path we use it to prove. Without the record,
// re-driving an order after a crash mints a SECOND fill_id for the same
// execution — and position_fills, which dedups on fill_id, folds it twice.
type SimVenue struct {
	mic     string
	account string
	price   PriceFunc
	now     func() time.Time
	newID   func() string

	// mu guards executed. SimVenue is shared by every goroutine handling orders
	// for this MIC, so the record is concurrent by construction.
	mu sync.Mutex
	// executed maps order_id → the fills this venue reported for it. Unbounded
	// by design: it is a simulator, its lifetime is a process, and forgetting an
	// order would resurrect the exact bug this record exists to close.
	executed map[string][]*orderpb.Fill
}
```

Then in `NewSimVenue` (currently `venue.go:130`), initialise the map — replace the construction line:

```go
func NewSimVenue(mic string, opts ...SimOption) *SimVenue {
	v := &SimVenue{
		mic:      mic,
		account:  "sim:" + mic,
		now:      time.Now,
		newID:    uuid.NewString,
		executed: make(map[string][]*orderpb.Fill),
	}
	for _, opt := range opts {
		opt(v)
	}
	return v
}
```

Add `"sync"` to the file's import block if it is not already there.

- [ ] **Step 4: Make `Execute` idempotent and record its fills**

In `kanz/internal/execution/venue.go`, replace the body of `Execute` (currently lines 145-174) with:

```go
// Execute fills the open quantity in full at the resolved price. A second call
// for an order id it has already executed returns THE SAME fills rather than
// executing again — the behaviour a real exchange's clOrdId dedup gives us, and
// the behaviour crash recovery depends on.
func (v *SimVenue) Execute(_ context.Context, st *orderpb.OrderState) ([]*orderpb.Fill, error) {
	if st == nil {
		return nil, errors.New("execution: nil order state")
	}

	v.mu.Lock()
	if prior, ok := v.executed[st.GetOrderId()]; ok {
		v.mu.Unlock()
		return prior, nil
	}
	v.mu.Unlock()

	price := v.executionPrice(st)
	if price == nil || dec.IsZero(price) {
		// PERMANENT, not transient: this venue has no price source for this order
		// type and will not acquire one at runtime, so every retry resolves the
		// same way. It used to return (nil, nil) — "no fill" — which left the
		// order RESTING FOREVER and indistinguishable from a working limit order.
		// A capital-path no-op that looks like normal operation is the wrong
		// failure direction; the caller must refuse the order, not re-queue it.
		//
		// Nothing is recorded here: an order this venue refused to price was
		// never executed, so a later query must answer UNKNOWN, not FILLED.
		return nil, fmt.Errorf("%w: %s has no price source for order type %s",
			ErrUnpriced, v.mic, st.GetOrderType())
	}
	fill := &orderpb.Fill{
		FillId:       v.newID(),
		OrderId:      st.GetOrderId(),
		InstrumentId: st.GetInstrumentId(),
		Side:         st.GetSide(),
		Quantity:     st.GetLeavesQuantity(),
		Price:        price,
		Venue:        v.mic,
		// The account that ACTUALLY executed — the venue's own, not the order's
		// intent. The ledger posts where the cash moved, not where it was meant to.
		VenueAccountId: v.account,
		ExecutedAt:     timestamppb.New(v.now().UTC()),
	}
	fills := []*orderpb.Fill{fill}

	v.mu.Lock()
	defer v.mu.Unlock()
	// Re-check under the lock: two concurrent Executes of one order id must
	// produce ONE execution, and the loser adopts the winner's fills. Returning
	// its own would be the double trade in miniature.
	if prior, ok := v.executed[st.GetOrderId()]; ok {
		return prior, nil
	}
	v.executed[st.GetOrderId()] = fills
	return fills, nil
}
```

- [ ] **Step 5: Add `QueryOrder`**

In `kanz/internal/execution/venue.go`, immediately after `Execute`, add:

```go
// QueryOrder answers what this venue did with an order — the Querier capability.
//
// SimVenue fills in full or not at all, so an order it remembers is FILLED and
// an order it does not is UNKNOWN. UNKNOWN here is an AFFIRMATIVE statement:
// this venue keeps a complete record for its process lifetime, so its silence
// about an order really does mean it never executed one.
func (v *SimVenue) QueryOrder(_ context.Context, st *orderpb.OrderState) (OrderView, error) {
	if st == nil {
		return OrderView{}, errors.New("execution: nil order state")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	fills, ok := v.executed[st.GetOrderId()]
	if !ok {
		return OrderView{State: OrderViewUnknown}, nil
	}
	return OrderView{State: OrderViewFilled, Fills: fills}, nil
}
```

- [ ] **Step 6: Run the tests to verify they pass**

```bash
cd kanz && go test ./internal/execution/ -run TestSimVenue -v
```

Expected: all three PASS.

Then confirm nothing that already depended on `SimVenue` regressed:

```bash
cd kanz && go test ./internal/execution/ ./services/oms/... 2>&1 | tail -20
```

Expected: all packages PASS. If a pre-existing OMS test now fails because `SimVenue` no longer re-fills a repeated order id, read it carefully — that is the behaviour change this task intends, and the test's assertion, not this code, is what should be revisited.

- [ ] **Step 7: Commit**

```bash
git add kanz/internal/execution/venue.go kanz/internal/execution/simvenue_query_test.go
git commit -m "feat(execution): SimVenue remembers what it executed and can be queried

A simulator that mints a fresh fill_id every time it is asked to execute the
same order cannot stand in for an exchange on the crash-recovery path — and
position_fills, which dedups on fill_id, would fold the re-driven fill twice.
SimVenue now dedups per order_id like a real clOrdId and implements Querier.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: The reconciliation policy, as a pure function

**Files:**
- Create: `kanz/services/oms/internal/order/reconcile.go`
- Test: `kanz/services/oms/internal/order/reconcile_test.go`

**Interfaces:**
- Consumes: `execution.OrderView`, `execution.OrderViewState` constants (Task 2); `OrderState.GetVenueAckAt()` (Task 1)
- Produces:
  - `type Action int` with constants `ActionQuarantine` (0), `ActionRedrive`, `ActionAdopt`, `ActionLeave`, and `func (a Action) String() string`
  - `func Reconcile(st *orderpb.OrderState, view execution.OrderView) (Action, string)` — returns the action and, when it is `ActionQuarantine`, the reason to record.

**Why pure:** the aggregate is pure by design (`aggregate.go:1-6`: "no I/O — so the state transitions are unit-testable in isolation and identical on the live path and any replay"). The policy is the part that must be exhaustively certified, so it takes a state and an answer and returns a decision, with every row of the table individually tested.

- [ ] **Step 1: Write the failing test**

Create `kanz/services/oms/internal/order/reconcile_test.go`:

```go
package order

import (
	"testing"
	"time"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/internal/execution"
)

func routedOrder(ackedAt *timestamppb.Timestamp) *orderpb.OrderState {
	return &orderpb.OrderState{
		OrderId:    "o-1",
		Status:     orderpb.OrderStatus_ORDER_STATUS_ROUTED,
		VenueAckAt: ackedAt,
	}
}

// THE WHOLE POLICY, ONE ROW AT A TIME.
//
// Every row here is a decision about somebody's money. The two that matter most
// are the UNKNOWN pair: the venue saying "I have no such order" means work it if
// we never had an acknowledgement, and means FREEZE if we did — because a venue
// that confirmed an order and now denies it is not a venue we can safely act on,
// and re-driving it is how a fund trades twice.
func TestReconcilePolicy(t *testing.T) {
	acked := timestamppb.New(time.Unix(1000, 0).UTC())

	for _, tc := range []struct {
		name  string
		st    *orderpb.OrderState
		view  execution.OrderView
		want  Action
	}{
		{
			name: "venue never saw it and we never had an ack — work it",
			st:   routedOrder(nil),
			view: execution.OrderView{State: execution.OrderViewUnknown},
			want: ActionRedrive,
		},
		{
			name: "venue denies an order it acknowledged — freeze",
			st:   routedOrder(acked),
			view: execution.OrderView{State: execution.OrderViewUnknown},
			want: ActionQuarantine,
		},
		{
			name: "venue is working it, no ack recorded — leave it, stamp the ack",
			st:   routedOrder(nil),
			view: execution.OrderView{State: execution.OrderViewWorking},
			want: ActionLeave,
		},
		{
			name: "venue is working it, ack recorded — leave it",
			st:   routedOrder(acked),
			view: execution.OrderView{State: execution.OrderViewWorking},
			want: ActionLeave,
		},
		{
			name: "venue filled it — adopt its fills",
			st:   routedOrder(nil),
			view: execution.OrderView{State: execution.OrderViewFilled},
			want: ActionAdopt,
		},
		{
			name: "venue partially filled it — adopt its fills",
			st:   routedOrder(acked),
			view: execution.OrderView{State: execution.OrderViewPartiallyFilled},
			want: ActionAdopt,
		},
		{
			name: "venue rejected it — adopt the rejection",
			st:   routedOrder(acked),
			view: execution.OrderView{State: execution.OrderViewRejected},
			want: ActionAdopt,
		},
		{
			name: "nothing was established — freeze",
			st:   routedOrder(nil),
			view: execution.OrderView{State: execution.OrderViewIndeterminate},
			want: ActionQuarantine,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := Reconcile(tc.st, tc.view)
			if got != tc.want {
				t.Fatalf("Reconcile = %v, want %v", got, tc.want)
			}
			if got == ActionQuarantine && reason == "" {
				t.Error("quarantine with an empty reason — an operator opening this " +
					"order sees a frozen order and no account of why")
			}
		})
	}
}

// The zero Action must be the one that freezes. A switch that falls through, or
// a caller that forgets to assign, must not produce "send it to the exchange".
func TestZeroActionIsQuarantine(t *testing.T) {
	var a Action
	if a != ActionQuarantine {
		t.Fatalf("zero Action is %v, want ActionQuarantine", a)
	}
}

// A terminal order is nobody's to reconcile. Reaching this function with one is
// a caller bug, and answering "re-drive" would re-trade a filled order.
func TestReconcileRefusesTerminalOrders(t *testing.T) {
	st := &orderpb.OrderState{
		OrderId: "o-1",
		Status:  orderpb.OrderStatus_ORDER_STATUS_FILLED,
	}
	got, reason := Reconcile(st, execution.OrderView{State: execution.OrderViewUnknown})
	if got != ActionQuarantine {
		t.Fatalf("Reconcile on a FILLED order = %v, want ActionQuarantine (got reason %q)", got, reason)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd kanz && go test ./services/oms/internal/order/ -run 'TestReconcile|TestZeroAction' -v
```

Expected: FAIL to compile — `undefined: Reconcile`, `undefined: ActionQuarantine`.

- [ ] **Step 3: Write the implementation**

Create `kanz/services/oms/internal/order/reconcile.go`:

```go
package order

import (
	"fmt"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"

	"github.com/kanz-eng/kanz/internal/execution"
)

// Action is what to do with an order whose work was interrupted.
//
// THE ZERO VALUE FREEZES. Every other value causes something to happen to
// somebody's capital, so the value a caller gets by forgetting to assign one
// must be the value that does nothing.
type Action int

const (
	// ActionQuarantine freezes the order: no re-drive, no cancel, no further
	// automatic handling. It is the answer whenever venue truth and our own
	// record cannot be reconciled, and it is deliberately the zero value.
	ActionQuarantine Action = iota
	// ActionRedrive works the order at the venue now. Valid ONLY when the venue
	// has affirmatively stated it has no such order.
	ActionRedrive
	// ActionAdopt takes the venue's truth — its fills, or its rejection — as
	// ours. The venue is the authority on what it did.
	ActionAdopt
	// ActionLeave does nothing: the venue holds a live order and is working it.
	ActionLeave
)

func (a Action) String() string {
	switch a {
	case ActionQuarantine:
		return "QUARANTINE"
	case ActionRedrive:
		return "REDRIVE"
	case ActionAdopt:
		return "ADOPT"
	case ActionLeave:
		return "LEAVE"
	default:
		return fmt.Sprintf("Action(%d)", int(a))
	}
}

// Reconcile decides what to do with an interrupted order, given what the venue
// says about it. It is pure: same inputs, same decision, on the live path and in
// a test.
//
// The policy, in full:
//
//	venue answer      | venue_ack_at unset | venue_ack_at set
//	------------------|--------------------|------------------
//	UNKNOWN           | REDRIVE            | QUARANTINE
//	WORKING           | LEAVE              | LEAVE
//	PARTIALLY_FILLED  | ADOPT              | ADOPT
//	FILLED            | ADOPT              | ADOPT
//	REJECTED          | ADOPT              | ADOPT
//	INDETERMINATE     | QUARANTINE         | QUARANTINE
//
// Only ONE cell reads venue_ack_at, and it is the only cell where it could
// possibly matter. Everywhere else the venue has made a positive statement about
// an order it holds, and the venue is the authority on that — our record of
// whether we heard an acknowledgement adds nothing. Where the venue says it has
// NO such order, our record is the entire question: never acknowledged means it
// truly never arrived, so work it; acknowledged-then-denied means two
// authorities contradict each other, and the correct action is to stop.
//
// A returned reason is populated for ActionQuarantine and empty otherwise.
func Reconcile(st *orderpb.OrderState, view execution.OrderView) (Action, string) {
	// A terminal order has nothing left to work. Reaching here with one is a
	// caller bug, and any answer other than "stop" would re-trade it.
	if IsTerminal(st) {
		return ActionQuarantine, fmt.Sprintf(
			"reconciliation was asked about a %s order, which is terminal and has nothing to resume; "+
				"this is a caller defect, not a venue disagreement", st.GetStatus())
	}

	switch view.State {
	case execution.OrderViewWorking:
		return ActionLeave, ""

	case execution.OrderViewFilled, execution.OrderViewPartiallyFilled, execution.OrderViewRejected:
		return ActionAdopt, ""

	case execution.OrderViewUnknown:
		if st.GetVenueAckAt() == nil {
			// The venue never confirmed it, and the venue says it does not have
			// it. Both records agree: it never arrived. Working it now is the
			// whole point of resuming.
			return ActionRedrive, ""
		}
		return ActionQuarantine, fmt.Sprintf(
			"venue acknowledged this order at %s and now reports UNKNOWN. Two authorities "+
				"contradict each other: our record says the venue held it, the venue says it "+
				"never did. Re-driving would trade the fund twice if our record is right; "+
				"abandoning would strand a live exchange order if the venue is wrong. "+
				"Resolve against the venue's own order history before clearing this",
			st.GetVenueAckAt().AsTime().UTC().Format("2006-01-02T15:04:05Z"))

	default:
		// OrderViewIndeterminate and anything a future venue adapter returns that
		// this policy has not been taught. Both freeze: an answer we cannot map
		// is not an answer.
		reason := fmt.Sprintf("venue returned %s — nothing was established about this order", view.State)
		if view.Reason != "" {
			reason += ": " + view.Reason
		}
		return ActionQuarantine, reason
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
cd kanz && go test ./services/oms/internal/order/ -run 'TestReconcile|TestZeroAction' -v
```

Expected: every subtest PASS.

- [ ] **Step 5: Commit**

```bash
git add kanz/services/oms/internal/order/reconcile.go kanz/services/oms/internal/order/reconcile_test.go
git commit -m "feat(oms): the interrupted-order reconciliation policy, as a pure function

Six venue answers x two ack states, each individually tested. Only one cell
reads venue_ack_at, and it is the only one where it can matter: the venue saying
it has no such order means work it if we never had an acknowledgement, and means
freeze if we did.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 5: A per-order claim, so a resume never races the delivery that owns the order

**Files:**
- Modify: `kanz/services/oms/internal/order/service.go:33-49` (the `Service` struct)
- Test: `kanz/services/oms/internal/order/service_claim_test.go`

**Interfaces:**
- Produces: `func (s *Service) claim(orderID string) (release func(), ok bool)` — an unexported per-order mutual exclusion for this process.

**Why:** `store.Create` is the admission gate, and it protects the FIRST delivery. It says nothing about two goroutines both finding an EXISTING order and both deciding to resume it — which, after Task 6, both re-drive to the venue. The bus partition key makes that rare, not impossible, and "rare double trade" is not a posture. `sync.Map.LoadOrStore` is already the idiom in this file (`sharedOnce`, `service.go:47`).

- [ ] **Step 1: Write the failing test**

Create `kanz/services/oms/internal/order/service_claim_test.go`:

```go
package order

import (
	"sync"
	"testing"
)

// Two goroutines that both decide to resume one order must not both resume it.
// store.Create guards ADMISSION — the first delivery — and says nothing about
// two deliveries that both find an order already there. After the resume path
// exists, "both proceed" means both re-drive to the venue.
func TestClaimAdmitsExactlyOneHolderPerOrder(t *testing.T) {
	svc := &Service{}

	const goroutines = 64
	var (
		start sync.WaitGroup
		done  sync.WaitGroup
		mu    sync.Mutex
		held  int
	)
	start.Add(1)
	for i := 0; i < goroutines; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			release, ok := svc.claim("o-1")
			if !ok {
				return
			}
			defer release()
			mu.Lock()
			held++
			mu.Unlock()
		}()
	}
	start.Done()
	done.Wait()

	if held == 0 {
		t.Fatal("no goroutine obtained the claim — the order would never be worked at all")
	}
	if held > goroutines {
		t.Fatalf("impossible: %d holders from %d goroutines", held, goroutines)
	}
	// The claim must be re-obtainable after release, or one crashed resume
	// poisons the order for the process lifetime.
	release, ok := svc.claim("o-1")
	if !ok {
		t.Fatal("claim not available after every holder released — a released claim " +
			"that stays held freezes the order until the pod restarts")
	}
	release()
}

// Distinct orders must never block each other. A single global lock here would
// serialize the entire OMS behind the slowest venue call.
func TestClaimIsPerOrderNotGlobal(t *testing.T) {
	svc := &Service{}
	releaseA, ok := svc.claim("order-a")
	if !ok {
		t.Fatal("first claim on order-a failed")
	}
	defer releaseA()

	releaseB, ok := svc.claim("order-b")
	if !ok {
		t.Fatal("order-b was blocked by a claim on order-a — the claim is global, " +
			"which serializes every order in the OMS behind one venue call")
	}
	releaseB()
}

// Holding a claim must exclude a second claim on the SAME order.
func TestClaimExcludesTheSameOrder(t *testing.T) {
	svc := &Service{}
	release, ok := svc.claim("o-1")
	if !ok {
		t.Fatal("first claim failed")
	}
	if _, ok := svc.claim("o-1"); ok {
		release()
		t.Fatal("a second claim on a held order succeeded — two goroutines would " +
			"both resume it, and both would re-drive it to the venue")
	}
	release()
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd kanz && go test ./services/oms/internal/order/ -run TestClaim -v
```

Expected: FAIL to compile — `svc.claim undefined (type *Service has no field or method claim)`.

- [ ] **Step 3: Write the implementation**

In `kanz/services/oms/internal/order/service.go`, add one field to the `Service` struct, immediately after the `sharedOnce sync.Map` line (currently `service.go:47`):

```go
	// working claims an order_id for the goroutine currently driving it, so a
	// resume can never run alongside the delivery that already owns the order.
	// See claim().
	working sync.Map // order_id → struct{}
```

Then add the method at the end of the file:

```go
// claim takes exclusive, in-process ownership of one order_id and returns the
// function that releases it. ok is false when another goroutine in THIS process
// already holds it, and the caller must then do nothing at all.
//
// WHAT THIS IS FOR, AND WHAT IT IS NOT. store.Create is the ADMISSION gate: it
// decides which of two concurrent first-deliveries owns a new order. It has
// nothing to say about two deliveries that both find an order that ALREADY
// exists — which, once the handler resumes interrupted orders instead of acking
// them, is two goroutines both re-driving one order to a venue. The bus
// partition key makes that rare; rare is not a posture on the capital path.
//
// It is per-order, not global: a single lock would serialize every order in the
// OMS behind the slowest venue call.
//
// It is IN-PROCESS ONLY, and that is a real limit, not an oversight. Two OMS
// pods resuming the same order are not excluded by this and cannot be — that
// requires a lease in the store. It is the same single-replica assumption the
// order and position stores already carry (see the openStores comment in
// cmd/oms/main.go); this narrows the window that exists WITHIN a pod, which is
// the window a redelivery actually opens.
func (s *Service) claim(orderID string) (func(), bool) {
	if _, loaded := s.working.LoadOrStore(orderID, struct{}{}); loaded {
		return func() {}, false
	}
	return func() { s.working.Delete(orderID) }, true
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
cd kanz && go test ./services/oms/internal/order/ -run TestClaim -v -race
```

Expected: all three PASS under `-race`.

- [ ] **Step 5: Commit**

```bash
git add kanz/services/oms/internal/order/service.go kanz/services/oms/internal/order/service_claim_test.go
git commit -m "feat(oms): claim an order id in-process before working it

store.Create guards admission — which of two first-deliveries owns a NEW order.
It says nothing about two deliveries that both find an existing one, which is
what a resuming handler turns into two re-drives of the same order.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 6: Resume on redelivery instead of acking on sight

**Files:**
- Modify: `kanz/services/oms/internal/order/service.go` — `handleSubmit` (lines 130-134 and 188-193), `work` (lines 309-356), plus new `resume`/`adopt`/`quarantine` methods and a `WithQuarantineCounter` option
- Modify: `kanz/services/oms/internal/order/service_redelivery_test.go` (rewrite the pinned-gap test as the fix's signal test)
- Modify: `kanz/test/arch/bus_dlq_test.go:162-172` (correct the rationale text; the ban itself STAYS)

**Interfaces:**
- Consumes: `Reconcile`, `Action*` (Task 4); `s.claim` (Task 5); `execution.Querier`, `execution.OrderView` (Task 2); `st.GetVenueAckAt()`, `st.GetQuarantine()` (Task 1)
- Produces: `func WithQuarantineCounter(c prometheus.Counter) ServiceOption`; `func (s *Service) resume(ctx context.Context, st *orderpb.OrderState) error`

- [ ] **Step 1: Write the failing test — rewrite the pinned-gap test as the signal**

Replace the ENTIRE contents of `kanz/services/oms/internal/order/service_redelivery_test.go` with:

```go
package order

import (
	"context"
	"errors"
	"sync"
	"testing"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"

	"github.com/kanz-eng/kanz/internal/execution"
)

// unreachableVenue fails its FIRST execute and then recovers — a venue call
// that timed out, a 500, an adapter that had lost its connection and got it
// back. By the time the first failure fires the order has already been admitted,
// routed, and SAVED as ROUTED.
//
// The recovery is load-bearing in this test, not incidental. A venue that failed
// forever could never demonstrate a successful resume; it would only ever prove
// that the redelivery errored again. The behaviour under test is that delivery 2
// ASKS and then WORKS the order, so the venue has to be able to work it.
//
// QueryOrder is delegated to the embedded SimVenue, which is the honest model:
// the EXECUTE call failed and recorded nothing, so the venue truthfully answers
// that it has no such order.
type unreachableVenue struct {
	*execution.SimVenue
	mu sync.Mutex
	n  int
}

func (v *unreachableVenue) Execute(ctx context.Context, st *orderpb.OrderState) ([]*orderpb.Fill, error) {
	v.mu.Lock()
	v.n++
	first := v.n == 1
	v.mu.Unlock()
	if first {
		return nil, errors.New("venue unreachable")
	}
	return v.SimVenue.Execute(ctx, st)
}

func (v *unreachableVenue) count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.n
}

// THIS TEST REPLACES TestRedeliveryAfterVenueFailureIsAckedWithoutResuming.
//
// That test pinned the gap: delivery 1 created the order and failed at the
// venue; delivery 2 found the record, took it for a duplicate, returned nil, and
// left the order at ROUTED with nothing working it and nothing anywhere saying
// so. Its own doc said "WHEN THIS TEST FAILS, THAT IS THE SIGNAL, NOT A
// REGRESSION. It means handlers have learned to resume."
//
// This is that signal, inverted into an assertion. The redelivery must now ASK
// THE VENUE and act on the answer. The venue never received this order — its
// Execute failed before recording anything — so it answers UNKNOWN, our record
// carries no venue_ack_at, and the only safe action is to work it. It fills.
func TestRedeliveryAfterVenueFailureResumesAgainstVenueTruth(t *testing.T) {
	ctx := context.Background()
	fb := &fakeBus{}
	venue := &unreachableVenue{SimVenue: execution.NewSimVenue("XSIM")}
	store := NewMemoryStore()
	svc, err := NewService(store, NewEmitter(fb), nil, execution.NewRouter(venue), nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	cmd := limitOrder(d(100, 0), d(1025, -2))
	body := mustMarshal(t, cmd)

	// Delivery 1: admitted, routed, stored as ROUTED, then the venue call fails.
	if err := svc.Handle(ctx, submitEnv(), body); err == nil {
		t.Fatal("delivery 1 returned nil — expected the venue failure to surface")
	}
	if got := venue.count(); got != 1 {
		t.Fatalf("venue.Execute called %d times on delivery 1, want exactly 1", got)
	}
	st, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load after delivery 1: %v", err)
	}
	if st.GetVenueAckAt() != nil {
		t.Fatal("venue_ack_at is set after a FAILED Execute — the ack must be stamped " +
			"only when the venue actually confirmed it holds the order, or the " +
			"quarantine arm fires on healthy orders")
	}

	// Delivery 2: the redelivery that error asked for. The handler must now ask
	// the venue rather than ack on sight.
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 2 returned %v, want nil after a successful resume", err)
	}

	st, err = store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load after delivery 2: %v", err)
	}
	if got := st.GetStatus(); got != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("order status is %v, want FILLED — the venue had no record of this "+
			"order and no ack was ever recorded, so the redelivery had to work it", got)
	}
	if st.GetQuarantine() != nil {
		t.Fatalf("order was quarantined (%q) — the venue affirmatively said UNKNOWN "+
			"and we held no ack, which is the one combination that is safe to re-drive",
			st.GetQuarantine().GetReason())
	}
}

// THE DOUBLE-TRADE ARM. This is the assertion the whole design exists to make.
//
// The venue confirmed it held this order, and now denies it. Exactly one of the
// two records is wrong and nothing here can tell which. Re-driving trades the
// fund twice if ours is right; abandoning strands a live exchange order if the
// venue is right. The order freezes, and it must NOT reach the venue again.
func TestVenueDenyingAnAcknowledgedOrderQuarantinesAndDoesNotRedrive(t *testing.T) {
	ctx := context.Background()
	fb := &fakeBus{}
	// amnesiacVenue acknowledges an execute (no error) but then has no record of
	// the order — a venue that lost its order book, or answered from a replica
	// that never saw the write.
	venue := &amnesiacVenue{SimVenue: execution.NewSimVenue("XSIM")}
	store := NewMemoryStore()
	svc, err := NewService(store, NewEmitter(fb), nil, execution.NewRouter(venue), nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	cmd := limitOrder(d(100, 0), d(1025, -2))
	body := mustMarshal(t, cmd)

	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 1: %v", err)
	}
	st, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.GetVenueAckAt() == nil {
		t.Fatal("venue_ack_at not stamped after a SUCCESSFUL Execute — without it the " +
			"quarantine arm below can never fire, and this venue's contradiction " +
			"would be re-driven as if it were a fresh order")
	}
	before := venue.executes()

	// The redelivery. The venue now denies the order it acknowledged.
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 2 returned %v; a quarantine is terminal and must ack", err)
	}

	st, err = store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load after delivery 2: %v", err)
	}
	if st.GetQuarantine() == nil {
		t.Fatal("order was NOT quarantined. The venue acknowledged it and now reports " +
			"UNKNOWN — that is two authorities contradicting each other, and acting " +
			"on either one is a coin flip with the fund's money")
	}
	if st.GetQuarantine().GetReason() == "" {
		t.Error("quarantine carries no reason — an operator sees a frozen order and no account of why")
	}
	if got := venue.executes(); got != before {
		t.Fatalf("venue.Execute called %d more times — the quarantined order was "+
			"RE-DRIVEN, which is the double trade this whole design exists to prevent",
			got-before)
	}
}

// amnesiacVenue executes successfully but never remembers: every QueryOrder
// answers UNKNOWN. It models a venue whose order book was lost or whose read
// replica never saw the write.
type amnesiacVenue struct {
	*execution.SimVenue
	mu sync.Mutex
	n  int
}

func (v *amnesiacVenue) Execute(ctx context.Context, st *orderpb.OrderState) ([]*orderpb.Fill, error) {
	v.mu.Lock()
	v.n++
	v.mu.Unlock()
	// Deliberately does NOT delegate to SimVenue.Execute: this venue must
	// acknowledge without recording, so the query below can contradict it.
	return nil, nil
}

func (v *amnesiacVenue) QueryOrder(context.Context, *orderpb.OrderState) (execution.OrderView, error) {
	return execution.OrderView{State: execution.OrderViewUnknown}, nil
}

func (v *amnesiacVenue) executes() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.n
}

// A venue that cannot be asked is not a venue we may guess about. Every real
// out-of-process adapter is in this state today, because venue.v1's wire
// contract has no query RPC.
func TestVenueWithoutQuerierQuarantinesRatherThanGuessing(t *testing.T) {
	ctx := context.Background()
	fb := &fakeBus{}
	venue := &muteVenue{}
	store := NewMemoryStore()
	svc, err := NewService(store, NewEmitter(fb), nil, execution.NewRouter(venue), nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	cmd := limitOrder(d(100, 0), d(1025, -2))
	body := mustMarshal(t, cmd)

	if err := svc.Handle(ctx, submitEnv(), body); err == nil {
		t.Fatal("delivery 1 returned nil — expected the venue failure to surface")
	}
	before := venue.executes()

	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 2 returned %v; a quarantine is terminal and must ack", err)
	}
	st, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.GetQuarantine() == nil {
		t.Fatal("an order at a venue that implements no Querier was not quarantined — " +
			"nothing can establish what that venue did, and the alternative to " +
			"freezing is guessing")
	}
	if got := venue.executes(); got != before {
		t.Fatalf("venue.Execute called %d more times on a venue nobody can query", got-before)
	}
}

// muteVenue implements Venue and nothing else — no Querier. This is every
// out-of-process GRPCVenue today.
type muteVenue struct {
	mu sync.Mutex
	n  int
}

func (v *muteVenue) MIC() string     { return "XMUTE" }
func (v *muteVenue) Account() string { return "acct-mute" }
func (v *muteVenue) Execute(context.Context, *orderpb.OrderState) ([]*orderpb.Fill, error) {
	v.mu.Lock()
	v.n++
	v.mu.Unlock()
	return nil, errors.New("venue unreachable")
}
func (v *muteVenue) executes() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.n
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd kanz && go test ./services/oms/internal/order/ -run 'TestRedeliveryAfterVenueFailureResumes|TestVenueDenying|TestVenueWithoutQuerier' -v
```

Expected: all three FAIL. `TestRedeliveryAfterVenueFailureResumes...` fails asserting status `ROUTED != FILLED`; the other two fail asserting `quarantine == nil`.

- [ ] **Step 3: Stamp the venue acknowledgement in `work`**

In `kanz/services/oms/internal/order/service.go`, in `work`, replace the block from `fills, err := venue.Execute(ctx, st)` through the start of the fill loop (currently lines 332-336) with:

```go
	fills, err := venue.Execute(ctx, st)
	if err != nil {
		return st, err
	}

	// THE VENUE HAS IT. Record that BEFORE folding any fill.
	//
	// This single timestamp is what makes a later resume safe. Without it, an
	// order stored as ROUTED is indistinguishable from an order the venue never
	// received — and the recovery action for those two is opposite: work it, or
	// freeze it. A crash between here and the fold leaves the ack recorded and
	// the fills unfolded, which reconciliation adopts from venue truth; a crash
	// before here leaves no ack, which is exactly right, because the venue never
	// confirmed anything.
	acked := cloneState(st)
	acked.VenueAckAt = timestamppb.New(s.now().UTC())
	if err := s.store.Save(ctx, acked); err != nil {
		return st, err
	}
	st = acked

	for _, fill := range fills {
```

Add `"google.golang.org/protobuf/types/known/timestamppb"` to the file's import block.

- [ ] **Step 4: Add the resume path**

In `kanz/services/oms/internal/order/service.go`, add these methods at the end of the file:

```go
// resume decides what to do with an order that already exists when a SubmitOrder
// for it arrives again — a broker redelivery, or the startup sweep replaying an
// order the previous process was working when it died.
//
// THIS REPLACES ACKING ON SIGHT. The handler used to load the record, take it
// for a duplicate, and return nil. That is correct for a genuinely duplicated
// command and catastrophic for a redelivery of a command whose work was
// interrupted: the order sits at ROUTED, nothing works it, nothing mentions it
// again, and over the API it is indistinguishable from a limit order resting
// normally at the exchange.
//
// The one thing it must never do is guess. Every path below either establishes
// what the venue did, or freezes the order.
func (s *Service) resume(ctx context.Context, st *orderpb.OrderState) error {
	// A terminal order is genuinely finished — this really is a duplicate.
	if IsTerminal(st) {
		return nil
	}
	// An already-quarantined order is frozen and stays frozen. Re-running the
	// policy on every redelivery would just re-derive the same freeze, and the
	// venue answer that resolves it is a human's to obtain.
	if st.GetQuarantine() != nil {
		return nil
	}
	release, ok := s.claim(st.GetOrderId())
	if !ok {
		// Another goroutine in this process owns the order right now. It is being
		// worked; this delivery must not also work it.
		return nil
	}
	defer release()

	// Re-read under the claim: the holder we just raced may have finished and
	// changed the order between our Load and our claim.
	fresh, err := s.store.Load(ctx, st.GetOrderId())
	if err != nil {
		return err
	}
	if IsTerminal(fresh) || fresh.GetQuarantine() != nil {
		return nil
	}
	st = fresh

	// PENDING_NEW: admitted but never routed. Nothing reached a venue, so there
	// is nothing to reconcile and nothing to be careful about — work it. This sits
	// UNDER the claim, not before it, because working an order is exactly the
	// thing two goroutines must not do at once.
	if st.GetStatus() == orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW {
		_, err := s.work(ctx, st)
		return err
	}

	if s.router == nil {
		return nil // a paper deployment: the order rests, and nothing routed it
	}
	venue, err := s.router.Route(st)
	if err != nil {
		// Nothing here can execute this order. It is not resumable and it is not
		// safely abandonable either — freeze it and say so.
		return s.quarantine(ctx, st, fmt.Sprintf(
			"order cannot be routed to any configured venue (%v), so nothing can be asked "+
				"what happened to it", err))
	}

	q, ok := venue.(execution.Querier)
	if !ok {
		return s.quarantine(ctx, st, fmt.Sprintf(
			"venue %s implements no Querier, so nothing can establish whether it holds this "+
				"order. Re-driving might trade the fund twice and abandoning might strand a "+
				"live exchange order; neither is a guess this platform will make",
			venue.MIC()))
	}

	view, qerr := q.QueryOrder(ctx, st)
	if qerr != nil {
		// The QUESTION could not be asked — unreachable, rate-limited, timed out.
		// That is transient: nack and let the broker redeliver. It is emphatically
		// NOT an UNKNOWN answer, and collapsing the two would turn a network blip
		// into a re-driven order.
		return fmt.Errorf("oms: could not query venue %s about order %s: %w",
			venue.MIC(), st.GetOrderId(), qerr)
	}

	action, reason := Reconcile(st, view)
	s.logger.Info("oms reconciling an interrupted order",
		"order_id", st.GetOrderId(), "venue", venue.MIC(),
		"venue_view", view.State.String(), "action", action.String())

	switch action {
	case ActionLeave:
		// The venue holds a live order. Record the acknowledgement if we did not
		// already have it — the venue just proved it has the order.
		if st.GetVenueAckAt() == nil {
			acked := cloneState(st)
			acked.VenueAckAt = timestamppb.New(s.now().UTC())
			return s.store.Save(ctx, acked)
		}
		return nil
	case ActionRedrive:
		_, err := s.work(ctx, st)
		return err
	case ActionAdopt:
		return s.adopt(ctx, st, view)
	default:
		return s.quarantine(ctx, st, reason)
	}
}

// adopt takes the venue's truth as ours: its fills, or its rejection.
//
// Folding a fill twice is prevented downstream, not here: the position book
// claims each fill_id in position_fills before folding it, so a fill this
// adoption re-emits after a crash is counted exactly once. That is why the
// venue must report its ORIGINAL fill ids on a query — a renamed fill defeats
// the claim and double-counts the book.
func (s *Service) adopt(ctx context.Context, st *orderpb.OrderState, view execution.OrderView) error {
	if view.State == execution.OrderViewRejected {
		now := s.now().UTC()
		if err := s.store.Save(ctx, Reject(st, now)); err != nil {
			return err
		}
		reason := view.Reason
		if reason == "" {
			reason = "venue reports this order rejected"
		}
		return s.refuse(ctx, st.GetOrderId(), "VENUE_REJECTED", reason, now)
	}

	// The venue confirmed it holds the order, so the ack is established even if
	// we never recorded one.
	if st.GetVenueAckAt() == nil {
		acked := cloneState(st)
		acked.VenueAckAt = timestamppb.New(s.now().UTC())
		if err := s.store.Save(ctx, acked); err != nil {
			return err
		}
		st = acked
	}

	for _, fill := range view.Fills {
		next, aerr := ApplyFill(st, fill, fill.GetExecutedAt().AsTime())
		if aerr != nil {
			// The venue's own fills do not fit the order we hold. That is not a
			// transient fault and re-driving cannot help; it is a disagreement
			// about what this order IS, and it freezes.
			return s.quarantine(ctx, st, fmt.Sprintf(
				"venue reported a fill this order cannot accept (%v). The venue's record and "+
					"ours describe different orders under one id", aerr))
		}
		if err := s.store.Save(ctx, next); err != nil {
			return err
		}
		if err := s.emitter.EmitFill(ctx, fill, next); err != nil {
			return err
		}
		st = next
		if IsTerminal(st) {
			break
		}
	}
	return nil
}

// quarantine freezes an order whose truth could not be established, and says so
// where somebody will see it: on the order, in the log at ERROR, and on a
// counter that can be alerted.
//
// It returns nil — the command is ACKED. A quarantine is terminal for this
// delivery: redelivering it would re-derive the same freeze forever, and the
// thing that resolves it is a human with venue access, not another attempt.
func (s *Service) quarantine(ctx context.Context, st *orderpb.OrderState, reason string) error {
	now := s.now().UTC()
	next := cloneState(st)
	next.Quarantine = &orderpb.OrderQuarantine{
		At:          timestamppb.New(now),
		Reason:      reason,
		LastQueryAt: timestamppb.New(now),
	}
	if err := s.store.Save(ctx, next); err != nil {
		return err
	}
	if s.quarantined != nil {
		s.quarantined.Inc()
	}
	s.logger.Error("ORDER QUARANTINED — the platform cannot establish what the venue did with this order, so it has stopped rather than guess. It will not be re-driven, cancelled, or mentioned again until a human resolves it against the venue's own order history",
		"order_id", st.GetOrderId(),
		"portfolio_id", st.GetPortfolioId(),
		"instrument_id", st.GetInstrumentId(),
		"venue", st.GetVenue(),
		"status", st.GetStatus().String(),
		"reason", reason,
	)
	return nil
}
```

Add `"github.com/kanz-eng/kanz/internal/execution"` — already imported — and confirm `fmt` and `timestamppb` are in the import block.

- [ ] **Step 5: Add the quarantine counter and wire the resume calls**

In `kanz/services/oms/internal/order/service.go`, add one field to the `Service` struct, after the `sharedCount prometheus.Counter` line:

```go
	// quarantined counts orders frozen because venue truth could not be
	// established. It is the alertable signal: a quarantine is a position whose
	// true size nobody knows, and it must not be discoverable only by reading logs.
	quarantined prometheus.Counter
```

Add the option, next to `WithAccountBindings`:

```go
// WithQuarantineCounter gives the OMS the counter it increments when an order is
// frozen because venue truth could not be established. Without it a quarantine
// is still persisted and logged at ERROR, but nothing is alertable — and an
// order nobody is watching is exactly what this whole path exists to prevent.
func WithQuarantineCounter(c prometheus.Counter) ServiceOption {
	return func(s *Service) { s.quarantined = c }
}
```

Now replace the fast-path duplicate check in `handleSubmit` (currently lines 125-134):

```go
	// AN ORDER WE ALREADY KNOW IS NOT AUTOMATICALLY A DUPLICATE.
	//
	// This used to return nil on sight, which is right for a genuinely duplicated
	// command and catastrophic for a redelivery of one whose work was interrupted:
	// delivery 1 created the order and died at the venue, delivery 2 acked it, and
	// the order sat at ROUTED forever with nothing working it. resume() establishes
	// what the venue actually did and acts on that — or freezes the order when it
	// cannot. Admission itself is still enforced atomically by store.Create below.
	if existing, err := s.store.Load(ctx, cmd.GetOrderId()); err == nil {
		return s.resume(ctx, existing)
	} else if !errors.Is(err, ErrNotFound) {
		return err // transient store failure
	}
```

Leave the `store.Create` / `ErrExists` branch (lines 188-193) EXACTLY as it is, returning nil. That branch means another delivery is admitting this order *right now* and is about to work it; resuming there would race the admission it just lost. Add this to its comment:

```go
		if errors.Is(err, ErrExists) {
			// Lost the admission race — the winner works the order. Deliberately NOT
			// a resume: the winner is mid-flight by construction, and the interrupted
			// case is reached through the Load fast path above (on redelivery) or the
			// startup sweep (after a crash), both of which take the per-order claim.
			return nil
		}
```

Finally, wire the counter in `kanz/services/oms/cmd/oms/main.go`. Immediately after the `obs.Registry.MustRegister(sharedCollateral)` line (currently line 249), add:

```go
	quarantined := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_oms_orders_quarantined_total",
		Help: "Orders frozen because the platform could not establish what the venue did with them — " +
			"the venue denied an order it had acknowledged, could not be queried at all, or reported " +
			"a fill the order cannot accept. Each one is a position whose true size nobody knows, and " +
			"it will not be re-driven, cancelled or mentioned again until a human resolves it. " +
			"This should be zero; any non-zero value is an incident, not a metric to trend.",
	})
	obs.Registry.MustRegister(quarantined)
```

Then extend the `order.NewService` call (currently lines 282-283) to pass it:

```go
	svc, err := order.NewService(store, emitter, gate, router, closeRegistry, logger,
		order.WithAccountBindings(bindings, cfg.RequireVenueAccount, sharedCollateral),
		order.WithQuarantineCounter(quarantined))
```

- [ ] **Step 6: Run the tests to verify they pass**

```bash
cd kanz && go test ./services/oms/internal/order/ -run 'TestRedeliveryAfterVenueFailureResumes|TestVenueDenying|TestVenueWithoutQuerier' -v -race
```

Expected: all three PASS.

Then the whole OMS package:

```bash
cd kanz && go test ./services/oms/... -race 2>&1 | tail -30
```

Expected: PASS. If `service_test.go`'s existing idempotency test now fails, read it before changing it — a resubmit of a FILLED order still returns nil via the `IsTerminal` branch at the top of `resume`, so a failure there means something else.

- [ ] **Step 7: Correct the arch guard's rationale — WITHOUT lifting the ban**

In `kanz/test/arch/bus_dlq_test.go`, replace the failure message at lines 162-172 with:

```go
		t.Fatalf("%s wire bus.WithRetry, and the handlers in this estate are not all "+
			"safely re-enterable yet.\n\n"+
			"WithRetry re-runs the SAME handler in-process. A handler that begins by "+
			"treating an event it can already load as a duplicate and returning nil turns "+
			"attempt 2 into an instant silent ack: the consumer marks the command HANDLED "+
			"and the failure never reaches the DLQ at all. Retry there is strictly worse "+
			"than no retry.\n\n"+
			"THE OMS's handleSubmit IS NOW AN EXCEPTION — it resumes against venue truth "+
			"rather than acking on sight, using OrderState.venue_ack_at to tell an order "+
			"the venue never received from one it acknowledged (see "+
			"services/oms/internal/order/reconcile.go). The other twelve consumers have had "+
			"no such change, so this ban stays estate-wide: it is now over-broad rather than "+
			"load-bearing for the OMS specifically. Narrowing it to the handlers that still "+
			"ack-on-sight is a real task; deleting it because one handler was fixed is not.",
			strings.Join(retrying, ", "))
```

- [ ] **Step 8: Run the arch tests**

```bash
cd kanz && go test ./test/arch/ 2>&1 | tail -20
```

Expected: PASS (the guard still bans `WithRetry`, and nothing wires it).

- [ ] **Step 9: Commit**

```bash
git add kanz/services/oms/internal/order/service.go \
        kanz/services/oms/internal/order/service_redelivery_test.go \
        kanz/services/oms/cmd/oms/main.go \
        kanz/test/arch/bus_dlq_test.go
git commit -m "feat(oms): resume interrupted orders against venue truth instead of acking on sight

handleSubmit's fast path returned nil for any order it could already load. That
is right for a duplicated command and catastrophic for a redelivery of one whose
work was interrupted: the order sat at ROUTED with nothing working it, looking
exactly like a limit order resting normally. It now asks the venue what happened
and acts on the answer — working the order only when the venue affirmatively has
no record of it and we never recorded an acknowledgement, and freezing it
whenever the two disagree.

The bus.WithRetry ban STAYS. One handler learned to resume; twelve did not.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 7: Sweep the interrupted orders at startup, before anything can add more

**Files:**
- Modify: `kanz/services/oms/internal/order/store.go` (add `ListByStatus` to the `Store` interface + `MemoryStore`)
- Modify: `kanz/services/oms/internal/order/postgres.go` (implement `ListByStatus`)
- Create: `kanz/services/oms/internal/order/sweep.go`
- Modify: `kanz/services/oms/cmd/oms/main.go` (call the sweep before the subscription goroutines start, around line 348)
- Test: `kanz/services/oms/internal/order/sweep_test.go`

**Interfaces:**
- Consumes: `s.resume` (Task 6)
- Produces:
  - `ListByStatus(ctx context.Context, statuses ...orderpb.OrderStatus) ([]*orderpb.OrderState, error)` on `Store`, `MemoryStore` and `Postgres`
  - `func (s *Service) SweepInterrupted(ctx context.Context) (swept int, err error)`

**Why a sweep as well as the redelivery path:** a redelivery only rescues an order whose command is still in the stream. An order whose command was ACKED and whose process then died between `Save(ROUTED)` and the fold has no redelivery coming — nothing will ever mention it again. `tv-sync` already establishes the shape (`cmd/tv-sync/main.go:114-129`): rebuild before serving, and `os.Exit(2)` rather than start degraded.

**Why `ListByStatus` and not the existing `List`:** `List` loads every order the store has ever held. The sweep wants the open ones, and `orders_status_idx` on `(tenant_id, status)` already exists in `migrations/0001_orders.sql` for exactly this shape of query. `List` stays — it is the bootstrap/inspection surface — and gains a caller nowhere.

- [ ] **Step 1: Write the failing test**

Create `kanz/services/oms/internal/order/sweep_test.go`:

```go
package order

import (
	"context"
	"testing"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"

	"github.com/kanz-eng/kanz/internal/execution"
)

// AN ORDER STRANDED BY A CRASH HAS NO REDELIVERY COMING.
//
// The redelivery path rescues an order whose command is still in the stream. It
// does nothing for one whose command was ACKED and whose process then died
// between Save(ROUTED) and the fold — nothing will ever mention that order
// again. The sweep is the only thing that finds it, and it must run before the
// consumers do, the way tv-sync rebuilds its book before it serves one.
func TestSweepResumesAnOrderStrandedAtRouted(t *testing.T) {
	ctx := context.Background()
	fb := &fakeBus{}
	venue := execution.NewSimVenue("XSIM")
	store := NewMemoryStore()
	svc, err := NewService(store, NewEmitter(fb), nil, execution.NewRouter(venue), nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// Exactly what a crashed predecessor leaves behind: admitted, routed, saved,
	// no venue acknowledgement, no fill.
	admitted, err := Accept(limitOrder(d(100, 0), d(1025, -2)), t0)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	stranded := Route(admitted, t0)
	if err := store.Create(ctx, stranded); err != nil {
		t.Fatalf("Create: %v", err)
	}

	swept, err := svc.SweepInterrupted(ctx)
	if err != nil {
		t.Fatalf("SweepInterrupted: %v", err)
	}
	if swept != 1 {
		t.Fatalf("swept %d orders, want 1", swept)
	}

	st, err := store.Load(ctx, stranded.GetOrderId())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := st.GetStatus(); got != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("stranded order status is %v, want FILLED — the venue had no record "+
			"of it and no ack was recorded, so the sweep had to work it", got)
	}
}

// The sweep must not touch finished orders. Loading the whole book and
// "resuming" a filled order would re-trade it.
func TestSweepIgnoresTerminalOrders(t *testing.T) {
	ctx := context.Background()
	fb := &fakeBus{}
	store := NewMemoryStore()
	svc, err := NewService(store, NewEmitter(fb), nil, execution.NewRouter(execution.NewSimVenue("XSIM")), nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	done, err := Accept(limitOrder(d(100, 0), d(1025, -2)), t0)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	done.Status = orderpb.OrderStatus_ORDER_STATUS_FILLED
	if err := store.Create(ctx, done); err != nil {
		t.Fatalf("Create: %v", err)
	}

	swept, err := svc.SweepInterrupted(ctx)
	if err != nil {
		t.Fatalf("SweepInterrupted: %v", err)
	}
	if swept != 0 {
		t.Fatalf("swept %d orders, want 0 — a FILLED order has nothing to resume", swept)
	}
}

// ListByStatus must select, not scan-and-filter-in-the-caller: the sweep is on
// the startup path and the store may hold every order the fund has ever placed.
func TestListByStatusReturnsOnlyTheRequestedStatuses(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	routed := &orderpb.OrderState{OrderId: "routed", Status: orderpb.OrderStatus_ORDER_STATUS_ROUTED}
	filled := &orderpb.OrderState{OrderId: "filled", Status: orderpb.OrderStatus_ORDER_STATUS_FILLED}
	pending := &orderpb.OrderState{OrderId: "pending", Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW}
	for _, st := range []*orderpb.OrderState{routed, filled, pending} {
		if err := store.Create(ctx, st); err != nil {
			t.Fatalf("Create %s: %v", st.GetOrderId(), err)
		}
	}

	got, err := store.ListByStatus(ctx,
		orderpb.OrderStatus_ORDER_STATUS_ROUTED,
		orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW)
	if err != nil {
		t.Fatalf("ListByStatus: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListByStatus returned %d orders, want 2", len(got))
	}
	for _, st := range got {
		if st.GetOrderId() == "filled" {
			t.Fatal("ListByStatus returned a FILLED order for a ROUTED/PENDING_NEW query")
		}
	}
}
```

This test defines no new helpers. It reuses the package's existing ones: `limitOrder` and `d` from `aggregate_test.go:14-29`, the fixed test clock `t0` from `aggregate_test.go:31`, and `fakeBus` from `service_test.go`. Do not add a second spelling of any of them.

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd kanz && go test ./services/oms/internal/order/ -run 'TestSweep|TestListByStatus' -v
```

Expected: FAIL to compile — `svc.SweepInterrupted undefined`, `store.ListByStatus undefined`.

- [ ] **Step 3: Add `ListByStatus` to the interface and `MemoryStore`**

In `kanz/services/oms/internal/order/store.go`, add to the `Store` interface, after `List`:

```go
	// ListByStatus returns every order currently in one of the given statuses.
	// It exists for the startup sweep, which wants the OPEN orders and must not
	// load every order the fund has ever placed to find them. A durable backend
	// must answer it with a selection, not a scan — migrations/0001_orders.sql
	// carries orders_status_idx on (tenant_id, status) for exactly this.
	// Passing no statuses returns nothing.
	ListByStatus(ctx context.Context, statuses ...orderpb.OrderStatus) ([]*orderpb.OrderState, error)
```

And the `MemoryStore` implementation, after `List`:

```go
func (m *MemoryStore) ListByStatus(_ context.Context, statuses ...orderpb.OrderStatus) ([]*orderpb.OrderState, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	want := make(map[orderpb.OrderStatus]bool, len(statuses))
	for _, s := range statuses {
		want[s] = true
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*orderpb.OrderState
	for _, st := range m.orders {
		if want[st.GetStatus()] {
			out = append(out, proto.Clone(st).(*orderpb.OrderState))
		}
	}
	return out, nil
}
```

- [ ] **Step 4: Implement `ListByStatus` on the Postgres store**

In `kanz/services/oms/internal/order/postgres.go`, add immediately after `List` (which ends at line 130, just before `func unmarshalState`). This mirrors `List` exactly — same `p.pool.Query`, same `(order_id, state)` scan, same `unmarshalState` helper — differing only in the WHERE clause:

```go
// ListByStatus selects the orders currently in one of the given statuses.
//
// It reads the DENORMALIZED status column, which is what orders_status_idx
// covers, so the startup sweep costs one index scan over the open orders rather
// than a full scan over every order the fund has ever placed. The authoritative
// state is still the marshaled proto in `state` — status is the index key, not
// the truth, and the two are written in the same statement so they cannot drift.
//
// Tenant scoping is NOT applied here and must not be: RLS is FORCEd on this
// table (migrations/0001_orders.sql) and the policy filters on
// current_setting('app.tenant_id'), exactly as it does for List and Load.
func (p *Postgres) ListByStatus(ctx context.Context, statuses ...orderpb.OrderStatus) ([]*orderpb.OrderState, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	codes := make([]int32, 0, len(statuses))
	for _, s := range statuses {
		codes = append(codes, int32(s))
	}
	rows, err := p.pool.Query(ctx,
		`SELECT order_id, state FROM orders WHERE status = ANY($1) ORDER BY order_id`, codes)
	if err != nil {
		return nil, fmt.Errorf("list orders by status: %w", err)
	}
	defer rows.Close()

	var out []*orderpb.OrderState
	for rows.Next() {
		var (
			id   string
			blob []byte
		)
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, fmt.Errorf("scan order: %w", err)
		}
		st, err := unmarshalState(blob, id)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}
```

**Verify the status column is actually maintained before trusting this.** `Save` and `Create` must both write `status` alongside `state`, or the index is querying a stale column and the sweep will miss orders:

```bash
grep -n "status" kanz/services/oms/internal/order/postgres.go
```

Expected: both the `Create` and `Save` INSERT statements set `status`. If either does not, fix that FIRST — a sweep that silently returns an incomplete set is worse than no sweep, because it reports success.

- [ ] **Step 5: Write the sweep**

Create `kanz/services/oms/internal/order/sweep.go`:

```go
package order

import (
	"context"
	"fmt"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
)

// SweepInterrupted reconciles every order this OMS left mid-flight, and it must
// run BEFORE the consumers subscribe.
//
// WHY A SWEEP AS WELL AS THE REDELIVERY PATH. A redelivery rescues an order
// whose command is still in the stream. It does nothing for an order whose
// command was ACKED and whose process then died between Save(ROUTED) and the
// fold: no redelivery is coming, and nothing will ever mention that order again.
// That order is a live position, or an order resting at an exchange, that this
// platform has forgotten. The sweep is the only thing that finds it.
//
// The shape is tv-sync's (cmd/tv-sync/main.go:114): rebuild what you know before
// anything can read it or add to it. The caller treats a failure the same way —
// a pod that could not reconcile its in-flight orders does not know what the
// fund holds, and starting anyway is not degraded service, it is wrong service.
//
// It returns the number of orders it acted on. Orders it leaves alone (the venue
// is working them) and orders it freezes both count: they were interrupted, and
// the number is the operator's first signal about how bad the interruption was.
func (s *Service) SweepInterrupted(ctx context.Context) (int, error) {
	open, err := s.store.ListByStatus(ctx,
		orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW,
		orderpb.OrderStatus_ORDER_STATUS_ROUTED,
		orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED,
	)
	if err != nil {
		return 0, fmt.Errorf("oms: could not list in-flight orders to reconcile: %w", err)
	}

	swept := 0
	for _, st := range open {
		if st.GetQuarantine() != nil {
			continue // already frozen; a human owns it
		}
		if err := s.resume(ctx, st); err != nil {
			// One order that cannot be reconciled stops the sweep. The alternative
			// is a pod that starts having silently skipped an order it could not
			// account for, which is the failure this whole path exists to end.
			return swept, fmt.Errorf("oms: could not reconcile in-flight order %s: %w",
				st.GetOrderId(), err)
		}
		swept++
	}
	return swept, nil
}
```

- [ ] **Step 6: Run the tests to verify they pass**

```bash
cd kanz && go test ./services/oms/internal/order/ -run 'TestSweep|TestListByStatus' -v -race
```

Expected: all three PASS.

- [ ] **Step 7: Call the sweep at startup, before the consumers**

In `kanz/services/oms/cmd/oms/main.go`, in `runConsumers`, insert immediately BEFORE the line `ctx, cancel := context.WithCancel(ctx)` (currently line 348) and after all wiring is complete:

```go
	// RECONCILE WHAT THE LAST PROCESS LEFT MID-FLIGHT, BEFORE ANYTHING CAN ADD MORE.
	//
	// An order saved as ROUTED whose process died before the fill was folded has
	// NO redelivery coming — its command was acked. Nothing else in this system
	// will ever mention it again, and it is indistinguishable over the API from a
	// limit order resting normally at the exchange. This is the only thing that
	// finds it. It runs before the subscriptions and before readiness, the way
	// tv-sync rebuilds its book before it serves one.
	//
	// A failure here is fatal on purpose. A pod that cannot account for the orders
	// its predecessor was working does not know what the fund holds, and admitting
	// new orders on top of that is not degraded operation — it is trading blind.
	sweepStart := time.Now()
	swept, err := svc.SweepInterrupted(ctx)
	if err != nil {
		logger.Error("could not reconcile the orders left in flight by the previous process — refusing to admit new orders on a book we cannot account for", "err", err, "reconciled_before_failure", swept)
		return err
	}
	logger.Info("in-flight orders reconciled", "count", swept, "took", time.Since(sweepStart).String())
```

Confirm `"time"` is in `main.go`'s import block (it is — `ReconcileInterval` and friends use it).

- [ ] **Step 8: Verify the whole service builds and passes**

```bash
cd kanz && go build ./... && go test ./services/oms/... -race 2>&1 | tail -30
```

Expected: build succeeds, all OMS packages PASS.

- [ ] **Step 9: Commit**

```bash
git add kanz/services/oms/internal/order/store.go \
        kanz/services/oms/internal/order/postgres.go \
        kanz/services/oms/internal/order/sweep.go \
        kanz/services/oms/internal/order/sweep_test.go \
        kanz/services/oms/cmd/oms/main.go
git commit -m "feat(oms): reconcile in-flight orders at startup, before consumers subscribe

An order whose command was acked and whose process then died between Save(ROUTED)
and the fold has no redelivery coming — nothing would ever mention it again. The
sweep is the only thing that finds it, and it runs before the subscriptions and
before readiness, the way tv-sync rebuilds its book before it serves one. A
failure is fatal: a pod that cannot account for its predecessor's in-flight
orders is trading blind.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 8: Prove it on the rig, then correct the board

**Files:** none (verification + board)

This task produces no code. Its deliverable is evidence.

- [ ] **Step 1: Full build, lint and test**

```bash
cd kanz && go build ./... && go vet ./... && go test ./... 2>&1 | tail -40
```

Expected: no failures. If `gofmt` reports differences, check whether they are CRLF line-ending artifacts on this Windows host before changing any file — that has cost a session before.

- [ ] **Step 2: Rebuild the rig and run the trading loop end to end**

```bash
bash tools/rig-images.sh --build --load --cluster "$(kind get clusters | head -1)"
bash tools/rig-apply.sh --deploy
kubectl -n kanz-services rollout restart deploy/oms
kubectl -n kanz-services rollout status deploy/oms --timeout=180s
```

Then re-run the end-to-end proof recorded in the `kanz-e2e-cluster-proof` memory (local webhook-ingest on the cluster spine, EXECUTION stream + EventFrame, limit-orders-only sim venue, HS256 gateway token).

Expected: alert → order → fill, as before. This change must not alter the healthy path at all.

- [ ] **Step 3: Prove the sweep ran and said so**

```bash
kubectl -n kanz-services logs deploy/oms | grep "in-flight orders reconciled"
```

Expected: one line, with a count. On a clean rig the count is 0 — and that is the point: the line proves the sweep RAN, which a silent no-op would not.

- [ ] **Step 4: Prove the recovery path on the rig, not just in tests**

Place a limit order, kill the OMS pod mid-flight, and confirm the replacement reconciles rather than stranding it:

```bash
# Submit an order, then immediately delete the pod.
kubectl -n kanz-services delete pod -l app=oms --grace-period=0 --force
kubectl -n kanz-services rollout status deploy/oms --timeout=180s
kubectl -n kanz-services logs deploy/oms | grep -E "in-flight orders reconciled|oms reconciling an interrupted order|ORDER QUARANTINED"
```

Expected: either the reconcile line naming the order and its action, or a count > 0 on the sweep line. An order left at ROUTED with no log line at all means the sweep did not see it — check that `ListByStatus` is setting the tenant, because RLS returns an empty set silently when it is not.

- [ ] **Step 5: Update the board honestly**

Rewrite the DLQ/crash-mid-work row in `KANZ_TASKS.md` to record precisely what is now true and what is not:

- `handleSubmit` resumes against venue truth; the OMS no longer acks an interrupted order on sight.
- The startup sweep finds orders that have no redelivery coming.
- **Still true and unchanged:** the `bus.WithRetry` ban stands. Twelve other consumers still ack-on-sight; the guard is now over-broad rather than load-bearing for the OMS, and narrowing it is a separate task.
- **Still true and unchanged:** only `SimVenue` implements `Querier`. Every out-of-process adapter QUARANTINES on an interrupted order, because `venue.v1.VenueAdapterService` has no query RPC. The recovery path is proven on the sim path ONLY.
- **Still true and unchanged:** the per-order claim is in-process. Two OMS pods resuming one order are not excluded; that needs a lease in the store.

```bash
bash tools/validate-board.sh KANZ_TASKS.md
git add KANZ_TASKS.md && git commit -m "docs(board): the OMS resumes interrupted orders; the sim path is proven, the venue path is not

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Verification

The plan is complete when all of the following hold:

1. `cd kanz && go build ./... && go test ./... ` is green.
2. `TestRedeliveryAfterVenueFailureResumesAgainstVenueTruth` passes — the redelivery works the order instead of acking it.
3. `TestVenueDenyingAnAcknowledgedOrderQuarantinesAndDoesNotRedrive` passes — and its `venue.executes()` assertion is what proves no double trade.
4. `TestVenueWithoutQuerierQuarantinesRatherThanGuessing` passes — an unqueryable venue freezes rather than guesses.
5. Every row of `TestReconcilePolicy` passes, including both `UNKNOWN` cells.
6. `TestSweepResumesAnOrderStrandedAtRouted` passes, and `SweepInterrupted` is called before the subscription goroutines in `cmd/oms/main.go`.
7. `cd kanz && go test ./test/arch/` is green, with the `WithRetry` ban still in force.
8. The rig's trading loop still completes end to end, and the OMS logs `in-flight orders reconciled` at startup.
9. `bash tools/validate-board.sh KANZ_TASKS.md` exits 0.

**Known limits, to be stated on the board rather than glossed:**

- **Only `SimVenue` can be queried.** Binance and OKX each have a private `queryOrder` their own reconcilers call, but `venue.v1.VenueAdapterService` exposes no query RPC, so an out-of-process adapter cannot be asked. Every interrupted order at a real venue QUARANTINES. That is the correct behaviour and it is not the finished one — surfacing the RPC is the follow-on task, and until it lands this work is proven on the simulator only.
- **The per-order claim is in-process.** It closes the window a redelivery opens within one pod. Two pods resuming the same order need a lease in the store, which is the same single-replica assumption the order and position stores already carry.
- **Quarantine has no clearing path.** A frozen order stays frozen until a human edits it. That is deliberate for a first cut — an automatic un-quarantine is an automatic guess — but it means an operator runbook is owed, and `autopilot`'s stance that "re-admitting bad data is a human decision" is the precedent to follow.
