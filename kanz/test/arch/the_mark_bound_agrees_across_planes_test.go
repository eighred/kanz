package arch

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// THE MARK'S STALENESS BOUND IS ONE NUMBER, AND TWO PLANES EACH WRITE IT DOWN (#1002).
//
// The OMS and the compliance service run the SAME control over the SAME
// observation. Both fold internal/marketdata/mark.Source over the same subjects
// (mark.DefaultSubjects), both hand that fold to internal/compliance.JoinEquity,
// and both get their answer from the one LeverageRule. The only thing that
// differs is the staleness bound each Source was constructed with: the OMS's
// OMS_PRICE_MAX_AGE before the trade, the monitor's COMPLIANCE_PRICE_MAX_AGE
// after it. infra/deploy/compliance-deploy.yaml has claimed "Matches
// OMS_PRICE_MAX_AGE" since it was written and nothing compared them — a safety
// property asserted in prose, which is the dated evidence AGENTS.md warns about
// rather than a check.
//
// # Why EQUAL, and not "compliance no looser than the OMS"
//
// The obvious reading is that the control plane must merely be no more
// permissive than the execution plane, which would make this an ordering. It is
// not, because BOTH directions are harms and the tighter one is the worse of the
// two.
//
// COMPLIANCE LOOSER THAN THE OMS is the permissive direction. The post-trade
// monitor values a book at a mark the pre-trade gate has already refused to
// price an order from, so the control plane reports a gross-leverage ratio
// computed from prices the execution plane calls too old to trade on. The two
// halves of one control then answer the same question differently, and neither
// says so — which is the state buildPostTradeValuation's own comment names as
// the reason both halves fold the same packages.
//
// COMPLIANCE TIGHTER THAN THE OMS IS NOT THE SAFE SIDE. MarkEquity is
// all-or-nothing by design: one holding whose mark has aged out leaves the whole
// book on its gross-positions basis, and LeverageRule then fails closed with
// "leverage cannot be verified". The monitor emits that refusal as a BREACH
// FACT, and AUTO-01 halts and escalates on a breach. So a compliance bound
// tighter than the OMS's converts an ordinary feed gap the execution plane is
// still trading straight through into an automated halt of a fund that is not
// breaching anything. Failing closed is right; failing closed EARLIER THAN THE
// PLANE THAT PRICES THE ORDER is a self-inflicted outage with a control's name
// on it.
//
// Neither direction is tolerable, so the relation is equality.
//
// # What this checks
//
// Every deployment on either plane states the bound, and every one of them
// states the SAME duration. Absence fails as loudly as disagreement: both keys
// have a code default that satisfies the invariant, so a manifest with neither
// line runs correctly while an operator reading the deployment to find out what
// this pod enforces finds nothing. The two code defaults are compared against
// each other for the same reason one layer down — a test rig or a local run with
// neither key set is exactly where a divergence would be invisible.
//
// # What it does not check
//
// That 30s is the RIGHT number. That is a measurement against the mark's
// producers — the venue adapters' 5s REST ticker polls — and it is derived and
// cited beside the value in infra/deploy/oms-deploy.yaml. Nor does it require
// the manifests to equal the code defaults: moving both planes to a different
// value is a supported operator change, and moving only one is the defect this
// guard exists for.

// complianceDeployManifests are the compliance deployments an operator can edit.
//
// THE TENANT GLOB MATCHES NOTHING TODAY, deliberately. internal/tenantgen
// renders accounting, archiver, oms and risk-engine per tenant; compliance is
// estate-wide, so every tenant's OMS is measured against this one bound. The
// pattern is here so a future per-tenant compliance render cannot land outside
// this guard's reach without anybody noticing.
var complianceDeployManifests = []string{
	"infra/deploy/compliance-deploy.yaml",
	"infra/deploy/tenants/*/compliance-*.yaml",
}

// markBoundPlanes pairs each plane's env key with the manifests that may set it.
// The OMS list is shared with TestTheQuoteBoundIsTighterThanTheMarkBoundInEveryDeployment
// rather than restated, so a manifest added for one guard is covered by both.
var markBoundPlanes = []struct {
	key       string
	manifests []string
}{
	{key: "OMS_PRICE_MAX_AGE", manifests: omsDeployManifests},
	{key: "COMPLIANCE_PRICE_MAX_AGE", manifests: complianceDeployManifests},
}

// markBoundConfigSources are the two config loaders that supply the shipped
// default when a deployment sets neither key.
var markBoundConfigSources = []struct {
	key  string
	file string
}{
	{key: "OMS_PRICE_MAX_AGE", file: "services/oms/internal/config/config.go"},
	{key: "COMPLIANCE_PRICE_MAX_AGE", file: "services/compliance/internal/config/config.go"},
}

