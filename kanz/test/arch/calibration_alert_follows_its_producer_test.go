package arch

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// THE STRIP-COVERAGE ALERT EXISTS IF AND ONLY IF SOME POD PRODUCES ITS SERIES (#912).
//
// # The two failures this sits between, which are the same failure
//
// #62 deleted ten data-quality alert rules because each one queried a series no
// running pod exported. A rule over an empty vector never fires, so the estate
// read as alerted and was not. infra/observability/alerts/operational.rules.yaml
// names that as the reason it withholds a rule over kanz_bus_consumer_lag, and
// alerts/README.md turns it into an order: INSTRUMENT FIRST, then write the rule.
//
// The mirror image costs the same. A rule whose series exists but is pinned in
// its alerting state — quoted == 0 out of configured == N, from the first refresh
// and forever, because the instruments named in RISK_ENGINE_CALIBRATION_RATES are
// not quoted by anything — fires on its first evaluation and never clears. That
// is the `absent()` failure the same file rejects one paragraph up: an alert
// nobody can ever resolve trains its readers to scroll past it, and takes the
// rest of the file's credibility with it.
//
// So the ordering has THREE steps, not two: a producer of rate quotes, then a
// deployment that calibrates against instruments that producer quotes, then the
// rule. #911 landed the instrumentation; #912 established that step one does not
// exist in this estate — the whole market spine is
// MARKET_INGEST_INSTRUMENTS="BTC-USDT,ETH-USDT", market-data's feed is unset in
// every manifest, and no deposit, rate future or par swap is quoted anywhere.
//
// # Why this is a guard and not another paragraph
//
// It already went stale once, and that is the whole argument. The "deliberately
// absent" note in operational.rules.yaml said the rule was unblocked by "a
// deployment that enables calibration" — one env-var edit, by its own reading —
// and calibration_posture.go said "ONE of them is scheduled … Two thirds of it is
// not", which is true of the code path and was never true of any deployment.
// Both were accurate the day they were written and neither is a thing the build
// checks, so both drifted from the estate they describe. A comment justifying a
// trade-off is dated evidence; this test is the date being re-stamped on every
// run.
//
// # What it derives, and from where
//
// Neither side is listed here:
//
//   - The PRODUCER side is derived from infra/ — every workload's container env
//     and every ConfigMap's data, looking for RISK_ENGINE_CALIBRATION_INTERVAL
//     and RISK_ENGINE_CALIBRATION_RATES. Both are needed: the composition root's
//     gate is an AND, and one alone starts nothing.
//   - The METRIC NAMES are derived from the risk-engine's own metric
//     declarations, so renaming a gauge in strip_coverage.go moves this guard
//     with it instead of leaving it matching a name nothing exports.
//   - The RULE side is derived from the `expr` of every rule in every
//     *.rules.yaml under infra/observability.
//
// Reading `expr` out of the PARSED yaml rather than grepping the file is what
// makes this guard immune to its own subject matter: the long comment above the
// rules — which names all three gauges, at length — is a yaml comment, so the
// decoder never sees it. A guard that grepped raw source would match that prose
// and pass with no rule present at all.
//
// # What it does NOT check
//
// That the instruments a future RISK_ENGINE_CALIBRATION_RATES names are actually
// quoted. No artifact in this repository can answer that — it depends on what a
// vendor feed delivers (#104 vendor Source bindings, #105 licensed rate data,
// both blocked-external). What this guard can do is make the day somebody sets
// those variables the day they must also confront the alert, and the paragraph
// in operational.rules.yaml tells them what to confront.
func TestTheStripCoverageAlertFollowsItsProducer(t *testing.T) {
	root := moduleRoot(t)

	metrics := stripCoverageMetricNames(t, root)
	// NON-VACUITY (1/4): the producer must exist in the binary. If #911's gauges
	// were renamed or deleted, every match below would be against nothing and this
	// guard would pass by finding no rule for a metric that no longer exists.
	if len(metrics) < 2 {
		t.Fatalf("found %d kanz_risk_calibration_strip_* metric declarations in the risk-engine "+
			"(want at least 2: _configured and _quoted). Either the strip-coverage instrumentation "+
			"from #911 was removed — in which case this guard and the 'deliberately absent' note in "+
			"operational.rules.yaml are both about a metric family that no longer exists — or the "+
			"declarations moved and this scanner no longer sees them. Either way it is asserting "+
			"nothing", len(metrics))
	}

	enablers, workloads := calibrationEnablingWorkloads(t, root)
	// NON-VACUITY (2/4): the manifest walk has to be finding workloads at all.
	if len(workloads) < 10 {
		t.Fatalf("decoded only %d workload documents under infra/ — the estate ships far more, so "+
			"the walk is broken and 'no manifest enables calibration' would be a vacuous pass "+
			"rather than a measurement", len(workloads))
	}
	// NON-VACUITY (3/4): and specifically the one workload that could enable it.
	if !workloads["risk-engine"] {
		t.Fatal("no workload labelled app=risk-engine was found under infra/ — this guard reads " +
			"that manifest's env to decide whether calibration is enabled anywhere, so without it " +
			"the producer side is always empty and the check is vacuous")
	}

	ruleRefs, exprs := rulesReferencingMetrics(t, root, metrics)
	// NON-VACUITY (4/4): the rule files must have parsed into real expressions.
	if exprs < 20 {
		t.Fatalf("parsed only %d rule expressions under infra/observability — the estate ships "+
			"dozens, so the decoder is not seeing the rules and 'no rule references the gauges' "+
			"would be a vacuous pass", exprs)
	}

	switch {
	case len(enablers) > 0 && len(ruleRefs) == 0:
		t.Errorf("calibration is ENABLED by %s, so a running pod now exports %s — and no rule "+
			"under infra/observability reads them.\n"+
			"The strip-coverage gauges exist to make a curve calibrated from a short strip visible "+
			"(#908/#911); a gauge nobody alerts on is visible only to whoever thinks to query it at "+
			"3am. Write the rule — quoted < configured needs no invented threshold, both sides are "+
			"the pod's own numbers — with a `for:` derived from that deployment's own "+
			"RISK_ENGINE_CALIBRATION_INTERVAL, a promtool firing case in operational_test.yaml, and "+
			"the must-NOT-fire case for a complete strip.\n"+
			"BEFORE YOU DO: confirm the spine actually quotes the instruments that "+
			"RISK_ENGINE_CALIBRATION_RATES names. If it does not, every one of them reports "+
			"MissingNoQuote from the first refresh, the rule fires forever and never clears, and "+
			"the seven FI and structured measures behind the same gate go on the wire flagged "+
			"unmeasured on every response (#912; #104, #105).\n"+
			"Then update the 'ALSO DELIBERATELY ABSENT' note in "+
			"infra/observability/alerts/operational.rules.yaml and the posture comment in "+
			"services/risk-engine/internal/app/calibration_posture.go — both currently state that "+
			"NOTHING enables calibration, which your change makes false.",
			strings.Join(enablers, ", "), strings.Join(metrics, ", "))

	case len(enablers) == 0 && len(ruleRefs) > 0:
		t.Errorf("a rule reads %s and NO workload under infra/ sets both "+
			"RISK_ENGINE_CALIBRATION_INTERVAL and RISK_ENGINE_CALIBRATION_RATES, so no running pod "+
			"exports the series.\n"+
			"rules: %s\n"+
			"The rule evaluates an empty vector on every scrape and can never fire — #62's defect, "+
			"which cost this repository ten deleted data-quality rules, reproduced with a new "+
			"metric name. alerts/README.md states the order: INSTRUMENT FIRST, then write the rule. "+
			"The instrumenting landed in #911; the producer has not started anywhere (#912).",
			strings.Join(metrics, ", "), strings.Join(ruleRefs, ", "))
	}
}

