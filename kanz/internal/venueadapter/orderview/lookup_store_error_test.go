package orderview

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/execution"
)

var errStoreDown = errors.New("orderview: connection refused")

// unreadableStore answers every Get with a failure and delegates the rest, so
// the row genuinely IS present and the only thing wrong is that it cannot be
// read — the shape of a Postgres blip, not of an empty view.
type unreadableStore struct {
	Store
	fail bool
}

func (s *unreadableStore) Get(ctx context.Context, orderID string) (*orderpb.OrderState, Revision, bool, error) {
	if s.fail {
		return nil, Revision{}, false, errStoreDown
	}
	return s.Store.Get(ctx, orderID)
}

// AN UNREADABLE VIEW AND AN ORDER WE DO NOT HOLD ARE DIFFERENT ANSWERS (#1047).
//
// Lookup returned (nil, false) for both. The user-data ingesters read the `ok`
// and skipped, which is the right answer to exactly one of them: a report for an
// order this adapter never dispatched belongs to another account or another
// replica, and skipping it is correct. A report this adapter could not resolve
// because its own store failed is the opposite case — the adapter almost
// certainly DOES own the order — and skipping it deletes a real execution from
// the order.order.filled stream, so the position book never books the position
// and the ledger never journals the cash.
//
// The information was always here; it was discarded one line later.
func TestSeamLookupStoreErrorIsNotAMiss(t *testing.T) {
	ctx := context.Background()
	mem := NewMemory()
	if err := mem.Record(ctx, working("o1")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	store := &unreadableStore{Store: mem}

	var reported []error
	seam := NewSeam(store, func(err error) { reported = append(reported, err) })

	// The control: the view is readable and holds the order.
	st, ok, err := seam.Lookup("o1")
	if err != nil || !ok || st == nil {
		t.Fatalf("Lookup on a healthy view = (%v, %v, %v), want the order", st, ok, err)
	}

	// The view is readable and does NOT hold the order: a miss, with no error.
	if st, ok, err := seam.Lookup("not-ours"); err != nil || ok || st != nil {
		t.Fatalf("Lookup of an order this adapter does not hold = (%v, %v, %v), want (nil, false, nil) — "+
			"reporting an error here would tear a websocket down on every frame belonging to "+
			"another account", st, ok, err)
	}

	// The view CANNOT BE READ: not a miss.
	store.fail = true
	st, ok, err = seam.Lookup("o1")
	if err == nil {
		t.Fatal("Lookup returned no error for a store that could not be read — the caller sees the " +
			"same (nil, false) it gets for somebody else's order, and the only safe reading of " +
			"that answer is the wrong one here: this adapter holds the order and cannot tell")
	}
	if !errors.Is(err, errStoreDown) {
		t.Errorf("Lookup returned %v, which does not wrap the store's own failure", err)
	}
	if ok {
		t.Error("Lookup reported the order held while the view was unreadable — a state it did not read")
	}
	if st != nil {
		t.Errorf("Lookup returned a state (%v) it could not have read", st)
	}

	// AND IT STILL REACHES onErr. Both, deliberately: onErr is where the
	// read-failure counter hangs, so dropping it in favour of the return value
	// would make the outage unalertable even though the caller now handles it.
	if len(reported) != 1 {
		t.Fatalf("onErr fired %d times (%v), want exactly one — the read-failure counter is "+
			"incremented there, and a log line is not a threshold", len(reported), reported)
	}
}

// BOTH SERIES EXIST, AT ZERO, BEFORE ANYTHING GOES WRONG (#1047).
//
// An un-incremented CounterVec label exports no series at all, so an alert
// written `kanz_venue_fill_reports_dropped_total{reason="store_error"} == 0`
// evaluates against an empty vector on an adapter that has never dropped a
// report — which is every adapter, right up until the outage the alert exists
// for. Seeding happens inside NewObservability rather than beside it precisely
// so a composition root cannot register the collector and forget the loop; this
// pins that it does.
func TestObservabilitySeedsEveryLabelAtZero(t *testing.T) {
	reg := prometheus.NewRegistry()
	o := NewObservability("binance", "BINANCE")
	reg.MustRegister(o.Collectors()...)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	got := map[string]map[string]float64{}
	for _, f := range families {
		series := map[string]float64{}
		for _, m := range f.GetMetric() {
			key := ""
			for _, l := range m.GetLabel() {
				if l.GetName() == "reason" || l.GetName() == "mic" {
					key += l.GetName() + "=" + l.GetValue() + ";"
				}
			}
			series[key] = m.GetCounter().GetValue()
		}
		got[f.GetName()] = series
	}

	want := map[string][]string{
		MetricReadFailures:   {"mic=BINANCE;"},
		MetricReportsDropped: {"mic=BINANCE;reason=" + execution.DropUnknownOrder + ";", "mic=BINANCE;reason=" + execution.DropStoreError + ";"},
	}
	for name, keys := range want {
		series, ok := got[name]
		if !ok {
			t.Errorf("%s exports no series at startup — an operator cannot tell \"nothing has been "+
				"dropped\" from \"nobody wired the metric\"", name)
			continue
		}
		for _, k := range keys {
			v, ok := series[k]
			if !ok {
				t.Errorf("%s{%s} exports no series at startup", name, k)
				continue
			}
			if v != 0 {
				t.Errorf("%s{%s} starts at %v, want 0", name, k, v)
			}
		}
	}

	// A REGISTRATION THAT PANICS CRASH-LOOPS THE POD, and MustRegister runs
	// inside serve() where no other test reaches it. Gathering above already
	// proved the names and label sets are acceptable to a real registry.
	if _, ok := got[MetricReadFailures]; !ok {
		t.Fatalf("%s did not survive registration", MetricReadFailures)
	}

	// THE COUNTER ACTUALLY MOVES, and under the label the callback uses.
	o.ReportsDropped.WithLabelValues("BINANCE", execution.DropStoreError).Inc()
	var after *dto.MetricFamily
	families, _ = reg.Gather()
	for _, f := range families {
		if f.GetName() == MetricReportsDropped {
			after = f
		}
	}
	total := 0.0
	for _, m := range after.GetMetric() {
		total += m.GetCounter().GetValue()
	}
	if total != 1 {
		t.Errorf("%s totals %v after one increment, want 1", MetricReportsDropped, total)
	}
}

// THE OBSERVED SEAM MOVES THE COUNTER AND SAYS WHAT WAS LOST (#1047).
//
// This is the arm that fails silently and has done so twice in this estate
// (#963, #973): a collector that is built, registered and seeded, and then never
// incremented, reads as a permanently healthy adapter. It is asserted here
// rather than in a composition root because everything in a root is a local that
// no test can reach — which is exactly why the increment was moved out of one.
//
// The ERROR is asserted alongside it because the message is the other half of
// the repair. The one both roots carried named only the healing watchdog
// ("reconciliation is degraded"), which sends an operator to the wrong system:
// the larger consequence is that fills are leaving the FACT stream, so no
// position is booked and no cash is journalled.
func TestObservedSeamCountsAndNamesTheReadFailure(t *testing.T) {
	var logs bytes.Buffer
	store := &unreadableStore{Store: NewMemory(), fail: true}
	o := NewObservability("binance", "BINANCE")
	seam := NewObservedSeam(store, o, "BINANCE", slog.New(slog.NewTextHandler(&logs, nil)))

	if _, _, err := seam.Lookup("o1"); err == nil {
		t.Fatal("Lookup did not report the unreadable store")
	}

	if got := testutil.ToFloat64(o.ReadFailures.WithLabelValues("BINANCE")); got != 1 {
		t.Errorf("%s{mic=\"BINANCE\"} = %v after one failed read, want 1 — a counter that never "+
			"moves is a series pinned at zero, which an `== 0` alert reads as a healthy adapter "+
			"for the whole duration of the outage", MetricReadFailures, got)
	}

	msg := logs.String()
	for _, want := range []string{"DROPPED from the FACT stream", "no position booked", "open at the venue"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the ERROR does not mention %q — it read:\n%s", want, msg)
		}
	}
}