// envOrDefaultRe finds the shipped default one config file passes to env.Or for
// a key. It is run over STRIPPED source: both files name their key repeatedly in
// the prose around the call, and a guard that greps raw source matches its own
// comments.
func envOrDefaultRe(key string) *regexp.Regexp {
	return regexp.MustCompile(`env\.Or\("` + key + `",\s*"([^"]+)"\)`)
}

func TestTheMarkStalenessBoundAgreesAcrossThePlanes(t *testing.T) {
	root := moduleRoot(t)

	// where records every manifest that stated each distinct duration, so a
	// failure names the files an operator has to reconcile rather than two
	// numbers.
	where := map[time.Duration][]string{}

	for _, plane := range markBoundPlanes {
		files := markBoundManifests(t, root, plane.key, plane.manifests)
		re := envDurationRe(plane.key)
		for _, f := range files {
			rel := filepath.ToSlash(mustRel(root, f))
			d, ok := durationFromEnv(t, rel, plane.key, re, readFile(t, f))
			if !ok {
				continue
			}
			where[d] = append(where[d], rel+" ("+plane.key+")")
		}
	}
	if t.Failed() {
		return
	}

	if len(where) != 1 {
		t.Errorf("the reference mark's staleness bound disagrees across the planes:\n%s\n\n"+
			"The OMS prices an order from this mark and the compliance monitor values the same "+
			"book from it, through the same mark.Source and the same LeverageRule. A looser "+
			"compliance bound reports a leverage ratio off prices the OMS refuses to trade on; a "+
			"tighter one refuses \"leverage cannot be verified\" for a book the OMS priced "+
			"happily, and that refusal is emitted as a BREACH FACT that AUTO-01 halts and "+
			"escalates on. Move both planes or neither (#1002).", markBoundListing(where))
	}

	// THE DEFAULTS ARE THE SAME INVARIANT one layer down. Every deployed manifest
	// states the key above, so a divergence here reaches no pod — it reaches every
	// test rig, kind cluster and local run that sets neither, which is where the
	// two planes would quietly stop agreeing with nothing to show for it.
	defaults := map[string][]string{}
	for _, src := range markBoundConfigSources {
		body := readStripped(t, filepath.Join(root, filepath.FromSlash(src.file)))
		m := envOrDefaultRe(src.key).FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("could not find the env.Or default for %s in %s — the load moved, and this "+
				"guard can no longer see which bound an unconfigured pod runs", src.key, src.file)
		}
		if _, err := time.ParseDuration(m[1]); err != nil {
			t.Fatalf("%s defaults %s to %q, which is not a duration — the service refuses to start "+
				"on it", src.file, src.key, m[1])
		}
		defaults[m[1]] = append(defaults[m[1]], src.file+" ("+src.key+"="+m[1]+")")
	}
	if len(defaults) != 1 {
		var lines []string
		for _, v := range defaults {
			lines = append(lines, v...)
		}
		sort.Strings(lines)
		t.Errorf("the two planes ship different default mark staleness bounds:\n  %s\n\n"+
			"Both manifests state the key, so no deployed pod runs these — every environment that "+
			"sets neither does, and there the pre-trade and post-trade halves of one leverage "+
			"control would silently disagree with no manifest to read the difference off (#1002).",
			strings.Join(lines, "\n  "))
	}
}

// markBoundManifests resolves one plane's patterns, and refuses to read nothing.
//
// A LITERAL PATTERN MUST MATCH. A glob may legitimately come back empty — there
// is no per-tenant compliance render today — but a named file that has been
// moved or renamed would otherwise silently remove a whole plane from the
// comparison, and a guard comparing one value against itself passes forever.
func markBoundManifests(t *testing.T, root, key string, patterns []string) []string {
	t.Helper()
	var files []string
	for _, pattern := range patterns {
		matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pattern)))
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		if len(matches) == 0 && !strings.ContainsAny(pattern, "*?[") {
			t.Fatalf("%s does not exist, so %s would be compared against nothing. Check "+
				"markBoundPlanes against infra/deploy/", pattern, key)
		}
		files = append(files, matches...)
	}
	if len(files) == 0 {
		t.Fatalf("no manifest found for %s — this guard is reading nothing, which is not the "+
			"same as passing", key)
	}
	sort.Strings(files)
	return files
}

// markBoundListing renders the disagreement grouped by the value each manifest
// set, in a stable order.
func markBoundListing(where map[time.Duration][]string) string {
	values := make([]time.Duration, 0, len(where))
	for d := range where {
		values = append(values, d)
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	var b strings.Builder
	for _, d := range values {
		sort.Strings(where[d])
		b.WriteString("  " + d.String() + ":\n")
		for _, f := range where[d] {
			b.WriteString("    " + f + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
