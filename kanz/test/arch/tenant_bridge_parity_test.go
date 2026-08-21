package arch

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/tenantgen"
)

// A TENANT WITH COMPUTE MUST HAVE A WAY TO SEND ITS FACTS BACK (#637).
//
// # What went wrong without it
//
// The MT-02 bridge carries order COMMANDS from __system__ into a tenant's NATS
// account. Nothing carried anything back. NATS accounts are isolated by
// construction; tenancy.yaml's own header says so ("An account's subjects are
// physically invisible to other accounts"). So every subject oms-acme published
// was invisible outside acme, and no platform service was a member of that
// account: accounting, risk-engine, audit, archiver, lake-sink, lineage,
// compliance, tv-sync, market-data and both venue adapters are all __system__
// users, each pinned to that tenant literally by a *_TENANT env var.
//
// A provisioned tenant therefore got orders IN and nothing OUT. Its OMS accepted
// an order, wrote its rows, and published the accepted FACT, the fills and
// risk.position.changed into an account consumed by nobody — no ledger entry, no
// positions, no audit trail, no Kafka archive — with every one of those services
// healthy and Ready throughout, because "this tenant produced no events" and
// "this tenant's events cannot reach me" are the same observable state.
//
// THIS IS NOT A LEAK, AND THE DISTINCTION MATTERS. Isolation holds in both
// directions — no tenant sees another's events, and a tenant saw nothing of its
// own either. The boundary is not leaking; it was half-built, and the half that
// was built is the one that lets capital move.
//
// # The two remedies, and why the guard accepts either
//
// There are exactly two ways a tenant's FACT can reach a platform
// responsibility, and they are not interchangeable per role:
//
//  1. RUN THE CONSUMER INSIDE THE TENANT'S ACCOUNT. Account membership IS the
//     return path — no export, no import, no subject rewriting. This is the only
//     option for a consumer whose book is TENANT-PINNED (internal/pg.NewTenantPool
//     binds a pool to one tenant for the process lifetime, #97; internal/topic.For
//     REFUSES an envelope whose tenant_id differs from the archiver's). It is how
//     the archiver moved.
//
//  2. EXPORT THE TENANT'S FACT SUBJECTS AND IMPORT THEM IN __system__. Only
//     honest for a consumer that is genuinely multi-tenant in code — audit takes
//     TenantID off the envelope. And it must land on a TENANT-PREFIXED subject:
//     importing unprefixed would deliver a tenant's FACTs to the platform
//     archiver (which NACKs, forever) and the platform accounting (which would
//     fold them into __system__'s own book — #223).
//
// So the guard requires a path per (tenant, role) and accepts either shape. What
// it will not accept is neither.
//
// # What it does not do
//
// It does not decide which remedy a role gets. That is a platform decision with
// a read-path consequence — the api-gateway dials one address per service — and
// it is tracked per role in the exemptions below, not settled here.

// factConsumerRole is a platform responsibility that must record a tenant's
// FACTs. The list is deliberately short: it is the set whose absence #637
// enumerated as an observable loss, not every consumer on the bus.
type factConsumerRole struct {
	// Name is both the role and, where remedy 1 applies, the name a
	// tenantgen.Service must carry for the guard to see it as satisfied.
	Name string
	// Loses states what the tenant loses while this role has no path. It is
	// printed on failure, so an operator reads the consequence, not the rule.
	Loses string
}

var factConsumerRoles = []factConsumerRole{
	{
		Name: "accounting",
		Loses: "no book of record — no ledger entry is posted, so NAV, cash and the buying-power " +
			"gate are all computed over an empty book",
	},
	{
		Name: "archiver",
		Loses: "no archive and no DR — the tenant's FACTs live only in NATS and age off at the " +
			"stream's max-age. This is the one CLAUDE.md's sequencing rule names: real orders must " +
			"never be placed against a store that may not be backed up",
	},
	{
		Name:  "audit",
		Loses: "no audit trail — the cross-tenant compliance record has no entry for a trade that happened",
	},
	{
		Name:  "risk-engine",
		Loses: "no risk — the engine holds no positions for this tenant, so every measure reports on nothing",
	},
}

// tenantBridgeExempt names (tenant, role) pairs whose FACTs are known to reach
// nothing, with the issue that resolves each one.
//
// DEFAULT-DENY, AND THESE ENTRIES ARE DEBT RECORDS RATHER THAN DECISIONS. The
// arrangement each describes is broken, not chosen. They are listed so main
// stays green while the state is stated in exactly one place, and so a SECOND
// tenant cannot be onboarded into the same hole without failing the build.
//
// Keyed "<tenant>/<role>" so a repair is retired one role at a time, and so
// onboarding a second tenant does not silently inherit the first's permission.
var tenantBridgeExempt = map[string]string{
	"acme/accounting": "#668 — accounting is pinned to __system__ by ACCOUNTING_TENANT and its pool is " +
		"bound to one tenant for the process lifetime (#97), so remedy 1 is the shape it needs. " +
		"Rendering it per tenant lands the WRITE side only: the api-gateway dials one address, " +
		"accounting.kanz-services.svc, which is the __system__ instance — so the tenant's ledger " +
		"would exist and no read route would reach it. That is not a regression (the reads are " +
		"empty today) but it is a decision, and #668 carries it.",
	"acme/audit": "#668 — audit is the one role here that is genuinely multi-tenant in code " +
		"(internal/audit/projector.go takes TenantID off the envelope), so remedy 2 is the honest " +
		"one for it: __system__ importing the tenant's FACTs under a tenant.<t>. PREFIX, with a " +
		"tenant.> stream and an AUDIT_SUBJECTS change. Importing them unprefixed instead would " +
		"deliver them to the platform archiver (NACKs forever) and platform accounting (#223).",
	"acme/risk-engine": "#668 — same tenant-pinned shape as accounting, plus two kinds tenantgen " +
		"refuses today: risk-engine deploys as an Argo Rollout with a canary analysis step and a KEDA " +
		"ScaledObject, and the analysis template selects on `app: risk-engine`, which every tenant's " +
		"rollout would share.",
}

