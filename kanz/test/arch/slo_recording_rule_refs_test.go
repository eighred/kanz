package arch

import (
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// EVERY slo:* SERIES A DASHBOARD OR AN ALERT USES MUST BE PRODUCED BY A RECORDING RULE.
//
// This is TestEveryObservabilityMetricExistsInSource's argument applied one layer
// up, and the layer it covers was genuinely uncovered: that guard matches
// `kanz_*` names, so it checks the RAW series a Go service exports and says
// nothing about the DERIVED series the recording rules compute. The SLO
// dashboards and every burn-rate alert are written entirely in the derived
// vocabulary — `slo:sli_error:ratio_rate1h`, `slo:error_budget:ratio` — and not
// one of those references was checked by anything.
//
// The failure mode is identical to the one that got three Grafana dashboards
// deleted in #123, and it is silent in exactly the same way:
//
//   - A panel querying a window nobody records (`ratio_rate45m`, or a `28d`
//     that looks obvious because slo.yaml's accounting window IS 28d) returns
//     an empty result. Grafana draws an empty panel, and a human reading a
//     board during an incident reads empty as QUIET.
//   - An alert whose expression names a series with no producer can never fire,
//     so the SLO it guards reads as permanently met.
//
// Neither shows up as an error anywhere. The typo is one character and the
// consequence is an unmonitored objective, so it is worth a guard.
//
// The check is the compile relationship: slo.recording.rules.yaml PRODUCES a
// set of series (its `record:` names), and everything else under
// infra/observability may only CONSUME from that set.
func TestEverySLORecordingRuleReferenceIsProduced(t *testing.T) {
	root := moduleRoot(t)
	obsRoot := filepath.Join(root, "infra", "observability")
	rulesFile := filepath.Join(obsRoot, "slo", "slo.recording.rules.yaml")

	produced := recordedSeries(t, rulesFile)
	// NON-VACUITY, half one. If the producer scan finds nothing — the file was
	// renamed, the `record:` syntax changed — then every reference below is an
	// orphan and this test would fail loudly for the wrong reason. Better to say
	// the scanner is broken than to let a rename turn the guard into noise.
	if len(produced) == 0 {
		t.Fatalf("found zero `record:` names in %s — the scanner is broken, not the rules", rulesFile)
	}

	refs := sloSeriesReferences(t, obsRoot, rulesFile)
	// NON-VACUITY, half two. A walk that finds no references passes no matter
	// what the dashboards say. This is the half that would break silently if the
	// dashboards moved directories.
	if len(refs) == 0 {
		t.Fatalf("found zero slo:* references under %s — the scanner is broken, not the config", obsRoot)
	}

	for _, file := range sortedKeys(refs) {
		var orphans []string
		for _, s := range refs[file] {
			if !produced[s] {
				orphans = append(orphans, s)
			}
		}
		if len(orphans) == 0 {
			continue
		}
		sort.Strings(orphans)
		t.Errorf("%s uses %d recording-rule series that %s does not produce: %s\n\n"+
			"A derived series name here is a claim that the rules compute it. They do not, so the "+
			"query returns an EMPTY result — an alert that can never fire, or a dashboard panel "+
			"that renders blank. Both read as healthy. Either add the recording rule (and keep it "+
			"consistent with slo.yaml, which is the source of truth), or fix the reference. Note "+
			"that a plausible-looking window is the likeliest mistake: only the windows named in "+
			"slo.yaml's defaults.burn_rates are compiled.",
			file, len(orphans), filepath.ToSlash(mustRel(obsRoot, rulesFile)), strings.Join(orphans, ", "))
	}
}

// sloSeriesRe matches a Prometheus recording-rule series name in the estate's
// convention: colon-separated levels, e.g. slo:sli_error:ratio_rate1h. Prometheus
// reserves the colon for recording rules precisely so derived series are
// distinguishable from exported ones, which is what makes this scannable.
var sloSeriesRe = regexp.MustCompile(`\bslo:[a-z_]+:[a-z_0-9]+`)

// recordDeclRe strips the `record: <name>` DECLARATIONS before a file is scanned
// for references, so a recording rule does not count as a consumer of itself.
// Chained rules (one recording rule reading another) are still checked, which is
// the reason for stripping declarations rather than skipping the whole file.
var recordDeclRe = regexp.MustCompile(`(?m)^\s*-?\s*record:\s*\S+`)

// recordedSeries returns the set of series names the recording rules produce.
func recordedSeries(t *testing.T, rulesFile string) map[string]bool {
	t.Helper()
	body := stripYAMLComments(readFile(t, rulesFile))

	out := map[string]bool{}
	for _, decl := range recordDeclRe.FindAllString(body, -1) {
		if name := sloSeriesRe.FindString(decl); name != "" {
			out[name] = true
		}
	}
	return out
}

// sloSeriesReferences maps each config file (relative to obsRoot, forward
// slashes) to the distinct slo:* series it references.
func sloSeriesReferences(t *testing.T, obsRoot, rulesFile string) map[string][]string {
	t.Helper()

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

		// Same split as TestEveryObservabilityMetricExistsInSource, for the same two
		// reasons. YAML: comments deliberately record series that were REMOVED
		// (the market-data freshness SLO), and a guard that flags those gets
		// neutered to make it pass, taking the record with it. JSON: it has no
		// comments and '#' is legitimate content (hex colours), so stripping
		// would truncate lines and could hide a real reference — failing OPEN,
		// the one direction this must never fail in.
		if ext != ".json" {
			body = stripYAMLComments(body)
		}
		if path == rulesFile {
			body = recordDeclRe.ReplaceAllString(body, "")
		}

		seen := map[string]bool{}
		var names []string
		for _, s := range sloSeriesRe.FindAllString(body, -1) {
			if seen[s] {
				continue
			}
			seen[s] = true
			names = append(names, s)
		}
		if len(names) == 0 {
			return nil
		}
		sort.Strings(names)
		out[filepath.ToSlash(mustRel(obsRoot, path))] = names
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", obsRoot, err)
	}
	return out
}

