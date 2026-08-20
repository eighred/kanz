package main

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// postureLogs captures records so the posture can be asserted on its LEVEL, not
// merely on the presence of text. The defect class this file guards is not a
// missing line; it is a reassuring one — #539 shipped an INFO reading "override
// surface fronted" for a gateway that mounted no override routes.
type postureLogs struct {
	recs []slog.Record
}

func (h *postureLogs) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *postureLogs) Handle(_ context.Context, r slog.Record) error {
	h.recs = append(h.recs, r.Clone())
	return nil
}
func (h *postureLogs) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *postureLogs) WithGroup(string) slog.Handler      { return h }

func (h *postureLogs) logger() *slog.Logger { return slog.New(h) }

// all renders every captured record, message and attributes, for failure output.
func (h *postureLogs) all() string {
	var b bytes.Buffer
	for _, r := range h.recs {
		b.WriteString(r.Level.String())
		b.WriteString(" ")
		b.WriteString(r.Message)
		r.Attrs(func(a slog.Attr) bool {
			b.WriteString(" ")
			b.WriteString(a.Key)
			b.WriteString("=")
			b.WriteString(a.Value.String())
			return true
		})
		b.WriteString("\n")
	}
	return b.String()
}

// atLevel returns the rendered records logged at lvl.
func (h *postureLogs) atLevel(lvl slog.Level) string {
	var b bytes.Buffer
	for _, r := range h.recs {
		if r.Level != lvl {
			continue
		}
		b.WriteString(r.Message)
		r.Attrs(func(a slog.Attr) bool {
			b.WriteString(" ")
			b.WriteString(a.Key)
			b.WriteString("=")
			b.WriteString(a.Value.String())
			return true
		})
		b.WriteString("\n")
	}
	return b.String()
}

// stageGauge reads kanz_oms_settlement_plane_running for one stage.
func stageGauge(t *testing.T, reg *prometheus.Registry, stage string) (float64, bool) {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() != settlementPlaneMetric {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "stage" && l.GetValue() == stage {
					return m.GetGauge().GetValue(), true
				}
			}
		}
	}
	return 0, false
}

// EVERY STAGE GETS A SERIES, INCLUDING — TODAY, ESPECIALLY — THE ZEROES.
//
// An absent series and a zero series mean different things to an alert: absent is
// "no data", which most rules treat as not-firing, and that is the empty
// SETTLEMENT stream's ambiguity moved from the broker into the monitoring system.
// This asserts the gauge is registered, that it carries exactly the stages the
// table declares, and that a deployment wiring nothing reports zero for all of
// them rather than reporting nothing.
func TestSettlementPlanePostureSeedsEveryStageAtZero(t *testing.T) {
	reg := prometheus.NewRegistry()
	logs := &postureLogs{}
	settlementPlanePosture(reg, logs.logger(), nil)

	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var found *dto.MetricFamily
	for _, f := range fams {
		if f.GetName() == settlementPlaneMetric {
			found = f
		}
	}
	if found == nil {
		t.Fatalf("%s was never registered — an absent posture metric reads as 'no data', which is "+
			"exactly how the empty SETTLEMENT stream already reads (#589)", settlementPlaneMetric)
	}
	if found.GetType() != dto.MetricType_GAUGE {
		t.Errorf("%s is a %v, want GAUGE — a counter would answer 'how many fails', which is the "+
			"question that cannot be answered here", settlementPlaneMetric, found.GetType())
	}

	var got []string
	for _, m := range found.GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == "stage" {
				got = append(got, l.GetValue())
			}
		}
		if m.GetGauge().GetValue() != 0 {
			t.Errorf("a stage reports %v with nothing wired, want 0", m.GetGauge().GetValue())
		}
	}
	var want []string
	for _, s := range settlementStages {
		want = append(want, s.stage)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("series stages = %v, want one per declared stage %v — a stage in the table with no "+
			"seeded series is a stage no alert can ever fire on", got, want)
	}
}

