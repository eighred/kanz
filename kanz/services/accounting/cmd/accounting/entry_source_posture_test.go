package main

// The ledger folds six kinds of journal entry; two of them — corporate_action
// and accrual — have no producer anywhere in the module, so a split, dividend,
// merger or coupon adjusts nothing and the book looks exactly like one with
// nothing to adjust. These tests pin the three things that make that visible:
// a series for EVERY entry type including the zeroes, a WARN naming what is
// unproduced and what would arm it, and — the trap liquidity.go fell into — the
// guarantee that an entry type added to the ledger and forgotten here still gets
// a series rather than none at all. #588.

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/services/accounting/internal/config"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// wiredSeries reads kanz_accounting_entry_source_wired off a registry as
// label→value. A missing metric returns a nil map, which every caller below
// distinguishes from an empty one: "absent" is the failure mode under test.
func wiredSeries(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "kanz_accounting_entry_source_wired" {
			continue
		}
		out := map[string]float64{}
		for _, m := range f.GetMetric() {
			var label string
			for _, l := range m.GetLabel() {
				if l.GetName() == "type" {
					label = l.GetValue()
				}
			}
			out[label] = m.GetGauge().GetValue()
		}
		return out
	}
	return nil
}

// fullyWired is a config with a broker and both subject sets — the deployment an
// operator would call correctly configured. corporate_action must STILL read 0
// under it: no environment variable arms it, because no publisher exists.
func fullyWired() config.Config {
	return config.Config{
		NATSURL:      "nats://localhost:4222",
		FillSubjects: config.DefaultFillSubjects,
		CashSubjects: config.DefaultCashSubjects,
	}
}

// EVERY entry type the ledger declares gets a series, including the zeroes. A
// metric that appeared only for the produced kinds would make an unproduced one
// absent rather than zero — indistinguishable from a service that never had it,
// which is the silence this posture exists to remove.
func TestEverySeriesIsSeededIncludingTheZeroes(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := &storeLogCapture{}

	stateEntrySourcePosture(reg, slog.New(h), fullyWired())

	series := wiredSeries(t, reg)
	if series == nil {
		t.Fatalf("kanz_accounting_entry_source_wired is ABSENT from the registry — the posture is " +
			"not on /metrics at all")
	}
	for _, typ := range ledger.EntryTypes {
		if typ == ledger.EntryUnspecified {
			continue
		}
		if _, ok := series[typ.String()]; !ok {
			t.Errorf("no series for entry type %q: an unseeded label is ABSENT rather than zero, so "+
				"the gap it reports is itself invisible", typ)
		}
	}
	// One per declared type, minus the unspecified zero value.
	if want := len(ledger.EntryTypes) - 1; len(series) != want {
		t.Errorf("got %d series %v, want %d (one per declared entry type)", len(series), series, want)
	}
}

// The finding itself, kept executable: corporate_action reads 0 on a deployment
// with a broker and every subject configured, because nothing publishes an
// announcement. The day something does, this test fails and says why.
func TestCorporateActionIsUnproducedEvenWhenFullyConfigured(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := &storeLogCapture{}

	unwired := stateEntrySourcePosture(reg, slog.New(h), fullyWired())

	series := wiredSeries(t, reg)
	if got, ok := series["corporate_action"]; !ok || got != 0 {
		t.Fatalf("kanz_accounting_entry_source_wired{type=\"corporate_action\"} = %v (present=%v), "+
			"want 0: nothing in this module publishes accounting.v1.CorporateAction. If a feed now "+
			"exists, wire it and delete the dark-capability exemption for "+
			"services/accounting/internal/corpact", got, ok)
	}
	if got := series["accrual"]; got != 0 {
		t.Errorf("accrual = %v, want 0: nothing calls accounting.AccrualEntry", got)
	}
	for _, name := range []string{"trade", "cash", "fee"} {
		if got := series[name]; got != 1 {
			t.Errorf("%s = %v, want 1 on a fully configured deployment", name, got)
		}
	}
	if strings.Join(unwired, ",") != "accrual,corporate_action" {
		t.Errorf("unwired = %v, want [accrual corporate_action]", unwired)
	}
}

// An operator has to be able to read WHAT is unproduced and WHAT WOULD ARM IT
// out of the log, at a level that is not filtered away. Info is where this would
// be lost.
func TestUnproducedKindsWarnWithTheReasonAndTheRemedy(t *testing.T) {
	h := &storeLogCapture{}

	stateEntrySourcePosture(prometheus.NewRegistry(), slog.New(h), fullyWired())

	if !h.hasWarnContaining("NOTHING PRODUCES this kind of journal entry") {
		t.Fatalf("no per-kind WARN; got records: %+v", h.records)
	}
	if !h.hasWarnContaining("NOT every journal entry type this book folds has a producer") {
		t.Fatalf("no summary WARN naming the posture; got records: %+v", h.records)
	}
	// The remedy has to name the missing feed, and has to say that fabricating
	// one is not the fix — a reader who arms it with invented announcements makes
	// the book confidently wrong instead of visibly untouched.
	joined := allAttrText(h)
	for _, want := range []string{
		"corporate_action",
		"accounting.v1.CorporateAction has no publisher",
		"announcement feed",
		"Fabricating announcements is NOT the fix",
		"#588",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the corporate-action posture never says %q; an operator cannot act on it", want)
		}
	}
}