func TestEveryRenderedTenantCanSendItsFactsBack(t *testing.T) {
	root := moduleRoot(t)
	text := tenancyText(t)
	blocks := accountBlocks(t, text)
	if len(blocks) < 2 {
		t.Fatalf("parsed %d account block(s) from tenancy.yaml — this guard is blind", len(blocks))
	}
	system, ok := blocks["__system__"]
	if !ok {
		t.Fatal("no __system__ account block in tenancy.yaml — this guard is blind")
	}
	accountUsers := natsAccountUsers(t)

	tenants := renderedTenants(t, root)
	// NON-VACUITY. acme is rendered today. A scan that found no tenants would
	// pass however one-directional the bridge had become.
	if len(tenants) == 0 {
		t.Fatal("found no rendered tenants under infra/deploy/tenants/ — the scan is broken, or " +
			"MT-02 per-tenant compute has been withdrawn. If it was withdrawn, this guard and the " +
			"exemptions below should go with it.")
	}
	if len(factConsumerRoles) == 0 {
		t.Fatal("factConsumerRoles is empty — this guard would pass for a tenant with no path for anything")
	}

	var problems []string
	live := map[string]bool{} // exemption keys this run actually consulted

	for _, tenant := range tenants {
		block, known := blocks[tenant]
		if !known {
			problems = append(problems, tenant+": infra/deploy/tenants/"+tenant+"/ renders compute for "+
				"this tenant, but tenancy.yaml declares no account by that name. Its workloads "+
				"authenticate and map to NO account, so they can neither publish nor subscribe at all.")
			continue
		}

		// REMEDY 2, measured once per tenant: the return path is two halves and
		// BOTH are required — an export from the tenant account, and an import in
		// __system__ naming that tenant. Either alone resolves to nothing: the
		// broker accepts the config and delivers no message, which is the failure
		// mode this whole file is about.
		exported := strings.Contains(block, "exports:") &&
			strings.Contains(system, "imports:") &&
			strings.Contains(system, "tenant."+tenant+".")

		for _, role := range factConsumerRoles {
			key := tenant + "/" + role.Name
			live[key] = true
			reason, exempt := tenantBridgeExempt[key]

			// REMEDY 1: the role runs inside this tenant's account. THREE halves,
			// and each fails differently: tenantgen must DECLARE it, a manifest
			// must EXIST for this tenant (infra/deploy/ is what deploys — a
			// declaration with no committed file runs nothing), and the account
			// must ADMIT the SVID that manifest renders (a pod whose SVID the
			// account does not list authenticates into NO account and consumes
			// nothing — SEC-M3). Checking only the account entry would leave this
			// guard green with the manifest deleted.
			var rendered bool
			if svc, declared := tenantgen.ServiceByName(role.Name); declared {
				_, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(svc.ManifestPath(tenant))))
				rendered = statErr == nil && accountUsers[tenant][svc.ComputeSPIFFEID(tenant)]
			}

			if rendered || exported {
				if exempt {
					how := "runs inside the " + tenant + " account"
					if !rendered {
						how = "reaches __system__ through the account's export"
					}
					problems = append(problems, "the exemption for "+key+" is DEAD: "+role.Name+" now "+
						how+", so the return path exists and the entry only hides this role from the "+
						"guard.\n      reason on file: "+reason)
				}
				continue
			}
			if exempt {
				continue
			}
			problems = append(problems, key+": compute is rendered for this tenant but its FACTs cannot "+
				"reach "+role.Name+" — "+role.Name+" is not rendered into the "+tenant+" account "+
				"(internal/tenantgen.Services), and the account exports nothing that __system__ imports.\n"+
				"      consequence: "+role.Loses+".\n"+
				"      and it is SILENT: a tenant that produced no events and a tenant whose events "+
				"cannot arrive are the same observable state, so every service stays Ready throughout.")
		}
	}

	// DEAD ENTRIES: an exemption for a tenant that is no longer rendered, or for
	// a role no longer in factConsumerRoles, is stale permission — and the next
	// reader takes it for a decision.
	for key := range tenantBridgeExempt {
		if !live[key] {
			problems = append(problems, "the exemption for "+key+" is DEAD: this run never consulted it. "+
				"Either no compute is rendered for that tenant under infra/deploy/tenants/ any more, or "+
				"that role is no longer in factConsumerRoles.")
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%d rendered tenant/role pair(s) whose FACTs reach nothing:\n\n  %s",
			len(problems), strings.Join(problems, "\n\n  "))
	}
}

// renderedTenants lists the tenants with per-tenant compute under
// infra/deploy/tenants/, which is what MT-02 renders and what
// tenant_compute_test.go already asserts has not drifted from the base manifest.
func renderedTenants(t *testing.T, root string) []string {
	t.Helper()
	dir := filepath.Join(root, "infra", "deploy", "tenants")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read infra/deploy/tenants: %v", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}