// THE TABLE MUST COVER THE PACKAGE, AND THE PACKAGE IS THE SOURCE OF TRUTH.
//
// settlementStages is hand-written, and a hand-enumerated list is the trap this
// repository has already shipped once: the seeding loops in
// services/risk-engine/cmd/risk-engine/liquidity.go enumerate every skip reason
// by name, so a NEW reason added to liquiditysource ships with no series at all —
// invisible in exactly the way the seeding exists to prevent.
//
// So the coverage is derived rather than trusted. This parses
// services/oms/internal/posttrade and requires that every exported top-level
// function there is claimed by exactly one stage, and that every name the table
// claims actually exists. Adding a capability to the post-trade plane and not
// stating its posture fails here; so does a stage entry whose entry point was
// renamed or deleted.
//
// It parses the AST rather than grepping, on the standing finding that a guard
// matching raw source matches its own comments and nearby identifiers.
func TestSettlementPlanePostureCoversEveryPosttradeEntrypoint(t *testing.T) {
	const pkgDir = "../../internal/posttrade"

	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		t.Fatalf("read %s: %v", pkgDir, err)
	}
	fset := token.NewFileSet()
	inPackage := map[string]bool{}
	var parsed int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(pkgDir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		if f.Name.Name != "posttrade" {
			t.Fatalf("%s declares package %q — the posture names a package that is not there, so "+
				"nothing it claims can be trusted", name, f.Name.Name)
		}
		parsed++
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			// Top-level functions only: a method is reached through the value its
			// constructor returned, so the constructor is the entry point.
			if !ok || fn.Recv != nil || !fn.Name.IsExported() {
				continue
			}
			inPackage[fn.Name.Name] = true
		}
	}
	if parsed == 0 {
		t.Fatalf("no production .go files under %s — the guard would pass by reading nothing", pkgDir)
	}
	if len(inPackage) == 0 {
		t.Fatalf("no exported top-level functions parsed out of %s — the guard would pass "+
			"vacuously, which is the failure mode it is written against", pkgDir)
	}

	claimed := map[string]string{}
	for _, s := range settlementStages {
		if s.stage == "" {
			t.Error("a stage has an empty name — it is the metric label an alert is written against")
		}
		if strings.TrimSpace(s.armedBy) == "" {
			t.Errorf("stage %q states no armedBy — a posture that does not say what would arm it is "+
				"a TODO with a metric attached", s.stage)
		}
		if len(s.entrypoints) == 0 {
			t.Errorf("stage %q names no entry points, so nothing ties it to code that exists", s.stage)
		}
		for _, name := range s.entrypoints {
			if prev, dup := claimed[name]; dup {
				t.Errorf("%s is claimed by both %q and %q — one function, two postures, so the "+
					"gauge could report it running and not running at once", name, prev, s.stage)
			}
			claimed[name] = s.stage
		}
	}

	for name := range inPackage {
		if _, ok := claimed[name]; !ok {
			t.Errorf("posttrade.%s is an exported entry point that NO stage claims.\n\n"+
				"Every capability of the post-trade plane must state whether it is running: an "+
				"unclaimed one ships with no series on %s, so an operator querying the posture gets "+
				"a confident answer that silently excludes it — the same defect as the empty "+
				"SETTLEMENT stream, one layer up (#589).\n\n"+
				"Add it to a stage in settlementStages, or add a stage.", name, settlementPlaneMetric)
		}
	}
	for name, stage := range claimed {
		if !inPackage[name] {
			t.Errorf("stage %q claims posttrade.%s, which does not exist in the package.\n\n"+
				"A posture reporting on a function that was renamed or deleted vouches for nothing, "+
				"and it does it at full confidence.", stage, name)
		}
	}
}

