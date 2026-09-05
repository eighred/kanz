package orderview

// THE ORDERING DISCIPLINE ON VENUE EXECUTION REPORTS (#1046).
//
// Before refuseStale, Progress refused exactly one backwards move — terminal to
// non-terminal — and carryVenueObserved then wrote filled_quantity and
// leaves_quantity unconditionally whenever the report set them. So a cumulative
// of 8 followed by a late cumulative of 3 left the view holding 3, with no
// error, no refusal and no counter. Measured, not inferred:
//
//	late report err = <nil>; view filled_quantity now = 3 leaves = 7
//
// The cost is not bookkeeping. This view is the EXPECTED side of the healing
// watchdog's comparison against exchange truth, so a regressed view disagrees
// with the venue about an order that is fine, healedState finds permanent drift,
// and the adapter emits a StateHealed FACT for a divergence that does not exist
// — the re-emit loop #904 and #891 were filed to stop, reached through the value
// this time rather than through the retention.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// venueAt is a venue timestamp, in the milliseconds both connectors read them in.
func venueAt(ms int64) *timestamppb.Timestamp { return timestamppb.New(time.UnixMilli(ms).UTC()) }

// stamped is venueReport with the venue's own ordering token on it.
func stamped(id string, status orderpb.OrderStatus, filled, leaves, ms int64) *orderpb.OrderState {
	st := venueReport(id, status, filled, leaves)
	st.AsOf = venueAt(ms)
	return st
}

// TestProgressRefusesANonMonotonicCumulativeFilledQuantity is the issue's own
// reproduction, inverted into an assertion.
//
// It asserts BOTH halves, because a refusal that does not preserve the value is
// half a fix: the error is what the counter and the operator see, and the
// retained 8 is what keeps the healing watchdog agreeing with the exchange.
func TestProgressRefusesANonMonotonicCumulativeFilledQuantity(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	if err := m.Record(ctx, working("o1")); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := Progress(ctx, m, venueReport("o1", orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED, 8, 2)); err != nil {
		t.Fatalf("the first venue report must be recorded: %v", err)
	}

	err := Progress(ctx, m, venueReport("o1", orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED, 3, 7))
	if !errors.Is(err, ErrFilledQuantityRegressed) {
		t.Fatalf("a late report carrying cumulative 3 over a recorded 8 returned %v, want "+
			"ErrFilledQuantityRegressed. An exchange does not un-execute, so this report is stale "+
			"or replayed and applying it runs the adapter's own book backwards", err)
	}

	got, _, ok, gerr := m.Get(ctx, "o1")
	if gerr != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, gerr)
	}
	if c := got.GetFilledQuantity().GetCoefficient(); c != 8 {
		t.Errorf("after the refused late report the view holds filled_quantity = %d, want 8. The "+
			"view is the EXPECTED side of reconcileOrders: a regressed value manufactures phantom "+
			"drift and a StateHealed FACT for a divergence that does not exist", c)
	}
	if c := got.GetLeavesQuantity().GetCoefficient(); c != 2 {
		t.Errorf("after the refused late report the view holds leaves_quantity = %d, want 2", c)
	}
}

// TestProgressRecordsAMonotonicallyAdvancingCumulative is the other side of the
// same gate, and it is not filler: a refusal written one comparison out — >= for
// > — would silently stop the view advancing at all, which is #904's leak back
// with a counter attached and a green test above it.
func TestProgressRecordsAMonotonicallyAdvancingCumulative(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	if err := m.Record(ctx, working("o1")); err != nil {
		t.Fatalf("record: %v", err)
	}
	for _, step := range []struct{ filled, leaves int64 }{{3, 7}, {8, 2}, {10, 0}} {
		report := venueReport("o1", orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED, step.filled, step.leaves)
		if err := Progress(ctx, m, report); err != nil {
			t.Fatalf("cumulative %d over the previous one was refused: %v", step.filled, err)
		}
	}
	got, _, _, _ := m.Get(ctx, "o1")
	if c := got.GetFilledQuantity().GetCoefficient(); c != 10 {
		t.Errorf("after three advancing reports the view holds %d, want 10", c)
	}
}

