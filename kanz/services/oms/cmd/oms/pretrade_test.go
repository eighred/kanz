package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/marketdata/mark"
	"github.com/eighred/kanz/internal/refdata"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	"github.com/eighred/kanz/services/oms/internal/config"
)

// THE TEST #643 SAYS IS IMPOSSIBLE TO WRITE.
//
// Its "Verified when" asks for exactly this: a test that builds the pre-trade
// gate through a named builder and asserts the gate it returns carries a
// classifier and a decision recorder. While those fifty constructions were
// locals inside a 1,400-line runConsumers there was nothing to call and nothing
// to inspect — the existing cmd/oms tests work around that by PARSING main.go as
// source text, which is a symptom rather than a technique.
//
// What it grades is the composition, not the gate's arithmetic (that is
// internal/compliance's own suite): which seams a real startup actually wires,
// and which of them may legally be absent.

// --- fixtures -------------------------------------------------------------

type stubBooks struct{}

func (stubBooks) Book(context.Context, string) (*comp.Book, error) { return &comp.Book{}, nil }

type stubMandates struct{}

func (stubMandates) Mandate(context.Context, string, string, time.Time) (*compliancepb.Mandate, bool, error) {
	return nil, false, nil
}

type stubMargin struct{}

func (stubMargin) Margin(string, string, string) (comp.MarginState, bool) {
	return comp.MarginState{}, false
}

func gateDeps() preTradeDeps {
	return preTradeDeps{
		Books:    stubBooks{},
		Mandates: stubMandates{},
		Margin:   stubMargin{},
		Marks:    mark.New(time.Now, time.Minute),
	}
}

func gateLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// masterConfigured is a config naming a security master, which is what makes the
// classifier resolvable. The URL is never dialled here: refdata.Cache reads from
// memory and is filled by a refresh cycle the composition root starts.
func masterConfigured() config.Config {
	return config.Config{
		Tenant:  "acme",
		RefData: refdata.Config{DatamasterURL: "http://datamaster.invalid"},
	}
}

// --- the seams that must never be nil -------------------------------------

// EVERY CONTROL THE GATE CLAIMS IS ACTUALLY WIRED.
//
// Two of these were bare nils on one line in the composition root. The
// classifier (#640) made every SECTOR and ISSUER mandate limit pass silently for
// months; the recorder meant the enforcement point deciding whether capital MOVES
// recorded nothing anywhere, while internal/compliance's type doc claimed the
// audit trail was complete.
func TestTheBuiltGateCarriesEveryControlItClaims(t *testing.T) {
	w, err := buildPreTradeGate(masterConfigured(), gateDeps(), nil, prometheus.NewRegistry(), gateLogger())
	if err != nil {
		t.Fatalf("buildPreTradeGate: %v", err)
	}
	p := w.Gate.Posture()
	for _, seam := range []struct {
		name string
		held bool
		cost string
	}{
		{"books", p.Books, "the gate cannot evaluate a single rule and admits"},
		{"mandates", p.Mandates, "no portfolio resolves a mandate, so every order is UNGOVERNED"},
		{"classifier", p.Classifier, "every SECTOR and ISSUER limit passes unverified (#640)"},
		{"recorder", p.Recorder, "no pre-trade decision is recorded anywhere, so the platform " +
			"cannot say an order was CHECKED"},
		{"margin", p.Margin, "a mandate declaring margin trading cannot be gated at all (#408)"},
	} {
		if !seam.held {
			t.Errorf("the pre-trade gate was built with no %s: %s", seam.name, seam.cost)
		}
	}
	if !p.Complete() {
		t.Errorf("GatePosture.Complete() = false on a fully configured build: %+v", p)
	}
}

// THE CLASSIFIER IS THE ONE SEAM THAT MAY LEGALLY BE ABSENT, and only because
// no security master is configured. It must be absent for THAT reason and no
// other — a build with a master that silently produced no classifier is #640
// returning.
func TestWithNoSecurityMasterOnlyTheClassifierIsAbsent(t *testing.T) {
	w, err := buildPreTradeGate(config.Config{Tenant: "acme"}, gateDeps(), nil, prometheus.NewRegistry(), gateLogger())
	if err != nil {
		t.Fatalf("buildPreTradeGate: %v", err)
	}
	p := w.Gate.Posture()
	if p.Classifier {
		t.Error("a classifier was wired with no OMS_DATAMASTER_URL — it can resolve nothing, and " +
			"a seam that answers 'unknown instrument' is not the same as one that is absent")
	}
	if w.Classifier != nil {
		t.Error("a reference cache was returned with no master configured")
	}
	if !p.Recorder || !p.Books || !p.Mandates || !p.Margin {
		t.Errorf("a missing security master took another seam down with it: %+v — the four below "+
			"do not depend on reference data and must be wired on every deployment", p)
	}
	if p.Complete() {
		t.Error("GatePosture.Complete() = true with no classifier — an incomplete gate that reports " +
			"itself complete is worse than the missing seam")
	}
}