// THE POSTURE MUST BE SAYABLE OUT LOUD, AT THE LEVEL THAT SURVIVES FILTERING.
//
// The startup log is the half an operator reads before they draw a conclusion
// from the stream, and it is only useful if it names the four things that make
// the conclusion wrong: the stream, the subject, the producer that does not
// exist, and the fact that empty means unmonitored rather than clean.
func TestSettlementPlanePostureWarnsThatFailDetectionIsNotRunning(t *testing.T) {
	logs := &postureLogs{}
	settlementPlanePosture(prometheus.NewRegistry(), logs.logger(), nil)

	warn := logs.atLevel(slog.LevelWarn)
	if warn == "" {
		t.Fatalf("nothing was logged at WARN.\n\nAn operator has no other signal: the SETTLEMENT "+
			"stream exists, the publish grant exists, and the stream is empty.\n\nlog was:\n%s",
			logs.all())
	}
	for _, want := range []string{
		"SETTLEMENT",
		"settlement.instruction.fail",
		"BusFailSink",
		"NOT RUNNING ON THIS DEPLOYMENT",
		"fail_detection",
		"fail_publication",
	} {
		if !strings.Contains(warn, want) {
			t.Errorf("the WARN does not name %q.\n\nlog was:\n%s", want, warn)
		}
	}
	// Every stage's armedBy must reach the log, or "what would arm it" is a
	// comment rather than something the running process tells you.
	for _, s := range settlementStages {
		if !strings.Contains(warn, s.armedBy) {
			t.Errorf("stage %q logs no armed_by reason — the posture says what is off and not what "+
				"turns it on.\n\nlog was:\n%s", s.stage, warn)
		}
	}
	if info := logs.atLevel(slog.LevelInfo); info != "" {
		t.Errorf("something was logged at INFO while the whole plane is dark — an INFO here is the "+
			"#539 shape, a reassuring line for a control that is not there.\n\nINFO was:\n%s", info)
	}
}

// AND A WIRED STAGE MUST REPORT AS WIRED. Without this the guard above is
// satisfied by warning unconditionally, which is how a real warning stops being
// read — and the 1 branch would be untested code that first runs on the day
// somebody wires the plane.
func TestSettlementPlanePostureReportsAWiredStage(t *testing.T) {
	reg := prometheus.NewRegistry()
	logs := &postureLogs{}
	settlementPlanePosture(reg, logs.logger(), map[string]bool{"fail_publication": true})

	if v, ok := stageGauge(t, reg, "fail_publication"); !ok || v != 1 {
		t.Errorf("fail_publication = %v (present=%v), want 1", v, ok)
	}
	if v, ok := stageGauge(t, reg, "fail_detection"); !ok || v != 0 {
		t.Errorf("fail_detection = %v (present=%v), want 0 — the stages that stay dark must stay "+
			"visible when a sibling is wired", v, ok)
	}
	warn := logs.atLevel(slog.LevelWarn)
	if !strings.Contains(warn, "fail_detection") {
		t.Errorf("a partly wired plane stopped naming the stages that are still dark.\n\nlog:\n%s", warn)
	}
	if strings.Contains(warn, "armed_by=[fail_publication") {
		t.Errorf("a wired stage is still being reported as needing arming.\n\nlog:\n%s", warn)
	}
}

// A FULLY WIRED PLANE MUST NOT WARN. The day the confirmation feed exists, this
// is what stops the posture from being a permanent WARN nobody reads.
func TestSettlementPlanePostureIsQuietWhenEverythingRuns(t *testing.T) {
	reg := prometheus.NewRegistry()
	logs := &postureLogs{}
	running := map[string]bool{}
	for _, s := range settlementStages {
		running[s.stage] = true
	}
	settlementPlanePosture(reg, logs.logger(), running)

	if warn := logs.atLevel(slog.LevelWarn); warn != "" {
		t.Errorf("a fully wired plane still WARNs:\n%s", warn)
	}
	if info := logs.atLevel(slog.LevelInfo); !strings.Contains(info, "every implemented post-trade stage") {
		t.Errorf("a fully wired plane says nothing at INFO either — 'running' and 'never reported' "+
			"must not look the same.\n\nlog:\n%s", logs.all())
	}
	for _, s := range settlementStages {
		if v, ok := stageGauge(t, reg, s.stage); !ok || v != 1 {
			t.Errorf("%s = %v (present=%v), want 1", s.stage, v, ok)
		}
	}
}
