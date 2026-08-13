package balancerecon

import (
	"context"
	"log/slog"
	"math/big"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// capturingHandler collects records so a test can assert not merely THAT
// something was logged but that the line names the operational consequence — the
// standard the in-memory order-view warning is already held to.
type capturingHandler struct{ records []slog.Record }

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r)
	return nil
}
func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

func (h *capturingHandler) warnContaining(subs ...string) bool {
	for _, r := range h.records {
		if r.Level != slog.LevelWarn {
			continue
		}
		msg := r.Message
		all := true
		for _, s := range subs {
			if !strings.Contains(msg, s) {
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

// staticBalances is a bound seam — the shape a real implementation would have.
type staticBalances map[string]*big.Rat

func (b staticBalances) Balance(asset string) (*big.Rat, bool) {
	v, ok := b[asset]
	return v, ok
}

// AN UNWIRED SEAM READS 0 AND SAYS WHY (#418).
//
// This is the whole point of the package. Before it, both venue mains simply
// omitted the field: no gauge, no log, and reconcileBalances returning nil. The
// platform had never compared its books against an exchange, and nothing on any
// dashboard could distinguish that from agreement.
func TestAnUnwiredSeamReportsZeroAndNamesTheConsequence(t *testing.T) {
	g := NewGauge("binance")
	h := &capturingHandler{}

	got := Announce(g, slog.New(h), "binance", nil)

	if got != nil {
		t.Fatalf("Announce returned %v for a nil seam, want nil — it must not fabricate one", got)
	}
	if v := testutil.ToFloat64(g); v != 0 {
		t.Errorf("%s = %v, want 0 for an unwired seam", MetricName, v)
	}
	// The log must carry the CONSEQUENCE, not just the fact. An operator reading
	// "balances not configured" has no way to know that the silence they are
	// looking at on accounting.balance.reconciled is the symptom.
	if !h.warnContaining("NO COMPARISON HAS RUN", "accounting.balance.reconciled") {
		t.Fatalf("no WARN naming the subject and what its silence means; records: %+v", h.records)
	}
}

// A WIRED SEAM READS 1 AND IS RETURNED UNCHANGED. Without this, "report 0" is
// satisfied by a function that always reports 0 and discards the seam — which
// would disable the very comparison it is reporting on.
func TestAWiredSeamReportsOneAndIsPassedThrough(t *testing.T) {
	g := NewGauge("okx")
	h := &capturingHandler{}
	want := staticBalances{"USDT": big.NewRat(100, 1)}

	got := Announce(g, slog.New(h), "okx", want)

	if got == nil {
		t.Fatal("Announce dropped a bound seam — the comparison would never run")
	}
	if bal, ok := got.Balance("USDT"); !ok || bal.Cmp(big.NewRat(100, 1)) != 0 {
		t.Fatalf("Balance(USDT) = %v, want 100 — the seam must be returned unchanged, not wrapped "+
			"in something that answers differently", bal)
	}
	if v := testutil.ToFloat64(g); v != 1 {
		t.Errorf("%s = %v, want 1 for a wired seam", MetricName, v)
	}
	if len(h.records) != 0 {
		t.Errorf("a WIRED seam logged %d record(s), want none — a warning that fires when nothing "+
			"is wrong is how an operator learns to ignore the warning: %+v", len(h.records), h.records)
	}
}

// The gauge carries the venue, because an estate runs one adapter per venue and
// "balance reconciliation is off" is only actionable if it says WHERE.
func TestTheGaugeIsLabelledByVenue(t *testing.T) {
	for _, venue := range []string{"binance", "okx"} {
		g := NewGauge(venue)
		Announce(g, nil, venue, nil)
		if err := testutil.CollectAndCompare(g, strings.NewReader(
			"# HELP "+MetricName+" 1 if the venue adapter can compare Kanz's balances against the exchange's, 0 if not. 0 means accounting.balance.reconciled is silent because NO COMPARISON RUNS, not because the books agree.\n"+
				"# TYPE "+MetricName+" gauge\n"+
				MetricName+`{venue="`+venue+`"} 0`+"\n"), MetricName); err != nil {
			t.Errorf("venue %q: %v", venue, err)
		}
	}
}

// A nil gauge and a nil logger must not panic. The composition roots always pass
// both, but this function is reached during startup — the one place where a
// panic turns an observability gap into a crash loop, which is strictly worse
// than the gap it reports.
func TestNilCollaboratorsDoNotPanic(t *testing.T) {
	if got := Announce(nil, nil, "binance", nil); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}
