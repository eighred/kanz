package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A PRE-TRADE MANDATE MUST BE RESOLVED FOR THE ORDER'S TENANT (#243).
//
// The mandate registry was keyed by portfolio_id alone, and portfolio_id is a
// CALLER-CHOSEN STRING off SubmitOrder — two funds each calling a portfolio
// "growth" is a coincidence, not an attack. Both landed in one bucket sorted by
// (effective_at, version), so the last effective mandate governed BOTH and
// tenant B's order cleared against tenant A's concentration limits and
// instrument allow-list. On a version-number collision Put's idempotent-replace
// branch deleted A's mandate outright, leaving A ungoverned — and with the
// shipped OMS_REQUIRE_MANDATE="false" that is ADMITTED UNCONSTRAINED, not
// refused.
//
// Two things keep that fixed, and each has an arm below.
//
//  1. THE SEAMS CARRY A TENANT. A composite map key behind an interface that
//     cannot carry a tenant is not a fix — it leaves every caller unable to
//     supply one. The parameter is what forces the question to be answered.
//
//  2. THE TENANT COMES OFF THE ENVELOPE. There is exactly one plausible wrong
//     answer and it compiles: the service's OWN configured tenant. The shipped
//     OMS runs OMS_TENANT="__system__" (infra/deploy/oms-deploy.yaml), so
//     scoping to it would ask for the platform's mandate for every customer
//     order, find none, and admit them all. This arm is default-deny: the
//     tenant argument must be one of the expressions listed below.

// tenantScopedSeams are the declarations that must keep a tenant parameter.
// Keyed by path, valued by the substring that proves the parameter survives.
var tenantScopedSeams = map[string]string{
	"internal/compliance/gate.go": "Mandate(ctx context.Context, tenantID, portfolioID string",
	"services/oms/internal/compliance/gate.go": "Check(ctx context.Context, tenantID string, " +
		"cmd *orderpb.SubmitOrder)",
}

// mandateTenantArgAllowed lists every expression permitted as the tenant
// argument of a mandate resolution, with where it comes from. DEFAULT-DENY:
// a call passing anything else fails until the expression is added here on
// purpose, which is the moment somebody has to justify it.
var mandateTenantArgAllowed = map[string]string{
	"d.TenantID": "OrderDelta.TenantID — set by the OMS adapter (services/oms/internal/compliance/" +
		"comp01.go) from the tenantID its Gate.Check was handed, which the order handler takes " +
		"off the command envelope.",
	"key.tenant": "monitor.bookKey.tenant — built from env.GetTenantId() at the top of the same " +
		"handler (services/compliance/internal/monitor/monitor.go).",
	"env.GetTenantId()": "the inbound envelope's tenant: the api-gateway stamps the authenticated " +
		"principal's tenant on the command, and bus.Validate requires it non-empty on the live path.",
}

var (
	// The resolution seam and the OMS gate seam, at their call sites.
	mandateCall = regexp.MustCompile(`\.Mandate\(\s*ctx\s*,\s*([^,]+?)\s*,`)
	gateCall    = regexp.MustCompile(`\.gate\.Check\(\s*ctx\s*,\s*([^,]+?)\s*,`)
)

func TestTheMandateSeamsCarryATenant(t *testing.T) {
	root := moduleRoot(t)
	var broken []string
	for rel, want := range tenantScopedSeams {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			broken = append(broken, rel+" (unreadable: "+err.Error()+")")
			continue
		}
		if !strings.Contains(string(b), want) {
			broken = append(broken, rel+" (no declaration matching `"+want+"`)")
		}
	}
	sort.Strings(broken)
	if len(broken) > 0 {
		t.Fatalf("these compliance seams no longer declare a tenant parameter:\n  %s\n\n"+
			"Without it a caller CANNOT scope the mandate lookup, and the registry falls back to "+
			"portfolio_id — a caller-chosen string. Two tenants naming a portfolio \"growth\" then "+
			"share one mandate, and the last one published governs both (#243).\n\n"+
			"If the signature moved rather than regressed, update tenantScopedSeams to match; do not "+
			"delete the entry.", strings.Join(broken, "\n  "))
	}
}

func TestEveryMandateLookupIsScopedByTheEnvelopesTenant(t *testing.T) {
	root := moduleRoot(t)

	type site struct{ where, arg string }
	var sites []site
	for _, dir := range []string{"internal", "services", "pkg", "cmd", "tools"} {
		base := filepath.Join(root, dir)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Fatalf("read %s: %v", path, rerr)
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			for _, re := range []*regexp.Regexp{mandateCall, gateCall} {
				for _, m := range re.FindAllStringSubmatch(string(b), -1) {
					sites = append(sites, site{where: rel, arg: strings.TrimSpace(m[1])})
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// NON-VACUITY. The pre-trade gate, the post-trade monitor and the OMS order
	// handler all resolve a mandate. A scan finding fewer means the call shapes
	// have moved and this guard is asserting nothing.
	if len(sites) < 3 {
		t.Fatalf("found only %d mandate-resolution call site(s) — expected at least 3 (the pre-trade "+
			"gate, the post-trade monitor, the OMS order handler). The guard is not finding them, so "+
			"it would pass no matter what tenant they pass", len(sites))
	}

	var offenders []string
	used := map[string]bool{}
	for _, s := range sites {
		if _, ok := mandateTenantArgAllowed[s.arg]; ok {
			used[s.arg] = true
			continue
		}
		offenders = append(offenders, s.where+": .Mandate/.Check(ctx, "+s.arg+", …)")
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("these mandate lookups are scoped by an expression that is not known to come from "+
			"the envelope:\n  %s\n\n"+
			"The tenant a mandate is resolved for must be the ORDER'S — env.GetTenantId(), which the "+
			"api-gateway stamps from the authenticated principal. It must NOT be the service's own "+
			"configured tenant: the shipped OMS runs OMS_TENANT=\"__system__\", so scoping to that "+
			"asks for the platform's mandate for every customer order, finds none, and admits them "+
			"unconstrained under OMS_REQUIRE_MANDATE=\"false\" (#243).\n\n"+
			"If the expression IS envelope-derived, add it to mandateTenantArgAllowed with the "+
			"file and line it is derived at.", strings.Join(offenders, "\n  "))
	}

	// DEAD ENTRIES: an allowed expression nothing passes any more is stale
	// permission, and the next wrong argument that happens to match it inherits
	// a justification written for something else.
	var dead []string
	for arg := range mandateTenantArgAllowed {
		if !used[arg] {
			dead = append(dead, arg+" (no call site passes it any more)")
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("mandateTenantArgAllowed has %d stale entr(y/ies):\n  %s",
			len(dead), strings.Join(dead, "\n  "))
	}
}