// TestProgressComparesCumulativesAcrossExponents pins the comparison to
// internal/dec rather than to a raw coefficient.
//
// 0.30 and 0.4 are {30,-2} and {4,-1}: the STALE one has the LARGER coefficient.
// A hand-rolled `a.Coefficient < b.Coefficient` passes every other test in this
// file and reverses this one, and exchanges send exactly these shapes — a
// quantity comes back at the symbol's step size, so the exponent moves between
// two reports on the same order.
func TestProgressComparesCumulativesAcrossExponents(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	if err := m.Record(ctx, working("o1")); err != nil {
		t.Fatalf("record: %v", err)
	}
	held := venueReport("o1", orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED, 0, 0)
	held.FilledQuantity = &commonpb.Decimal{Coefficient: 4, Exponent: -1} // 0.4
	if err := Progress(ctx, m, held); err != nil {
		t.Fatalf("first report: %v", err)
	}

	late := venueReport("o1", orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED, 0, 0)
	late.FilledQuantity = &commonpb.Decimal{Coefficient: 30, Exponent: -2} // 0.30 — smaller value, larger coefficient
	if err := Progress(ctx, m, late); !errors.Is(err, ErrFilledQuantityRegressed) {
		t.Fatalf("0.30 following 0.4 returned %v, want ErrFilledQuantityRegressed. 0.30 has the "+
			"LARGER coefficient, so a comparison on coefficients alone reads this regression as an "+
			"advance", err)
	}
}

// TestProgressRefusesAReportOlderThanTheLastVenueObservation is the ordering
// gate — the general form the quantity gate cannot reach.
//
// Both reports carry the SAME cumulative, so nothing about the quantity says
// which came first. Only the venue's own token does, and without it a replayed
// push writes an older STATUS and average fill price over a newer one.
func TestProgressRefusesAReportOlderThanTheLastVenueObservation(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	if err := m.Record(ctx, working("o1")); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := Progress(ctx, m, stamped("o1", orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED, 5, 5, 1_700_000_002_000)); err != nil {
		t.Fatalf("the first venue report must be recorded: %v", err)
	}

	replay := stamped("o1", orderpb.OrderStatus_ORDER_STATUS_ROUTED, 5, 5, 1_700_000_001_000)
	if err := Progress(ctx, m, replay); !errors.Is(err, ErrReportOutOfOrder) {
		t.Fatalf("a report stamped one second BEFORE the last one folded in returned %v, want "+
			"ErrReportOutOfOrder. Its cumulative is identical, so the quantity gate cannot see it "+
			"and the older status would be written over the newer one", err)
	}
	got, _, _, _ := m.Get(ctx, "o1")
	if got.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED {
		t.Errorf("after the refused replay the view holds status %v, want PARTIALLY_FILLED", got.GetStatus())
	}
}

// TestProgressAcceptsTwoReportsStampedInTheSameMillisecond is why the ordering
// gate is strictly-older rather than not-newer, and it is the case the venue
// produces most often — a taker order sweeping several makers emits one
// executionReport per maker, all carrying the same event time.
//
// A not-newer gate passes every other test here and DROPS REAL FILLS on this
// one, which is a far worse failure than the regression the gate exists for.
func TestProgressAcceptsTwoReportsStampedInTheSameMillisecond(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	if err := m.Record(ctx, working("o1")); err != nil {
		t.Fatalf("record: %v", err)
	}
	const ms = 1_700_000_002_000
	if err := Progress(ctx, m, stamped("o1", orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED, 4, 6, ms)); err != nil {
		t.Fatalf("first leg of the sweep: %v", err)
	}
	if err := Progress(ctx, m, stamped("o1", orderpb.OrderStatus_ORDER_STATUS_FILLED, 10, 0, ms)); err != nil {
		t.Fatalf("the second leg of the same sweep, stamped in the SAME millisecond, was refused: "+
			"%v. An exchange emits one report per maker filled and they share an event time; "+
			"refusing anything not strictly newer loses the rest of the sweep", err)
	}
	got, _, _, _ := m.Get(ctx, "o1")
	if c := got.GetFilledQuantity().GetCoefficient(); c != 10 {
		t.Errorf("after both legs the view holds filled_quantity = %d, want 10", c)
	}
}