// A DASHBOARD MUST NOT NAME A SERVICE OR AN SLO.
//
// The burn-rate panels are written label-generically — `{{service}}/{{slo}}`
// over whatever the catalog contains — so an SLO added to slo.yaml and compiled
// into the rules appears on the board without anyone editing JSON. That is not a
// style preference; it is what stops the board from silently covering a subset.
//
// The alternative is per-service panels, and its failure is the one this estate
// has already had twice: someone adds an SLO, nobody adds the panel, and the
// dashboard keeps showing all-green for the services it happens to name. A
// reader cannot tell a board that covers everything from one that covers four of
// five, because both are green.
//
// So: no dashboard query may pin `service=` or `slo=`. Enumerating in the LEGEND
// is fine (that is the label reference), and this checks selectors only.
func TestSLODashboardsDoNotPinAServiceOrSLO(t *testing.T) {
	root := moduleRoot(t)
	dashDir := filepath.Join(root, "infra", "observability", "dashboards")

	// A label matcher against service/slo inside a selector: service="x",
	// slo=~"y". Deliberately catches =, !=, =~ and !~ — an exclusion pins the
	// board to a hard-coded list just as effectively as an inclusion does.
	//
	// THE `\\?` IS NOT DECORATION. Every panel query lives inside a JSON string,
	// so the quote that opens a PromQL label value is escaped on disk as `=\"`,
	// not `="`. Without the optional backslash this pattern matches nothing a
	// dashboard can actually contain — the guard would pass on every input,
	// including the exact mutation it exists to catch. It was written that way
	// first and only the mutation test found it. PromQL's other string forms are
	// allowed for too, since neither needs escaping in JSON.
	pinRe := regexp.MustCompile("\\b(service|slo)\\s*(=~|!~|!=|=)\\s*\\\\?[\"'`]")

	scanned := 0
	err := filepath.WalkDir(dashDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || strings.ToLower(filepath.Ext(path)) != ".json" {
			return nil
		}
		scanned++
		body := readFile(t, path)
		if m := pinRe.FindAllString(body, -1); len(m) > 0 {
			t.Errorf("%s pins %d label selector(s): %s\n\n"+
				"Panels here are label-generic on purpose: they render whatever the SLO catalog "+
				"contains, so an SLO added to slo.yaml shows up without editing this file. A pinned "+
				"selector turns the board into a hard-coded list, and the next SLO nobody adds a "+
				"panel for is invisible — indistinguishable from an SLO with nothing wrong. Use "+
				"{{service}}/{{slo}} in the legend and leave the query unfiltered.",
				filepath.ToSlash(mustRel(root, path)), len(m), strings.Join(m, ", "))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dashDir, err)
	}

	// NON-VACUITY. The dashboards directory has been EMPTY before — #123 deleted
	// all three of its files — so "no dashboards found" is a state this repo has
	// actually been in, and it must not read as the rule holding.
	if scanned == 0 {
		t.Fatalf("scanned no dashboard JSON under %s — the guard is pointed at nothing, which is "+
			"not the same as the rule holding", filepath.ToSlash(mustRel(root, dashDir)))
	}
}
