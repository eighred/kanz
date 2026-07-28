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

// EVERY METRIC NAMED IN A RULE, AN SLO OR A DASHBOARD MUST EXIST IN THE GO SOURCE.
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
func TestEveryObservabilityMetricExistsInGo(t *testing.T) {
	root := moduleRoot(t)
	obsRoot := filepath.Join(root, "infra", "observability")

	goMetrics := declaredGoMetrics(t, root)
	// NON-VACUITY, half one. A scan that finds no Go metrics would pass this
	// guard no matter how many orphans the config carries — the guard would be
	// broken, not the config, and it would look green while asserting nothing.
	if len(goMetrics) == 0 {
		t.Fatal("found zero kanz_* metric literals in the Go source — the scanner is broken, not the services")
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
			if !goMetrics[m] {
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
			t.Errorf("%s references %d metric(s) that no Go source emits: %s\n\n"+
				"A metric name here is a claim the platform exports that series. It does not. "+
				"An alert over it can never fire, an SLO over it can never breach, and a dashboard "+
				"panel over it renders empty — all three of which read as HEALTHY. Either instrument "+
				"the metric in Go, re-point this at the series that actually exists, or delete the "+
				"rule/panel. Do not add an entry to metricSurfacesPendingRepair to make this pass "+
				"unless there is a tracked issue that will remove it.",
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
var metricSurfacesPendingRepair = map[string]string{
	"dashboards/completeness.json": "issue #123: renders kanz_data_gap_missing_total, " +
		"kanz_data_reconcile_discrepancies_total, kanz_data_reconcile_match_latency_seconds and " +
		"kanz_data_reconcile_pending — all exported by internal/integrity, deleted in DATA-M4. " +
		"ZERO live metrics on this dashboard. #123 decides whether the three data-quality " +
		"dashboards are deleted or kept as the target contract for re-instrumenting the exporter; " +
		"it must not be resolved by quietly deleting them, because the DataQualityEvent stream " +
		"they would render still flows and still has live consumers (services/audit classifies it " +
		"as KindDataQuality, services/autopilot remediates gap/staleness/drift).",

	"dashboards/drift.json": "issue #123: renders kanz_data_drift_score, kanz_data_drift_threshold " +
		"and kanz_data_quality_events_total — same DATA-M4 deletion, zero live metrics. Worth " +
		"preserving from this one when it is repaired: the drift panels compared the score against " +
		"the EXPORTED threshold rather than a duplicated literal, so the alarm point tracked each " +
		"detector's own configuration automatically.",

	"dashboards/freshness.json": "issue #123: renders kanz_data_last_event_age_seconds, " +
		"kanz_data_quality_events_total and kanz_data_staleness_lag_seconds — same DATA-M4 " +
		"deletion, zero live metrics. Note kanz_data_last_event_age_seconds appeared in NO alert " +
		"rule, so issue #62's inventory of the orphaned contract missed it; it is only visible " +
		"because this guard reads the dashboards too.",
}

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
// literal anywhere in the module's Go source.
//
// A literal scan is sound HERE because this estate declares every Prometheus
// metric with a full literal Name (prometheus.CounterOpts{Name: "kanz_..."}).
// It would stop being sound the moment a metric were assembled from
// Namespace/Subsystem/Name — the guard would report a false MISSING and, worse,
// the obvious fix would be to weaken it. If that day comes, match on the
// assembled name rather than deleting the check.
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
			case ".git", "vendor", "node_modules", ".gotmp":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
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