// TestTheOrderingGateDoesNotFireBeforeTheFirstVenueObservation is the reason
// refuseStale asks venueObserved before comparing clocks, and the failure it
// prevents is the expensive one.
//
// A row this adapter has only DISPATCHED carries the OMS's as_of — the OMS's
// host clock at admission. The venue's clock is a different one, tied to it only
// loosely (both exchanges accept requests inside a recvWindow measured in
// seconds), so a market order filling within the skew arrives stamped EARLIER
// than admission. An unconditional gate refuses that first real fill.
func TestTheOrderingGateDoesNotFireBeforeTheFirstVenueObservation(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	seed := working("o1")
	// The OMS's admission stamp, on the OMS's clock, 40ms ahead of the venue's.
	seed.AsOf = venueAt(1_700_000_002_040)
	if err := m.Record(ctx, seed); err != nil {
		t.Fatalf("record: %v", err)
	}

	fill := stamped("o1", orderpb.OrderStatus_ORDER_STATUS_FILLED, 10, 0, 1_700_000_002_000)
	if err := Progress(ctx, m, fill); err != nil {
		t.Fatalf("the first venue fill was refused as stale against the OMS's own admission stamp: "+
			"%v. as_of is not one clock across every writer of this view, and a gate that forgets "+
			"that drops the first fill of every order that trades faster than the clock skew", err)
	}
	got, _, _, _ := m.Get(ctx, "o1")
	if c := got.GetFilledQuantity().GetCoefficient(); c != 10 {
		t.Errorf("the first venue fill did not land: filled_quantity = %d, want 10", c)
	}
}

// TestObservedSeamCountsAStaleReportOnItsOwnSeries.
//
// The two events must not share a counter. kanz_venue_orderview_read_failures_total
// is what an alert fires on to say the adapter has gone BLIND, and a websocket
// replaying on reconnect — a venue-side condition with no store fault behind it
// — would page an operator to go and look at Postgres.
func TestObservedSeamCountsAStaleReportOnItsOwnSeries(t *testing.T) {
	ctx := context.Background()
	var logs bytes.Buffer
	m := NewMemory()
	o := NewObservability("binance", "BINANCE")
	seam := NewObservedSeam(m, o, "BINANCE", slog.New(slog.NewTextHandler(&logs, nil)))

	if err := m.Record(ctx, working("o1")); err != nil {
		t.Fatalf("record: %v", err)
	}
	seam.Progressed(venueReport("o1", orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED, 8, 2))
	seam.Progressed(venueReport("o1", orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED, 3, 7))

	if got := testutil.ToFloat64(o.StaleReports.WithLabelValues("BINANCE", StaleQuantityRegressed)); got != 1 {
		t.Errorf("%s{reason=%q} = %v after one regressed report, want 1. A refusal nobody counted "+
			"is a refusal nobody can alert on, and this one is the adapter and the exchange "+
			"disagreeing about what filled", MetricStaleReports, StaleQuantityRegressed, got)
	}
	if got := testutil.ToFloat64(o.ReadFailures.WithLabelValues("BINANCE")); got != 0 {
		t.Errorf("%s = %v, want 0. The store answered every call here; routing a stale report onto "+
			"the read-failure series sends an operator to the database for a venue replay",
			MetricReadFailures, got)
	}
	msg := logs.String()
	for _, want := range []string{"REFUSED AS STALE", "The fill FACT was already published"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the ERROR does not mention %q — it read:\n%s", want, msg)
		}
	}
}

// TestObservedSeamStillCountsARealReadFailureAfterTheStaleBranch — the
// classifier must not swallow the case the read-failure counter exists for.
func TestObservedSeamStillCountsARealReadFailureAfterTheStaleBranch(t *testing.T) {
	store := &unreadableStore{Store: NewMemory(), fail: true}
	o := NewObservability("binance", "BINANCE")
	seam := NewObservedSeam(store, o, "BINANCE", slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))

	if _, _, err := seam.Lookup("o1"); err == nil {
		t.Fatal("Lookup over an unreadable store returned no error")
	}
	if got := testutil.ToFloat64(o.ReadFailures.WithLabelValues("BINANCE")); got != 1 {
		t.Errorf("%s = %v, want 1", MetricReadFailures, got)
	}
	if got := testutil.ToFloat64(o.StaleReports.WithLabelValues("BINANCE", StaleQuantityRegressed)); got != 0 {
		t.Errorf("%s{reason=%q} = %v, want 0 — a store outage is not a stale report",
			MetricStaleReports, StaleQuantityRegressed, got)
	}
}

