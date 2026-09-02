package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/marketdata/mark"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	"github.com/eighred/kanz/services/oms/internal/config"
)

// THE UNPRICED OBSERVER, THROUGH THE BUILDER THE COMPOSITION ROOT CALLS (#883).
//
// The callback under test is a closure inside buildPreTradeGate. Nothing but the
// builder can reach it, and cmd/*/main.go wiring escapes every unit test — this
// platform has twice shipped a startup crash under a green suite — so these drive
// the gate the real startup builds and read the counter and the log it actually
// produced.
//
// Two properties, and they are deliberately in tension:
//
//   - the COUNTER is unconditional. It is a rate: how fast orders are being
//     refused for want of a price. Gating it on the say-once verdict would turn
//     the series into a count of distinct instruments first seen unpriced,
//     silently, under the same metric name.
//   - the LOG is deduped, once per (tenant, portfolio, instrument), matching the
//     gate's own WARN. An operator wants to be told the condition CHANGED; the
//     rate is already on the counter.

const unpricedMaxAge = 30 * time.Second

var unpricedBase = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

// governedMandates governs every portfolio with a mandate carrying one rule.
// That is the minimum that gets an order past the ungoverned and unconstrained
// short-circuits and down to the price check, which is the branch under test.
type governedMandates struct{}

func (governedMandates) Mandate(context.Context, string, string, time.Time) (*compliancepb.Mandate, comp.Governance, error) {
	return &compliancepb.Mandate{
		MandateId: "m1", TenantId: "acme", PortfolioId: "PF1", Version: 1,
		Rules: []*compliancepb.Rule{{
			RuleId: "r1", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
		}},
	}, comp.Governed, nil
}

// unpricedRig builds the gate the composition root builds, over a mark fold whose
// clock the caller drives, and returns the registry and the log an operator would
// have read.
type unpricedRig struct {
	gate  *comp.PreTradeGate
	marks *mark.Source
	reg   *prometheus.Registry
	log   *bytes.Buffer
}

func newUnpricedRig(t *testing.T, now func() time.Time) *unpricedRig {
	t.Helper()
	var buf bytes.Buffer
	marks := mark.New(now, unpricedMaxAge)
	deps := gateDeps()
	deps.Mandates = governedMandates{}
	deps.Marks = marks
	reg := prometheus.NewRegistry()
	cfg := config.Config{
		Tenant:        "acme",
		PriceMaxAge:   unpricedMaxAge,
		PriceSubjects: mark.DefaultSubjects,
	}
	// No producer, so the decision recorder is the SYNCHRONOUS slog arm: every
	// line below was written by the goroutine this test runs on, and there is no
	// drain to wait for before reading the buffer.
	w, err := buildPreTradeGate(cfg, deps, nil,
		reg, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if err != nil {
		t.Fatalf("buildPreTradeGate: %v", err)
	}
	t.Cleanup(w.CloseRecorder)
	return &unpricedRig{gate: w.Gate, marks: marks, reg: reg, log: &buf}
}

// refuse submits one MARKET order (no price) and asserts the gate refused it as
// Unpriced — the branch that reaches the observer.
func (r *unpricedRig) refuse(t *testing.T, instrumentID string) {
	t.Helper()
	got, err := r.gate.Evaluate(context.Background(), comp.OrderDelta{
		TenantID: "acme", PortfolioID: "PF1", InstrumentID: instrumentID,
		SignedQuantity: &commonpb.Decimal{Coefficient: 10}, Price: nil,
		Currency: "USD", OrderID: "o-" + instrumentID, AsOf: unpricedBase,
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got.Allowed || !got.Unpriced {
		t.Fatalf("a MARKET order on a governed portfolio must be refused as Unpriced, got "+
			"Allowed=%v Unpriced=%v — the observer under test was never reached",
			got.Allowed, got.Unpriced)
	}
}

// unpricedCounts reads kanz_compliance_unpriced_orders_total by reason label.
func (r *unpricedRig) unpricedCounts(t *testing.T) map[string]float64 {
	t.Helper()
	families, err := r.reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]float64{}
	for _, f := range families {
		if f.GetName() != "kanz_compliance_unpriced_orders_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "reason" {
					out[l.GetValue()] = m.GetCounter().GetValue()
				}
			}
		}
	}
	return out
}

