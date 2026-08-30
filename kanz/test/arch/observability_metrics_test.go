package arch

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// EVERY METRIC NAMED IN A RULE, AN SLO OR A DASHBOARD MUST BE DECLARED IN THE
// SOURCE THAT EMITS IT — GO **OR** PYTHON.
//
// This guard exists because the platform has already shipped an entire
// observability layer built on series nothing emitted. `internal/integrity`
// exported the kanz_data_* contract and was deleted in DATA-M4; the alert
// rules, the market-data freshness SLO and all three Grafana dashboards that
// consumed it were not, and nothing noticed for months.
//
// The failure mode is worse than a broken dashboard, because none of it looks
// broken:
//
//   - An alert whose expression compares a threshold against a series with no
//     producer yields an EMPTY VECTOR, so it can never fire. A data-integrity
//     alerting layer showing zero firing alerts is indistinguishable from a
//     healthy platform.
//   - The one rule that could fire was `absent(kanz_data_quality_events_total)`
//     — so it fired continuously and forever, which trains an operator to
//     ignore the layer.
//   - An SLO compiled from an absent gauge produces empty windowed ratios, so
//     its burn-rate alerts never fire and the objective reads as permanently
//     met.
//   - A dashboard is the worst of the three, because a human deliberately opens
//     it during an incident and empty panels read as QUIET rather than BROKEN.
//
// A metric name in a config file is a claim that the platform emits it. This
// test is what makes that claim checkable.
//
// CROSS-LANGUAGE ON PURPOSE, and the same reasoning as
// TestEverySubjectHasAKafkaTopic. kanz-py is a scrape target like any other
// service, so an alert or SLO over a kanz_inference_* series is exactly as
// legitimate as one over kanz_risk_*. A truth set built only from the Go source
// would call every one of those an orphan and fail a CORRECT configuration —
// and a guard that is red on correct configuration gets weakened rather than
// fixed. That made this the piece #241 was blocked behind: the inference
// metrics work could not land while the guard was structurally blind to the
// language that would declare them.
//
// kanz-py DECLARES ZERO METRICS TODAY — it does not even depend on
// prometheus_client — so the Python half of the truth set is currently empty
// and this guard is green either way. That is precisely the situation in which
// a broken scanner is invisible, so two things stand in for the metric that
// does not exist yet: the pyFiles floor below proves the walk still reaches
// kanz-py's source, and TestPythonMetricScannerRecognizesPrometheusClient
// proves the extractor still matches the shapes prometheus_client uses. Zero
// found is only meaningful when both of those hold.
func TestEveryObservabilityMetricExistsInSource(t *testing.T) {
	root := moduleRoot(t) // .../kanz
	repo := filepath.Dir(root)
	obsRoot := filepath.Join(root, "infra", "observability")

	declared := declaredGoMetrics(t, root)
	// NON-VACUITY, half one. A scan that finds no Go metrics would pass this
	// guard no matter how many orphans the config carries — the guard would be
	// broken, not the config, and it would look green while asserting nothing.
	if len(declared) == 0 {
		t.Fatal("found zero kanz_* metric literals in the Go source — the scanner is broken, not the services")
	}

	pyMetrics, pyFiles := declaredPythonMetrics(t, filepath.Join(repo, "kanz-py"))
	// NON-VACUITY, half three — a FILE floor, not a metric floor. kanz-py has no
	// metrics yet, so `len(pyMetrics) == 0` is the correct answer and cannot be
	// asserted against; what can be asserted is that the walk still has source
	// to read. Without this, moving or renaming kanz-py would silently return an
	// empty Python truth set forever, and the first inference alert to land
	// would be blamed on the alert.
	if pyFiles == 0 {
		t.Fatalf("scanned zero .py files under %s — the Python scanner is broken, or kanz-py moved", filepath.Join(repo, "kanz-py"))
	}
	for m := range pyMetrics {
		declared[m] = true
	}

	refs := referencedMetrics(t, obsRoot)
	// NON-VACUITY, half two. Same reasoning from the other side: if the config
	// walk stops finding files (a rename, a moved directory), every assertion
	// below becomes trivially true.
	if len(refs) == 0 {
		t.Fatalf("found zero kanz_* metric references under %s — the scanner is broken, not the config", obsRoot)
	}

	seenOrphanedFiles := map[string]bool{}

	for _, file := range sortedKeys(refs) {
		var orphans []string
		for _, m := range refs[file] {
			if !declared[m] {
				orphans = append(orphans, m)
			}
		}
		sort.Strings(orphans)

		if _, pending := metricSurfacesPendingRepair[file]; pending {
			if len(orphans) > 0 {
				seenOrphanedFiles[file] = true
			}
			continue
		}
		if len(orphans) > 0 {
			t.Errorf("%s references %d metric(s) that no Go or Python source declares: %s\n\n"+
				"A metric name here is a claim the platform exports that series. It does not. "+
				"An alert over it can never fire, an SLO over it can never breach, and a dashboard "+
				"panel over it renders empty — all three of which read as HEALTHY. Either instrument "+
				"the metric in Go or in kanz-py, re-point this at the series that actually exists, or "+
				"delete the rule/panel. Do not add an entry to metricSurfacesPendingRepair to make "+
				"this pass unless there is a tracked issue that will remove it.",
				file, len(orphans), strings.Join(orphans, ", "))
		}
	}

	// DEAD-ENTRY CHECK. An allow-list entry naming a file that no longer has
	// orphaned metrics — because it was fixed, or deleted — is a stale
	// exemption. It protects nothing, it sits in the list looking load-bearing,
	// and the next orphan added to that file passes unnoticed. Same shape and
	// same reasoning as dlqExemptBroadcastOnlyConsumers and
	// retryCertifiedConsumers in bus_dlq_test.go.
	var dead []string
	for file := range metricSurfacesPendingRepair {
		if !seenOrphanedFiles[file] {
			dead = append(dead, file)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		t.Fatalf("metricSurfacesPendingRepair names %d file(s) that no longer carry an orphaned "+
			"metric: %s\n\nThe repair happened — delete the entry. A stale exemption re-opens the "+
			"hole it was documenting.", len(dead), strings.Join(dead, ", "))
	}
}

// metricSurfacesPendingRepair is a DEFAULT-DENY allow-list: every config file
// under infra/observability is checked unless it is named here with a
// justification and a tracked issue that will remove it.
//
// It exists so this guard can land BEFORE the repairs it demands, instead of
// waiting behind them and protecting nothing in the meantime. Listing a file
// cannot silently widen the exemption — only enumerating a specific path can,
// and that requires a reviewed edit to this file. The dead-entry check above
// then forces each entry out again the moment its repair lands.
// IT IS CURRENTLY EMPTY, AND THAT IS THE POINT. It held the three Grafana
// data-quality dashboards until #123 deleted them; when they went, the
// dead-entry check above failed the build naming all three, and these entries
// came out in the same change. That is the intended lifecycle — an exemption
// here is a debt with an issue number on it, not a permanent carve-out.
//
// Leave it empty rather than deleting the map. An empty default-deny list is a
// working guard with nothing exempted; removing it would mean the next person
// needing a temporary exemption has to reinvent the mechanism, and would most
// likely reach for weakening the check instead.
var metricSurfacesPendingRepair = map[string]string{}

// referencedMetrics maps each config file (path relative to obsRoot, with
// forward slashes so the allow-list keys are platform-independent) to the
// distinct kanz_* metric names it references.
func referencedMetrics(t *testing.T, obsRoot string) map[string][]string {
	t.Helper()

	metricRe := regexp.MustCompile(`kanz_[a-z0-9_]+`)
	out := map[string][]string{}

	err := filepath.WalkDir(obsRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" && ext != ".json" {
			return nil
		}

		body := readFile(t, path)

		// STRIP COMMENTS FOR YAML ONLY, AND NEVER FOR JSON.
		//
		// YAML: issue #62 deliberately left orphaned metric names inside
		// explanatory comments — the record of what was removed and why. A
		// guard that reads raw lines flags those forever and gets neutered to
		// make it pass, taking the documentation with it.
		//
		// JSON has no comments, and '#' is legitimate content there — Grafana
		// panels carry hex colours like "#FF0000". Stripping from '#' to
		// end-of-line would TRUNCATE those lines and could hide a real metric
		// reference behind a colour, which fails OPEN. That is the one
		// direction this guard must never fail in, so JSON is scanned whole.
		if ext != ".json" {
			body = stripYAMLComments(body)
		}

		seen := map[string]bool{}
		var names []string
		for _, m := range metricRe.FindAllString(body, -1) {
			m = normalizeMetric(m)
			if seen[m] {
				continue
			}
			seen[m] = true
			names = append(names, m)
		}
		if len(names) == 0 {
			return nil
		}

		rel, rerr := filepath.Rel(obsRoot, path)
		if rerr != nil {
			return rerr
		}
		sort.Strings(names)
		out[filepath.ToSlash(rel)] = names
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", obsRoot, err)
	}
	return out
}

