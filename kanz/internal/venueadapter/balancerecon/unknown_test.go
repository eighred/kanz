package balancerecon

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/eighred/kanz/internal/execution"
)

// EVERY REASON EXPORTS A SERIES BEFORE ANY ASSET IS SKIPPED (#1063).
//
// This is the half that is easy to leave out and the half an alert depends on.
// An un-incremented CounterVec label exports NO series at all, so a reason nobody
// has hit yet is indistinguishable from a metric nobody registered — and a rule
// over a series that does not exist is silent in exactly the state it detects.
// That has shipped twice on this estate (#973, #963).
//
// The seeding is asserted through the CONSTRUCTOR rather than by a loop written
// here, because the constructor is what the composition roots call: a test that
// seeds the counter itself proves its own loop, not theirs.
func TestUnknownCounterExportsEveryReasonAtZero(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := NewUnknownCounter("binance")
	reg.MustRegister(c)

	seen := gatherReasons(t, reg)
	for _, reason := range execution.BalanceUnknownReasons {
		v, ok := seen[reason]
		if !ok {
			t.Errorf("reason %q exports no series at startup — an operator cannot tell "+
				"\"every asset was compared\" from \"nobody wired the metric\"", reason)
			continue
		}
		if v != 0 {
			t.Errorf("reason %q starts at %v, want 0", reason, v)
		}
	}
	if len(seen) != len(execution.BalanceUnknownReasons) {
		t.Errorf("the counter exports %d reason series, want %d (%v) — a reason that is seeded "+
			"and not in the shared set, or the reverse, means the alert and the code disagree "+
			"about what can happen", len(seen), len(execution.BalanceUnknownReasons), seen)
	}
}

// THE RATE-LIMIT SLOTS AND THE SHARED REASON SET MUST STAY THE SAME SIZE.
//
// The observer keeps its per-reason log state in a fixed array. A fourth reason
// added to execution.BalanceUnknownReasons and not to unknownReasonIndex would
// fall to the unattributed slot silently — which is the same class of quiet
// collapse this whole seam exists to remove.
func TestEveryReasonHasARateLimitSlot(t *testing.T) {
	if len(execution.BalanceUnknownReasons) != unknownReasonSlots {
		t.Fatalf("execution.BalanceUnknownReasons has %d entries and the observer keeps %d slots",
			len(execution.BalanceUnknownReasons), unknownReasonSlots)
	}
	if len(unknownReasonIndex) != unknownReasonSlots {
		t.Fatalf("unknownReasonIndex has %d entries and the observer keeps %d slots",
			len(unknownReasonIndex), unknownReasonSlots)
	}
	for _, reason := range execution.BalanceUnknownReasons {
		if _, ok := unknownReasonIndex[reason]; !ok {
			t.Errorf("reason %q has no rate-limit slot", reason)
		}
	}
}

// THE COUNTER MOVES ON EVERY ASSET AND THE LOG DOES NOT.
//
// Two signals, not one, and the split matters operationally. The counter is the
// volume — "how much of this account is going unchecked" — and it must not be
// rate-limited or the number is a lie. The ERROR log is a diagnosis, and the
// exchange lists hundreds of assets, so a line per asset per pass buries the rest
// of the log on the day it matters.
func TestEveryUnknownIsCountedAndTheLogIsBounded(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := NewUnknownCounter("okx")
	reg.MustRegister(c)
	h := &capturingHandler{}
	now := time.Unix(0, 0).UTC()
	o := NewUnknownObserver(c, slog.New(h), "okx",
		WithUnknownLogEvery(15*time.Minute),
		WithUnknownClock(func() time.Time { return now }))

	for _, asset := range []string{"USDT", "BTC", "ETH", "SOL", "DOGE"} {
		o.Observe(asset, execution.BalanceUnknownNeverAnnounced)
	}

	if got := gatherReasons(t, reg)[execution.BalanceUnknownNeverAnnounced]; got != 5 {
		t.Errorf("counter = %v after five unchecked assets, want 5 — the counter is the volume "+
			"signal and must not be rate-limited", got)
	}
	if n := h.errorCount(); n != 1 {
		t.Errorf("wrote %d ERROR lines for five assets sharing one reason, want 1 — the "+
			"exchange lists hundreds of assets and this runs once a minute", n)
	}
	// The line has to name the consequence, not the condition. An operator reading
	// "balance unknown" has no way to know the clean-looking reconciliation beside
	// it is the symptom.
	if !h.errorContaining("COULD NOT CHECK", "reported no break") {
		t.Fatalf("no ERROR naming what the silence means; records: %+v", h.records)
	}

	// The window elapses: the next asset earns a line again, and it carries how
	// many were suppressed — otherwise the operator reads one asset and infers one.
	now = now.Add(16 * time.Minute)
	o.Observe("USDC", execution.BalanceUnknownNeverAnnounced)
	if n := h.errorCount(); n != 2 {
		t.Errorf("wrote %d ERROR lines after the window elapsed, want 2", n)
	}
	if !h.errorAttr("suppressed_since_last_line", int64(4)) {
		t.Errorf("the second line does not report the four assets suppressed since the first — " +
			"a rate-limited line that hides its own suppression understates the outage")
	}
}