// foldTrade puts a mark on the fold at `at`, the way the price spine would.
func (r *unpricedRig) foldTrade(t *testing.T, instrumentID string, at time.Time) {
	t.Helper()
	payload, err := proto.Marshal(&marketpb.MarketDataEvent{
		InstrumentId: instrumentID,
		EventTime:    timestamppb.New(at),
		Data: &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{
			Price: &commonpb.Decimal{Coefficient: 100},
		}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	env := &envelopepb.Envelope{EventTime: timestamppb.New(at), EventType: "market.crypto.trade"}
	if err := r.marks.Handle(context.Background(), env, payload); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

// The two operator-facing sentences the observer chooses between, and the gate's
// own line beside them. Matched on the fragment that carries the DIAGNOSIS, so a
// reworded message stays matched and a collapsed one does not.
const (
	sayStale     = "the reference mark is STALE"
	sayNeverSeen = "NO reference mark has ever been seen"
	sayGate      = "no usable price"
)

// THE COUNTER IS A RATE AND THE LOG IS A CONDITION.
//
// Before #883 this seam logged on every refused order while internal/compliance's
// gate — one layer in, about the same event — logged once per pair. An operator
// on a stalled feed got one line from one and N from the other, and the line that
// announced the condition was buried by the ones repeating it.
func TestTheUnpricedObserverCountsEveryOrderAndWarnsOncePerPair(t *testing.T) {
	rig := newUnpricedRig(t, func() time.Time { return unpricedBase })

	const orders = 4
	for i := 0; i < orders; i++ {
		rig.refuse(t, "NO-QUOTES")
	}

	if got := rig.unpricedCounts(t)["never_seen"]; got != orders {
		t.Errorf("kanz_compliance_unpriced_orders_total{reason=\"never_seen\"} = %v after %d refused "+
			"orders, want %d. A counter behind a say-once gate is not a rate — it counts the "+
			"instruments first seen unpriced, under a name that reads as the refusal rate",
			got, orders, orders)
	}

	out := rig.log.String()
	if got := strings.Count(out, sayNeverSeen); got != 1 {
		t.Errorf("the observer wrote %d WARN lines for %d orders on ONE (portfolio, instrument) "+
			"pair, want 1 — the gate beside it writes one, and two dedup policies for one event is "+
			"the defect #883 filed", got, orders)
	}
	if got := strings.Count(out, sayGate); got != 1 {
		t.Errorf("the gate wrote %d WARN lines, want 1 — its say-once ledger is what the observer's "+
			"verdict now comes from, so a change here changes both", got)
	}
}

// THE WARM-UP AND THE OUTAGE STAY APART.
//
// A mark NEVER seen is a cold pod, a thin instrument, or a subscription
// delivering nothing. A mark seen and EXPIRED is a feed that was working and
// stalled. #96's tombstone design exists to keep those two answerable — plain
// deletion would have reported every stalled feed as a cold instrument — and
// deduping the log must not be the thing that collapses them.
func TestTheUnpricedObserverTellsAWarmUpFromAnOutage(t *testing.T) {
	now := unpricedBase
	rig := newUnpricedRig(t, func() time.Time { return now })

	// A feed that worked and stopped: mark STALLED, then let it age out. The
	// second fold drives the fold's amortised sweep, which tombstones the first.
	rig.foldTrade(t, "STALLED", unpricedBase)
	now = unpricedBase.Add(time.Hour)
	rig.foldTrade(t, "FRESH", now)

	rig.refuse(t, "STALLED")
	rig.refuse(t, "NEVER-QUOTED")

	counts := rig.unpricedCounts(t)
	if counts["expired"] != 1 {
		t.Errorf("reason=\"expired\" = %v, want 1 — a stalled price feed is being counted as "+
			"something else, and an OUTAGE is now indistinguishable from a warm-up on the dashboard",
			counts["expired"])
	}
	if counts["never_seen"] != 1 {
		t.Errorf("reason=\"never_seen\" = %v, want 1 — an instrument nothing has ever quoted is "+
			"being counted as something else", counts["never_seen"])
	}

	out := rig.log.String()
	if !strings.Contains(out, sayStale) {
		t.Errorf("the stalled feed produced no STALE line: an operator is told to go and look for a "+
			"cold pod while the price feed is down. Log was:\n%s", out)
	}
	if !strings.Contains(out, sayNeverSeen) {
		t.Errorf("the never-quoted instrument produced no never-seen line: an operator is sent to "+
			"chase a feed outage that is not happening. Log was:\n%s", out)
	}
	if strings.Count(out, sayStale) != 1 || strings.Count(out, sayNeverSeen) != 1 {
		t.Errorf("the two diagnoses did not appear exactly once each — they are being written for "+
			"the same refusal:\n%s", out)
	}
}

// THE DEDUP IS PER PAIR AND NOT PER PROCESS. A second unpriced instrument must
// still be announced; a ledger that said "already warned about unpriced orders"
// would hide the SECOND feed going down during the first one's incident.
func TestASecondUnpricedInstrumentIsStillAnnounced(t *testing.T) {
	rig := newUnpricedRig(t, func() time.Time { return unpricedBase })

	rig.refuse(t, "NO-QUOTES-A")
	rig.refuse(t, "NO-QUOTES-A")
	rig.refuse(t, "NO-QUOTES-B")

	out := rig.log.String()
	if got := strings.Count(out, sayNeverSeen); got != 2 {
		t.Errorf("%d never-seen lines for 2 distinct unpriced instruments, want 2 — a per-process "+
			"say-once would swallow every instrument after the first, so the second feed to go down "+
			"during an incident is the one nobody is told about", got)
	}
	if got := rig.unpricedCounts(t)["never_seen"]; got != 3 {
		t.Errorf("reason=\"never_seen\" = %v after 3 refused orders, want 3", got)
	}
	if !strings.Contains(out, "NO-QUOTES-B") {
		t.Errorf("the second instrument is not named anywhere in the log:\n%s", out)
	}
}