// declaredGoMetrics returns every kanz_* metric name that appears as a string
// literal in the module's non-test Go source.
//
// A literal scan is sound HERE because this estate declares every Prometheus
// metric with a full literal Name (prometheus.CounterOpts{Name: "kanz_..."}).
// It would stop being sound the moment a metric were assembled from
// Namespace/Subsystem/Name — the guard would report a false MISSING and, worse,
// the obvious fix would be to weaken it. If that day comes, match on the
// assembled name rather than deleting the check.
//
// _test.go IS EXCLUDED, AND THAT IS NOT TIDINESS. A name that only ever appears
// in a test is not a series any deployed binary exports, so counting it as
// declared makes an alert over it pass while it can never fire — the exact
// fail-open this guard exists to prevent, reached through the truth set instead
// of the config. It was already happening: metric_writer_test.go builds its
// fixtures from six kanz_sample_* literals and pkg/observability's test from
// kanz_test_custom_total, so an alert naming any of those seven would have been
// accepted. Adding this file's own prometheus_client fixtures to the pile is
// what surfaced it: with tests included, the example bodies in
// TestPythonMetricScannerRecognizesPrometheusClient silently satisfied a rule
// over kanz_inference_predictions_total that nothing declared in Python — the
// proof of the Python scanner passed without the Python scanner.
func declaredGoMetrics(t *testing.T, root string) map[string]bool {
	t.Helper()

	literalRe := regexp.MustCompile(`"(kanz_[a-z0-9_]+)"`)
	out := map[string]bool{}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".claude", "vendor", "node_modules", ".gotmp":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, m := range literalRe.FindAllStringSubmatch(string(b), -1) {
			out[normalizeMetric(m[1])] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// pyMetricDecl matches a prometheus_client metric constructor whose name is a
// literal — `Counter("kanz_inference_predictions_total", ...)` — including the
// black-formatted multi-line shape, because `\s` spans newlines, and the
// keyword form `Counter(name="kanz_...")`.
//
// ANCHORED ON THE CONSTRUCTOR, not on a bare string-literal scan, for the same
// reason pySubjectAssign in kafka_topology_test.go anchors on an assignment:
// prose must not be able to declare a metric. A docstring or comment naming
// kanz_inference_predictions_total is not a call to a metric class, so it is
// structurally invisible here and no regex has to special-case comments.
//
// The residual blind spot is the same one declaredGoMetrics documents, in the
// same direction: a name assembled at runtime — an f-string, or
// prometheus_client's own namespace=/subsystem= kwargs, which join with '_' to
// build the exposed series — is not a literal and will read as MISSING. That is
// a false red, and the obvious fix is to weaken this. Match the assembled name
// instead.
var pyMetricDecl = regexp.MustCompile(`\b(Counter|Gauge|Histogram|Summary|Info|Enum)\s*\(\s*(?:name\s*=\s*)?"(kanz_[a-z0-9_]+)"`)

// pythonMetricNames returns the series names a Python source body DECLARES, as
// Prometheus would expose them — which is not always the string in the source.
//
// prometheus_client GENERATES a suffix for two of the six classes: a Counter
// named "kanz_x" is exported as kanz_x_total (and a name already ending in
// _total is not doubled), and an Info named "kanz_x" is exported as kanz_x_info.
// An alert can only ever reference the exported name, so recording the literal
// alone would fail a correct pairing of Counter("kanz_x") with a rule over
// kanz_x_total. This is the Python-side counterpart of normalizeMetric, which
// handles the other direction for histogram/summary sub-series.
//
// Split out from the walk so the extractor can be exercised directly against
// known-good and known-bad shapes — see
// TestPythonMetricScannerRecognizesPrometheusClient. With kanz-py declaring no
// metrics, that test is the only thing standing between "found zero, correctly"
// and "found zero, because it is broken".
func pythonMetricNames(body string) []string {
	var out []string
	for _, m := range pyMetricDecl.FindAllStringSubmatch(body, -1) {
		kind, name := m[1], normalizeMetric(m[2])
		switch kind {
		case "Counter":
			name = strings.TrimSuffix(name, "_total") + "_total"
		case "Info":
			name = strings.TrimSuffix(name, "_info") + "_info"
		}
		out = append(out, name)
	}
	return out
}

// declaredPythonMetrics returns every metric kanz-py declares, plus the number
// of .py files scanned so the caller can tell "no metrics" apart from "no
// source". Test files are skipped — a metric declared in a test is not one the
// deployed service emits — matching the same exclusion in pythonSubjects.
func declaredPythonMetrics(t *testing.T, pyRoot string) (map[string]bool, int) {
	t.Helper()

	out := map[string]bool{}
	files := 0

	err := filepath.WalkDir(pyRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "__pycache__", ".venv", "tests":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".py") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		files++
		for _, m := range pythonMetricNames(string(b)) {
			out[m] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", pyRoot, err)
	}
	return out, files
}

// THE PYTHON SCANNER MUST BE PROVEN ON A SHAPE, BECAUSE IT HAS NO DATA.
//
// kanz-py declares no metrics, so declaredPythonMetrics correctly returns an
// empty set — and an extractor that matches nothing at all returns exactly the
// same thing. Every regression this could suffer (a rename of the classes, a
// formatter that puts the name on its own line, someone "simplifying" the
// pattern) would leave the guard green while silently reverting it to Go-only.
// This test is the difference between those two states: it feeds the extractor
// the shapes prometheus_client actually produces and asserts the exported
// series names come back.
//
// The negative cases matter as much as the positive ones. Prose must not
// declare a metric — otherwise a comment naming a series would satisfy an alert
// over it, which fails OPEN, the one direction this guard must never fail in.
func TestPythonMetricScannerRecognizesPrometheusClient(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{{
		name: "counter with explicit _total",
		body: `PREDICTIONS = Counter("kanz_inference_predictions_total", "Predictions scored.", ["tenant_id"])`,
		want: []string{"kanz_inference_predictions_total"},
	}, {
		// prometheus_client appends _total itself; the rule can only name the
		// exported series, so the scanner has to agree with the client.
		name: "counter without _total is exported with it",
		body: `PREDICTIONS = Counter("kanz_inference_predictions", "Predictions scored.")`,
		want: []string{"kanz_inference_predictions_total"},
	}, {
		name: "black-formatted multi-line declaration",
		body: `LOADED = Gauge(
    "kanz_inference_model_loaded",
    "1 when a model is resident.",
    ["model_id"],
)`,
		want: []string{"kanz_inference_model_loaded"},
	}, {
		name: "keyword name argument",
		body: `LAT = Histogram(name="kanz_inference_latency_seconds", documentation="Scoring latency.")`,
		want: []string{"kanz_inference_latency_seconds"},
	}, {
		name: "summary and info",
		body: `S = Summary("kanz_inference_batch_bytes", "Batch size.")
BUILD = Info("kanz_inference_build", "Build metadata.")`,
		want: []string{"kanz_inference_batch_bytes", "kanz_inference_build_info"},
	}, {
		// The reason for anchoring on the constructor rather than scanning for
		// kanz_* literals: this would otherwise satisfy an alert over a series
		// nothing emits.
		name: "prose naming a metric declares nothing",
		body: `# TODO: export kanz_inference_predictions_total once batching lands.
def score(x):
    """Emits kanz_inference_latency_seconds when instrumented."""
    return x`,
		want: nil,
	}, {
		// Documented blind spot, asserted so a future reader finds it here
		// rather than in a false MISSING on a rule.
		name: "assembled name is not seen",
		body: `P = Counter(f"kanz_inference_{kind}_total", "Assembled at runtime.")`,
		want: nil,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pythonMetricNames(tc.body)
			sort.Strings(got)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("pythonMetricNames = %v, want %v\n\nThe Python half of the truth set is "+
					"only meaningful while this holds. If the extractor no longer sees a "+
					"prometheus_client declaration, every kanz_inference_* metric an alert names "+
					"reads as undeclared — or worse, if prose now matches, an alert over a series "+
					"nothing emits passes.", got, want)
			}
		})
	}
}

// normalizeMetric strips the suffixes Prometheus GENERATES for histograms and
// summaries. A Go histogram declared as kanz_x_duration_seconds produces
// kanz_x_duration_seconds_bucket/_count/_sum at scrape time, and none of those
// three ever appear in Go. Matching them literally is how a correct
// configuration turns a guard red — and a guard that is red on correct
// configuration gets deleted rather than fixed. Doing this comparison by hand
// produced four false positives.
func normalizeMetric(m string) string {
	for _, suffix := range []string{"_bucket", "_count", "_sum"} {
		if strings.HasSuffix(m, suffix) {
			return strings.TrimSuffix(m, suffix)
		}
	}
	return m
}

// stripYAMLComments and sortedKeys are shared with the rest of this package —
// see nats_bootstrap_posture_test.go and spiffe_env_wiring_test.go. This guard
// made stripYAMLComments quote-aware rather than adding a second copy of it;
// the reason is documented at its definition.