// TestStaleReportsIsSeededAtZeroForEveryReason.
//
// An un-incremented CounterVec label exports NO SERIES AT ALL, so an alert
// written `... {reason="filled_quantity_regressed"} == 0` evaluates against an
// empty vector on every adapter that has never refused a report — which is every
// adapter, right up until the stream it exists for starts replaying. This estate
// has shipped that exact silence in #963, #973 and #1064.
//
// It gathers from a REGISTRY rather than reading the vector, because that is the
// only thing that answers the question actually being asked: what does /metrics
// expose before anything happens.
func TestStaleReportsIsSeededAtZeroForEveryReason(t *testing.T) {
	o := NewObservability("binance", "BINANCE")
	reg := prometheus.NewRegistry()
	reg.MustRegister(o.Collectors()...)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var seen []string
	for _, f := range families {
		if f.GetName() != MetricStaleReports {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "reason" {
					seen = append(seen, l.GetValue())
				}
			}
		}
	}
	for _, want := range []string{StaleQuantityRegressed, StaleReportOutOfOrder} {
		if !containsString(seen, want) {
			t.Errorf("%s exports no series for reason=%q before anything increments it (it exports "+
				"%v). An `== 0` alert over a label with no series never fires, and the state it is "+
				"written for is exactly the state before the first increment",
				MetricStaleReports, want, seen)
		}
	}
}

// TestStaleReasonNamesBothRefusalsAndNothingElse pins the classifier
// NewObservedSeam routes on. A sentinel added to refuseStale and not to
// StaleReason is a refusal counted as an unreadable view — the alert that pages
// an operator to the database for a venue-side replay.
func TestStaleReasonNamesBothRefusalsAndNothingElse(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
		ok   bool
	}{
		{ErrFilledQuantityRegressed, StaleQuantityRegressed, true},
		{ErrReportOutOfOrder, StaleReportOutOfOrder, true},
		{ErrNotInView, "", false},
		{ErrTerminalNotReopened, "", false},
		{ErrContended, "", false},
		{errStoreDown, "", false},
	} {
		got, ok := StaleReason(tc.err)
		if ok != tc.ok || got != tc.want {
			t.Errorf("StaleReason(%v) = (%q, %v), want (%q, %v)", tc.err, got, ok, tc.want, tc.ok)
		}
	}
	// Progress returns these WRAPPED with %w. If the classifier stopped seeing
	// through a wrap it would read every real refusal as a store failure, and
	// every test above would still pass because they match the sentinel directly.
	wrapped := errors.Join(errors.New("orderview: order o1"), ErrReportOutOfOrder)
	if _, ok := StaleReason(wrapped); !ok {
		t.Error("StaleReason does not see through a wrap, so the refusals Progress actually " +
			"returns would be counted as read failures")
	}
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// TestProgressRefusesAStaleReportAgainstPostgres is the same two gates against
// the store both deployed adapters actually run on.
//
// IT IS NOT A DUPLICATE OF THE MEMORY TESTS. Memory holds the OrderState as a
// proto in the process; Postgres marshals it into BYTEA and decodes it back, so
// the value refuseStale compares against has been through a round trip — and
// Memory ignores ctx entirely while pgx honours it, so a read-modify-write is a
// genuinely different operation here. Both gates read the value the STORE
// returned, which is the thing this exercises and Memory cannot.
func TestProgressRefusesAStaleReportAgainstPostgres(t *testing.T) {
	f := newPGFixture(t)
	st, _ := f.storeAs(t, "acme")
	ctx := context.Background()

	seed := routed("o-stale")
	seed.Quarantine = nil // Progress does not consult it; leaving it in would only obscure the case.
	if err := st.Record(ctx, seed); err != nil {
		t.Fatalf("Record: %v", err)
	}

	first := stamped("o-stale", orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED, 8, 2, 1_700_000_002_000)
	if err := Progress(ctx, st, first); err != nil {
		t.Fatalf("the first venue report must be recorded: %v", err)
	}

	late := stamped("o-stale", orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED, 3, 7, 1_700_000_003_000)
	if err := Progress(ctx, st, late); !errors.Is(err, ErrFilledQuantityRegressed) {
		t.Fatalf("cumulative 3 over a stored 8 returned %v, want ErrFilledQuantityRegressed. The "+
			"held value came back through a proto round trip, and a comparison that reads it "+
			"wrongly is invisible on the in-memory store", err)
	}

	replay := stamped("o-stale", orderpb.OrderStatus_ORDER_STATUS_ROUTED, 8, 2, 1_700_000_001_000)
	if err := Progress(ctx, st, replay); !errors.Is(err, ErrReportOutOfOrder) {
		t.Fatalf("a report stamped before the stored observation returned %v, want ErrReportOutOfOrder", err)
	}

	got, _, ok, err := st.Get(ctx, "o-stale")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if c := got.GetFilledQuantity().GetCoefficient(); c != 8 {
		t.Errorf("the durable view holds filled_quantity = %d after two refused reports, want 8", c)
	}
	if got.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED {
		t.Errorf("the durable view holds status %v, want PARTIALLY_FILLED", got.GetStatus())
	}
}
