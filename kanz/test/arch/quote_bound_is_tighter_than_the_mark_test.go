package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// THE QUOTED WIDTH'S BOUND MUST STAY TIGHTER THAN THE MARK'S, IN EVERY DEPLOYMENT.
//
// This is the invariant #956 established, and it is a DEPLOYMENT fact rather than
// a code one, which is why a config unit test cannot hold it. The OMS parses both
// OMS_PRICE_MAX_AGE and OMS_QUOTE_MAX_AGE from the environment; each is refused
// only if non-positive, so a manifest is free to set the width's bound to 60s
// against the mark's 30s and the pod starts perfectly.
//
// THAT STATE IS THE DEFECT ITSELF, RESTORED. A mark's producers are the venue
// adapters' 5s REST ticker polls, so 30s is six missed observations. A width's
// producer is market-ingest's 1s book-snapshot ticker, so the SAME 30s is thirty
// missed publishes — five times looser in the only unit that matters, on the one
// observation an execution report cannot recompute afterwards. Anything at or
// above the mark's bound puts the width back under a tolerance calibrated for a
// different feed.
//
// AND IT FAILS IN THE FLATTERING DIRECTION, which is why a guard is worth its
// weight here rather than a comment. A stale width is usually a TIGHTER width —
// the book was calm when it was last quoted, and the event that widened it is
// what stopped the quotes — so the half-spread comes back too small and the
// residual is charged to the algorithm as impact or timing. Nothing refuses, no
// alert fires, and the number reads as exact.
//
// WHAT THIS CHECKS: every deployed OMS names both bounds, both parse as positive
// durations, and the width's is strictly tighter than the mark's.
//
// WHAT IT DOES NOT CHECK: that 6s is the RIGHT number. That is a measurement
// against the quote producer's cadence, it is derived and cited in
// infra/deploy/oms-deploy.yaml beside the value, and
// services/oms/internal/config pins the shipped default. This holds the ordering,
// which is the part an edit to either line can silently invert.

// omsDeployManifests are the OMS deployments an operator can edit: the base, and
// every per-tenant render of it. The tenant files are generated from the base by
// internal/tenantgen — TestTenantComputeGuard holds them in sync — but they are
// COMMITTED YAML that a person can edit, so a guard reading only the base would
// pass while a tenant's OMS ran a bound nobody chose.
var omsDeployManifests = []string{
	"infra/deploy/oms-deploy.yaml",
	"infra/deploy/tenants/*/oms-*.yaml",
}

// envDurationRe pulls one env key's value out of either manifest style: the
// base's inline `- { name: K, value: "v" }` and tenantgen's expanded
// `- name: K` / `value: v` pair.
var envDurationRe = func(key string) *regexp.Regexp {
	return regexp.MustCompile(`(?:name:\s*` + key + `,\s*value:\s*"?([0-9a-z.]+)"?|name:\s*` + key + `\s*\n\s*value:\s*"?([0-9a-z.]+)"?)`)
}

func TestTheQuoteBoundIsTighterThanTheMarkBoundInEveryDeployment(t *testing.T) {
	root := moduleRoot(t)

	var files []string
	for _, pattern := range omsDeployManifests {
		matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pattern)))
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		files = append(files, matches...)
	}
	if len(files) == 0 {
		t.Fatal("no OMS deploy manifest found — this guard is reading nothing, which is not " +
			"the same as passing. Check omsDeployManifests against infra/deploy/")
	}

	priceRe := envDurationRe("OMS_PRICE_MAX_AGE")
	quoteRe := envDurationRe("OMS_QUOTE_MAX_AGE")

	for _, f := range files {
		rel, _ := filepath.Rel(root, f)
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		body := string(b)

		price, ok := durationFromEnv(t, rel, "OMS_PRICE_MAX_AGE", priceRe, body)
		if !ok {
			continue
		}
		quote, ok := durationFromEnv(t, rel, "OMS_QUOTE_MAX_AGE", quoteRe, body)
		if !ok {
			continue
		}

		if quote >= price {
			t.Errorf("%s sets OMS_QUOTE_MAX_AGE=%v against OMS_PRICE_MAX_AGE=%v. The quoted "+
				"width is back under a bound calibrated for the MARK's 5s ticker feed, which "+
				"against market-ingest's 1s quote cadence is five times looser than the mark "+
				"gets. Nothing refuses and no alert fires: the spread leg of every execution "+
				"attribution is simply measured against an older market, and a stale width is "+
				"a tighter one, so the cost comes back flattering. See #956 and the derivation "+
				"beside the value in infra/deploy/oms-deploy.yaml.", rel, quote, price)
		}
	}
}

// durationFromEnv reports the parsed value of one env key in one manifest, and
// FAILS when the key is missing rather than skipping it.
//
// ABSENCE IS THE FAILURE THIS GUARD EXISTS FOR AS MUCH AS INVERSION IS. Both
// keys have code defaults that satisfy the ordering, so an OMS with neither line
// runs correctly — and an operator reading the deployment to find out what bounds
// it enforces finds nothing, and the derivation that justifies 6s has nowhere to
// live. "Nothing configured" and "checked, and fine" must not look the same.
func durationFromEnv(t *testing.T, rel, key string, re *regexp.Regexp, body string) (time.Duration, bool) {
	t.Helper()
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Errorf("%s does not set %s. Both staleness bounds are stated in the manifest so an "+
			"operator can read what this pod enforces and compare the two; a code default that "+
			"appears nowhere is a bound nobody knows exists (#956).", rel, key)
		return 0, false
	}
	raw := m[1]
	if raw == "" {
		raw = m[2]
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		t.Errorf("%s sets %s=%q, which is not a duration — the OMS refuses to start on it", rel, key, raw)
		return 0, false
	}
	if d <= 0 {
		t.Errorf("%s sets %s=%v. mark.Source reads a non-positive bound as NEVER EXPIRES; the "+
			"OMS refuses to start rather than run with the staleness check off", rel, key, d)
		return 0, false
	}
	return d, true
}