// A SECOND REASON IS NOT SUPPRESSED BY THE FIRST. They are separate incidents —
// never-announced is a spine that has not started, stale is one that has stopped
// — and a shared window would hide the transition between them.
func TestEachReasonHasItsOwnLogWindow(t *testing.T) {
	h := &capturingHandler{}
	now := time.Unix(0, 0).UTC()
	o := NewUnknownObserver(NewUnknownCounter("binance"), slog.New(h), "binance",
		WithUnknownLogEvery(time.Hour),
		WithUnknownClock(func() time.Time { return now }))

	o.Observe("USDT", execution.BalanceUnknownNeverAnnounced)
	o.Observe("USDT", execution.BalanceUnknownStale)
	o.Observe("BTC", execution.BalanceUnknownNeverAnnounced)

	if n := h.errorCount(); n != 2 {
		t.Fatalf("wrote %d ERROR lines for two distinct reasons, want 2", n)
	}
}

// A NON-POSITIVE WINDOW LOGS EVERY UNKNOWN — the option a test uses when it
// wants the line for each asset.
//
// THIS ARM WAS FOUND BY MUTATION (#1063): `false &&` on the `logEvery <= 0`
// disjunct alone changed nothing observable, because every other test supplies a
// positive window and the first call is always due. A disjunct no test can
// distinguish is a disjunct that could be deleted, and this seam's whole subject
// is the difference between "checked" and "nothing looked".
func TestANonPositiveWindowLogsEveryUnknown(t *testing.T) {
	h := &capturingHandler{}
	now := time.Unix(0, 0).UTC()
	o := NewUnknownObserver(NewUnknownCounter("binance"), slog.New(h), "binance",
		WithUnknownLogEvery(0),
		WithUnknownClock(func() time.Time { return now }))

	for _, asset := range []string{"USDT", "BTC", "ETH"} {
		o.Observe(asset, execution.BalanceUnknownStale)
	}

	if n := h.errorCount(); n != 3 {
		t.Fatalf("wrote %d ERROR lines with the window disabled, want 3", n)
	}
}

// AN UNRECOGNISED REASON IS NAMED, NOT PASSED THROUGH. The label is a Prometheus
// dimension reached from an exchange response; a seam that returns a new string
// must not be able to create a new series.
func TestAnUnrecognisedReasonBecomesUnattributed(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := NewUnknownCounter("binance")
	reg.MustRegister(c)
	o := NewUnknownObserver(c, slog.New(&capturingHandler{}), "binance")

	o.Observe("USDT", "")
	o.Observe("BTC", "something-a-future-seam-invented")

	seen := gatherReasons(t, reg)
	if got := seen[execution.BalanceUnknownUnattributed]; got != 2 {
		t.Errorf("unattributed = %v, want 2", got)
	}
	if len(seen) != len(execution.BalanceUnknownReasons) {
		t.Errorf("the counter grew to %d series (%v) — an unrecognised reason created a new "+
			"label rather than being folded into the named one", len(seen), seen)
	}
}

// A nil counter and a nil logger must not panic: this runs inside a background
// reconciliation goroutine, and losing the process is a worse outcome than
// losing one of the two signals.
func TestNilObserverCollaboratorsDoNotPanic(t *testing.T) {
	NewUnknownObserver(nil, nil, "binance").Observe("USDT", execution.BalanceUnknownStale)
}

func gatherReasons(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var got *dto.MetricFamily
	for _, f := range families {
		if f.GetName() == UnknownMetricName {
			got = f
		}
	}
	if got == nil {
		t.Fatalf("%s exports no series at all", UnknownMetricName)
	}
	out := map[string]float64{}
	for _, m := range got.GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == "reason" {
				out[l.GetValue()] = m.GetCounter().GetValue()
			}
		}
	}
	return out
}

func (h *capturingHandler) errorCount() int {
	n := 0
	for _, r := range h.records {
		if r.Level == slog.LevelError {
			n++
		}
	}
	return n
}

func (h *capturingHandler) errorContaining(subs ...string) bool {
	for _, r := range h.records {
		if r.Level != slog.LevelError {
			continue
		}
		all := true
		for _, s := range subs {
			if !strings.Contains(r.Message, s) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func (h *capturingHandler) errorAttr(key string, want int64) bool {
	found := false
	for _, r := range h.records {
		if r.Level != slog.LevelError {
			continue
		}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == key && a.Value.Int64() == want {
				found = true
			}
			return true
		})
	}
	return found
}