// TestTheDeliberatelyAbsentNoteNamesThisGuard pins the other half of the
// arrangement: the paragraph a human reads must point at the check a machine
// runs, or the next person to size this gap sizes it from prose again — which is
// exactly how the note came to claim the rule was one env-var edit away.
func TestTheDeliberatelyAbsentNoteNamesThisGuard(t *testing.T) {
	p := filepath.Join(moduleRoot(t), "infra", "observability", "alerts", "operational.rules.yaml")
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	const self = "calibration_alert_follows_its_producer_test.go"
	if !strings.Contains(string(body), self) {
		t.Errorf("operational.rules.yaml withholds the strip-coverage rule and does not name %s, "+
			"the guard that decides when it may land. A withheld rule whose condition is only prose "+
			"goes stale silently — this one already did", self)
	}
}

// TestEveryCalibrationKindHasAnUnscheduledReason keeps the risk-engine's posture
// reasons derived from its kind list rather than parallel to it. A fourth
// calibrator added to CalibrationKinds without a reason logs "NO REASON RECORDED"
// at runtime, which is honest but late; this fails the build instead.
//
// The two sides are read out of the source with the AST-free scanner below
// because test/arch does not import the service packages — the point is that
// nobody has to remember to update a list here when they add a kind there.
func TestEveryCalibrationKindHasAnUnscheduledReason(t *testing.T) {
	p := filepath.Join(moduleRoot(t), "services", "risk-engine", "internal", "app",
		"calibration_posture.go")
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	src := string(body)

	kinds := betweenLiteralStrings(src, "var CalibrationKinds = []string{", "}")
	if len(kinds) < 3 {
		t.Fatalf("read %d calibration kinds out of %s (want at least 3: curve, volsurface, "+
			"credit) — the declaration moved and this check is asserting nothing", len(kinds), p)
	}
	reasons := reasonMapKeys(src)
	if len(reasons) == 0 {
		t.Fatalf("read no keys out of calibrationUnscheduledReason in %s — the declaration moved "+
			"and this check is asserting nothing", p)
	}

	for _, k := range kinds {
		if !reasons[k] {
			t.Errorf("calibration kind %q has no entry in calibrationUnscheduledReason. An "+
				"operator told a calibration is idle without being told what blocks it cannot "+
				"act, and cannot tell 'deliberately unconfigured' from 'broken' — which is the "+
				"distinction calibration_posture.go exists to keep", k)
		}
	}
	for k := range reasons {
		if !containsString(kinds, k) {
			t.Errorf("calibrationUnscheduledReason names %q, which is not in CalibrationKinds — "+
				"a reason for a calibration this platform does not implement is never logged, so "+
				"it is a claim nothing checks", k)
		}
	}
}