// A deployment with no broker folds no fills and no cash either, and must say so
// per kind rather than implying trade is live.
func TestReadOnlyDeploymentReportsEveryKindUnproduced(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := &storeLogCapture{}

	unwired := stateEntrySourcePosture(reg, slog.New(h), config.Config{
		FillSubjects: config.DefaultFillSubjects,
		CashSubjects: config.DefaultCashSubjects,
	})

	series := wiredSeries(t, reg)
	for name, got := range series {
		if got != 0 {
			t.Errorf("%s = %v with no ACCOUNTING_NATS_URL, want 0 — nothing is folded at all", name, got)
		}
	}
	if len(unwired) != len(ledger.EntryTypes)-1 {
		t.Errorf("unwired = %v, want every declared kind", unwired)
	}
}

// THE TRAP, PINNED. liquidity.go hand-enumerated its metric reason set, so a
// reason added elsewhere shipped with no series at all until its first
// increment — absent rather than zero, which makes the very gap it reports
// invisible. This posture seeds from ledger.EntryTypes instead, so a type
// declared in the ledger and NOT described here still gets a series, at 0, with
// an ERROR naming it. Simulated by deleting a declared type from the map, which
// is exactly the state a newly added type starts in.
func TestAnUndescribedEntryTypeIsStillSeededAndSaysSo(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := &storeLogCapture{}
	orphan := ledger.EntryTrade // described today; pretend nobody described it

	postures := entrySourcePostures(fullyWired())
	delete(postures, orphan)
	unwired := seedEntrySourcePosture(reg, slog.New(h), postures)

	series := wiredSeries(t, reg)
	got, ok := series[orphan.String()]
	if !ok {
		t.Fatalf("entry type %q has no posture declared and got NO SERIES AT ALL — it is absent "+
			"rather than zero, which is the exact hole this seeding exists to close", orphan)
	}
	if got != 0 {
		t.Errorf("undescribed entry type %q reported %v, want 0 — nothing says it is produced", orphan, got)
	}
	if !containsString(unwired, orphan.String()) {
		t.Errorf("undescribed entry type %q missing from the unwired list %v", orphan, unwired)
	}
	if !h.hasErrorContaining("NO PRODUCER POSTURE DECLARED") {
		t.Fatalf("an entry type with no declared posture was seeded SILENTLY; got records: %+v", h.records)
	}
	if !strings.Contains(allAttrText(h), orphan.String()) {
		t.Errorf("the ERROR does not name the undescribed type %q", orphan)
	}
}

// Every entry type the ledger declares is described here today. The test above
// proves a forgotten one is still visible; this one proves none is forgotten.
func TestEveryDeclaredEntryTypeHasADeclaredPosture(t *testing.T) {
	postures := entrySourcePostures(fullyWired())
	for _, typ := range ledger.EntryTypes {
		if typ == ledger.EntryUnspecified {
			continue
		}
		p, ok := postures[typ]
		if !ok {
			t.Errorf("entry type %q has no entry in entrySourcePostures", typ)
			continue
		}
		if p.what == "" {
			t.Errorf("entry type %q does not say what produces it", typ)
		}
		if !p.wired && (p.why == "" || p.arm == "") {
			t.Errorf("entry type %q is unproduced but does not say why, or what would arm it", typ)
		}
	}
}

func containsString(haystack []string, want string) bool {
	for _, s := range haystack {
		if s == want {
			return true
		}
	}
	return false
}

// hasErrorContaining is the ERROR-level counterpart of storeLogCapture's
// hasWarnContaining. The level matters: an entry type nobody described is a
// coding omission rather than a deployment posture, so it must not sit at the
// same level as the postures an operator is expected to read and accept.
func (h *storeLogCapture) hasErrorContaining(substrs ...string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Level != slog.LevelError {
			continue
		}
		match := true
		for _, s := range substrs {
			if !strings.Contains(r.Message, s) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// allAttrText flattens every captured record's message and attribute values into
// one string, so a test can assert that the operator-facing detail is present
// without depending on which attribute carries it.
func allAttrText(h *storeLogCapture) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var b strings.Builder
	for _, r := range h.records {
		b.WriteString(r.Message)
		b.WriteString(" ")
		r.Attrs(func(a slog.Attr) bool {
			b.WriteString(a.Key)
			b.WriteString("=")
			b.WriteString(a.Value.String())
			b.WriteString(" ")
			return true
		})
	}
	return b.String()
}