// THE POSTURE IS EXPORTED, NOT JUST ASSERTED HERE. A test that reads the seams
// and a dashboard that cannot is half a control: #640 ran for months because
// every rule returned "pass" and no series anywhere distinguished that from a
// book with no sector limits in it.
func TestTheGatePostureIsOnAGaugeIncludingTheZeroes(t *testing.T) {
	reg := prometheus.NewRegistry()
	if _, err := buildPreTradeGate(config.Config{Tenant: "acme"}, gateDeps(), nil, reg, gateLogger()); err != nil {
		t.Fatalf("buildPreTradeGate: %v", err)
	}
	got := seamGauge(t, reg)
	if len(got) != 5 {
		t.Fatalf("kanz_compliance_pretrade_seam_wired has %d series %v, want one per seam", len(got), got)
	}
	if got["classifier"] != 0 {
		t.Errorf("classifier gauge = %v with no master configured, want 0", got["classifier"])
	}
	// THE ZERO IS THE WHOLE POINT. A seam that goes ABSENT from the metric rather
	// than reading 0 is no-data to an alert, and an alert on no-data cannot fire.
	for _, seam := range []string{"books", "mandates", "recorder", "margin"} {
		if got[seam] != 1 {
			t.Errorf("%s gauge = %v, want 1", seam, got[seam])
		}
	}
}

// A REFERENCE-DATA CONFIGURATION THIS POD CANNOT ACCEPT REFUSES THE START. The
// alternative is a pod that admits orders while every classified-dimension limit
// passes, which is the defect this gate exists to prevent.
func TestAnUnusableSecurityMasterRefusesToStart(t *testing.T) {
	cfg := config.Config{Tenant: "acme", RefData: refdata.Config{DatamasterURL: "://not-a-url"}}
	w, err := buildPreTradeGate(cfg, gateDeps(), nil, prometheus.NewRegistry(), gateLogger())
	if err == nil {
		t.Fatal("buildPreTradeGate accepted a security master it cannot read — the pod would admit " +
			"orders with every sector and issuer limit passing unverified")
	}
	if w.Gate != nil {
		t.Error("a gate was returned alongside the refusal")
	}
}

// THE REFRESH COUNTER EXISTS EVEN WHERE NOTHING REFRESHES. Created at the loop
// it would be absent on a deployment with no master, and "the refresh is
// failing" would read the same as "there is no refresh".
func TestTheRefreshFailureCounterIsRegisteredWithNoMaster(t *testing.T) {
	reg := prometheus.NewRegistry()
	w, err := buildPreTradeGate(config.Config{Tenant: "acme"}, gateDeps(), nil, reg, gateLogger())
	if err != nil {
		t.Fatalf("buildPreTradeGate: %v", err)
	}
	if w.RefreshFailures == nil {
		t.Fatal("no refresh-failure counter was returned")
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var found bool
	for _, f := range families {
		if f.GetName() == "kanz_instrument_reference_refresh_failures_total" {
			found = true
		}
	}
	if !found {
		t.Error("kanz_instrument_reference_refresh_failures_total is absent on a build with no " +
			"security master — a missing series reads as no-data, not as zero failures")
	}
}

// --- the recorder is a real sink ------------------------------------------

// THE RECORDER WRITES SOMETHING. Asserting the seam is non-nil would pass for a
// recorder that discards, which is the state this replaced — so the sink is
// exercised and its output read.
func TestTheDecisionRecorderActuallyRecords(t *testing.T) {
	var out strings.Builder
	rec := comp.NewSlogRecorder(slog.New(slog.NewTextHandler(&out, nil)))
	err := rec.Record(context.Background(), comp.DecisionRecord{
		Phase:   comp.PhasePreTrade,
		Allowed: true,
		OrderID: "ORD-1",
		Issuer:  "operator:akif",
		Result: &compliancepb.ComplianceResult{
			PortfolioId: "fund-alpha", MandateId: "M-1", MandateVersion: 3,
		},
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	line := out.String()
	for _, want := range []string{"compliance decision", "pre_trade", "ORD-1", "fund-alpha", "allowed=true"} {
		if !strings.Contains(line, want) {
			t.Errorf("the recorded decision does not mention %q: %s", want, line)
		}
	}
}

// AN ADMISSION IS RECORDED, NOT ONLY A REFUSAL. A recorder that logged rejections
// alone answers "why was this order stopped" and not "why was this one allowed",
// and the second is the question asked about the trade that lost the money.
func TestAnAllowedDecisionIsRecordedToo(t *testing.T) {
	var out strings.Builder
	rec := comp.NewSlogRecorder(slog.New(slog.NewTextHandler(&out, nil)))
	for _, allowed := range []bool{true, false} {
		if err := rec.Record(context.Background(), comp.DecisionRecord{
			Phase: comp.PhasePreTrade, Allowed: allowed,
			Result: &compliancepb.ComplianceResult{PortfolioId: "fund-alpha"},
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if got := strings.Count(out.String(), "compliance decision"); got != 2 {
		t.Errorf("recorded %d decisions, want 2 — one of the two verdicts is not being written", got)
	}
}

// --- helpers --------------------------------------------------------------

func seamGauge(t *testing.T, g prometheus.Gatherer) map[string]float64 {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]float64{}
	for _, f := range families {
		if f.GetName() != "kanz_compliance_pretrade_seam_wired" {
			continue
		}
		for _, m := range f.GetMetric() {
			out[seamLabel(m)] = m.GetGauge().GetValue()
		}
	}
	return out
}

func seamLabel(m *dto.Metric) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == "seam" {
			return l.GetValue()
		}
	}
	return ""
}