// stripCoverageMetricNames reads the strip-coverage gauge names out of the
// risk-engine's own declarations, so the guard follows a rename instead of
// matching a name nothing exports. Sorted for a stable failure message.
func stripCoverageMetricNames(t *testing.T, root string) []string {
	t.Helper()
	dir := filepath.Join(root, "services", "risk-engine")
	re := regexp.MustCompile(`"(kanz_risk_calibration_strip_[a-z_]+)"`)
	seen := map[string]bool{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skipWalkDir(d) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, m := range re.FindAllStringSubmatch(string(body), -1) {
			seen[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return sortedKeys(seen)
}

// calibrationEnablingWorkloads reports every manifest source under infra/ that
// sets BOTH calibration variables — the composition root's gate is an AND, and
// one alone starts nothing. It also returns the set of workload names decoded, so
// a broken walk cannot pass as "nothing enables it".
//
// A `valueFrom` entry counts as SET. The value is unknowable from the manifest,
// and the fail-closed reading is that a deployment naming the variable intends to
// supply it — which puts the burden on the rule rather than on this guard's
// guesswork.
func calibrationEnablingWorkloads(t *testing.T, root string) ([]string, map[string]bool) {
	t.Helper()
	var enablers []string
	workloads := map[string]bool{}
	forEachInfraDoc(t, root, func(source string, doc calibrationDoc) {
		if doc.isWorkload {
			workloads[doc.name] = true
		}
		if doc.setsInterval && doc.setsRates {
			enablers = append(enablers, source+" ("+doc.name+")")
		}
	})
	sort.Strings(enablers)
	return enablers, workloads
}

// rulesReferencingMetrics returns "file:group/rule" for every rule whose expr
// names one of the metrics, plus the total number of expressions parsed.
//
// EXPR ONLY, out of the parsed document. Annotations and yaml comments are prose
// about a rule, not the rule; matching them is how a guard comes to check its own
// paragraph — and the paragraph above these rules names all three gauges.
func rulesReferencingMetrics(t *testing.T, root string, metrics []string) ([]string, int) {
	t.Helper()
	dir := filepath.Join(root, "infra", "observability")
	var refs []string
	exprs := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skipWalkDir(d) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".rules.yaml") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		var f struct {
			Groups []struct {
				Name  string `yaml:"name"`
				Rules []struct {
					Alert  string `yaml:"alert"`
					Record string `yaml:"record"`
					Expr   string `yaml:"expr"`
				} `yaml:"rules"`
			} `yaml:"groups"`
		}
		if uerr := yaml.Unmarshal(body, &f); uerr != nil {
			t.Fatalf("parse %s: %v", path, uerr)
		}
		rel, _ := filepath.Rel(root, path)
		for _, g := range f.Groups {
			for _, r := range g.Rules {
				if r.Expr == "" {
					continue
				}
				exprs++
				for _, m := range metrics {
					if strings.Contains(r.Expr, m) {
						name := r.Alert
						if name == "" {
							name = r.Record
						}
						refs = append(refs, filepath.ToSlash(rel)+":"+g.Name+"/"+name)
						break
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	sort.Strings(refs)
	return refs, exprs
}

// calibrationDoc is the slice of an infra document this guard needs.
type calibrationDoc struct {
	name         string
	isWorkload   bool
	setsInterval bool
	setsRates    bool
}

const (
	envCalibrationInterval = "RISK_ENGINE_CALIBRATION_INTERVAL"
	envCalibrationRates    = "RISK_ENGINE_CALIBRATION_RATES"
)

// forEachInfraDoc decodes every yaml document under infra/ and hands the caller
// the calibration-relevant slice of it.
//
// ConfigMaps are read as well as workloads: a value delivered through envFrom is
// the same enablement as one written inline, and only checking container env
// would leave the obvious indirection invisible.
func forEachInfraDoc(t *testing.T, root string, fn func(source string, doc calibrationDoc)) {
	t.Helper()
	dir := filepath.Join(root, "infra")
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skipWalkDir(d) {
			return filepath.SkipDir
		}
		if d.IsDir() || (!strings.HasSuffix(d.Name(), ".yaml") && !strings.HasSuffix(d.Name(), ".yml")) {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, path)
		source := filepath.ToSlash(rel)
		dec := yaml.NewDecoder(strings.NewReader(string(body)))
		for {
			var raw rawCalibrationDoc
			derr := dec.Decode(&raw)
			if errors.Is(derr, io.EOF) {
				break
			}
			if derr != nil {
				// A tree this size holds documents that are not Kubernetes objects
				// (Argo templates, Backstage catalog entries, terraform-adjacent
				// yaml). A document this decoder cannot shape is not a failure of
				// the estate; skipping the FILE rather than the document would be,
				// because the decoder cannot resynchronise mid-stream.
				break
			}
			fn(source, raw.slice())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
}

// rawCalibrationDoc mirrors only the fields inspected. Separate from
// workloadDoc's rawWorkload because that type models SPIFFE env NAMES and CSI
// volumes; this one needs env VALUES and ConfigMap data.
type rawCalibrationDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Data map[string]string `yaml:"data"`
	Spec struct {
		Template struct {
			Metadata struct {
				Labels map[string]string `yaml:"labels"`
			} `yaml:"metadata"`
			Spec struct {
				Containers []struct {
					Env []struct {
						Name      string     `yaml:"name"`
						Value     string     `yaml:"value"`
						ValueFrom *yaml.Node `yaml:"valueFrom"`
					} `yaml:"env"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

func (r rawCalibrationDoc) slice() calibrationDoc {
	doc := calibrationDoc{name: r.Metadata.Name}
	switch r.Kind {
	case "Deployment", "StatefulSet", "Rollout", "DaemonSet", "Job", "CronJob":
		doc.isWorkload = true
		if app := r.Spec.Template.Metadata.Labels["app"]; app != "" {
			doc.name = app
		}
		for _, c := range r.Spec.Template.Spec.Containers {
			for _, e := range c.Env {
				set := e.Value != "" || e.ValueFrom != nil
				if !set {
					continue
				}
				switch e.Name {
				case envCalibrationInterval:
					doc.setsInterval = true
				case envCalibrationRates:
					doc.setsRates = true
				}
			}
		}
	case "ConfigMap":
		if r.Data[envCalibrationInterval] != "" {
			doc.setsInterval = true
		}
		if r.Data[envCalibrationRates] != "" {
			doc.setsRates = true
		}
	}
	return doc
}

// betweenLiteralStrings returns the unquoted contents of the double-quoted string
// literals appearing between an opening marker and the next closing marker after
// it. Used to read a declaration out of Go source without importing the package
// under test — test/arch deliberately depends on no service package.
func betweenLiteralStrings(src, open, shut string) []string {
	i := strings.Index(src, open)
	if i < 0 {
		return nil
	}
	rest := src[i+len(open):]
	j := strings.Index(rest, shut)
	if j < 0 {
		return nil
	}
	var out []string
	for _, m := range regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(rest[:j], -1) {
		out = append(out, m[1])
	}
	return out
}

// reasonMapKeys returns the keys declared in calibrationUnscheduledReason. Keys
// only: the values are multi-line concatenations whose fragments would otherwise
// be read as further keys.
func reasonMapKeys(src string) map[string]bool {
	i := strings.Index(src, "var calibrationUnscheduledReason = map[string]string{")
	if i < 0 {
		return nil
	}
	rest := src[i:]
	if j := strings.Index(rest, "\n}\n"); j >= 0 {
		rest = rest[:j]
	}
	out := map[string]bool{}
	for _, line := range strings.Split(rest, "\n") {
		m := regexp.MustCompile(`^\t"([a-z]+)":`).FindStringSubmatch(line)
		if m != nil {
			out[m[1]] = true
		}
	}
	return out
}
