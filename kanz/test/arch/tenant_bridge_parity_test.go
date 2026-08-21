package arch

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A TENANT WITH COMPUTE MUST HAVE A WAY TO SEND ITS FACTS BACK (#637).
//
// # What went wrong without it
//
// The MT-02 bridge carries order COMMANDS from __system__ into a tenant's NATS
// account. Nothing carries anything back. tenancy.yaml contains exactly ONE
// exports: block (in __system__) and ONE imports: block (in acme) — there is no
// export from a tenant account and no import in __system__.
//
// NATS accounts are isolated by construction; the file's own header says so
// ("An account's subjects are physically invisible to other accounts"). So every
// subject oms-acme publishes is invisible outside acme, and no platform service
// is a member of that account: its users list is the tenant's risk-engine and
// the tenant's OMS, while accounting, risk-engine, audit, archiver, lake-sink,
// lineage, compliance, tv-sync, market-data and both venue adapters are all
// __system__ users, each pinned to that tenant literally by a *_TENANT env var.
//
// So a provisioned tenant gets orders IN and nothing OUT. Its OMS accepts an
// order, writes its rows, and publishes the accepted FACT, the fills and
// risk.position.changed into an account consumed by nobody. That tenant has:
//
//   - no book of record — accounting posts no ledger entry, so NAV, cash and the
//     buying-power gate are computed over an empty book
//   - no risk — risk-engine holds no positions
//   - no audit trail — for a trade that happened
//   - no archive and no DR — the archiver refuses the envelope at topic.For
//
// and every one of those services is healthy and Ready throughout, because "this
// tenant produced no events" and "this tenant's events cannot reach me" are the
// same observable state. It also collides with the one sequencing rule CLAUDE.md
// says outlives any issue: a tenant provisioned today can place orders whose
// FACTs are, by construction, outside the archival path.
//
// THIS IS NOT A LEAK, AND THE DISTINCTION MATTERS. Isolation holds in both
// directions — no tenant sees another's events, and a tenant sees nothing of its
// own either. The boundary is not leaking; it is half-built, and the half that
// is built is the one that lets capital move.
//
// # What this guard does, and what it deliberately does not
//
// It does NOT fix the architecture. Which direction that goes — exporting the
// FACT subjects back to __system__, or rendering the consuming services per
// tenant the way oms-acme is — is an estate-shape decision that collides with
// #97's ruling that a pool is bound to one tenant for the process lifetime, and
// it belongs in its own issue rather than inside a guard.
//
// What it does is stop the gap SPREADING. Today it is one tenant; the defect
// duplicates itself once per onboarding, silently, and the exemption below is
// the only place that says so. Tenant number two fails the build.
func TestEveryRenderedTenantCanSendItsFactsBack(t *testing.T) {
	root := moduleRoot(t)
	text := tenancyText(t)
	accounts := accountBlocks(t, text)
	if len(accounts) < 2 {
		t.Fatalf("parsed %d account block(s) from tenancy.yaml — this guard is blind", len(accounts))
	}
	system, ok := accounts["__system__"]
	if !ok {
		t.Fatal("no __system__ account block in tenancy.yaml — this guard is blind")
	}

	tenants := renderedTenants(t, root)
	// NON-VACUITY. acme is rendered today. A scan that found no tenants would
	// pass however one-directional the bridge had become.
	if len(tenants) == 0 {
		t.Fatal("found no rendered tenants under infra/deploy/tenants/ — the scan is broken, or " +
			"MT-02 per-tenant compute has been withdrawn. If it was withdrawn, this guard and the " +
			"exemption below should go with it.")
	}

	var problems []string
	for _, tenant := range tenants {
		reason, exempt := tenantBridgeExempt[tenant]

		block, known := accounts[tenant]
		if !known {
			problems = append(problems, tenant+": infra/deploy/tenants/"+tenant+"/ renders compute for "+
				"this tenant, but tenancy.yaml declares no account by that name. Its workloads "+
				"authenticate and map to NO account, so they can neither publish nor subscribe at all.")
			continue
		}

		// The return path is two halves and BOTH are required: an export from the
		// tenant account, and an import in __system__ naming that tenant. Either
		// alone resolves to nothing — the broker accepts the config and delivers
		// no message, which is the failure mode this whole file is about.
		hasExport := strings.Contains(block, "exports:")
		hasImport := strings.Contains(system, "tenant."+tenant+".") &&
			strings.Contains(system, "imports:")

		if hasExport && hasImport {
			if exempt {
				problems = append(problems, "the exemption for "+tenant+" is DEAD: that account now "+
					"exports its FACTs and __system__ imports them, so the return path exists and the "+
					"entry only hides this tenant from the guard.\n      reason on file: "+reason)
			}
			continue
		}
		if exempt {
			continue
		}
		missing := []string{}
		if !hasExport {
			missing = append(missing, "the "+tenant+" account has no exports: block")
		}
		if !hasImport {
			missing = append(missing, "__system__ imports nothing from tenant."+tenant+".")
		}
		problems = append(problems, tenant+": compute is rendered for this tenant but its FACTs can "+
			"reach nothing — "+strings.Join(missing, ", ")+". Its OMS will accept orders and publish "+
			"the accepted FACT, the fills and risk.position.changed into an account no platform "+
			"service is a member of: no ledger entry, no positions, no audit trail, no Kafka archive "+
			"— and every one of those services Ready throughout, because a tenant that produced no "+
			"events and a tenant whose events cannot arrive are the same observable state.")
	}

	// DEAD ENTRIES: an exemption for a tenant that is no longer rendered is stale
	// permission, and the next reader takes it for a decision.
	present := map[string]bool{}
	for _, tn := range tenants {
		present[tn] = true
	}
	for tenant := range tenantBridgeExempt {
		if !present[tenant] {
			problems = append(problems, "the exemption for "+tenant+" is DEAD: no compute is rendered "+
				"for that tenant under infra/deploy/tenants/ any more")
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%d rendered tenant(s) whose FACTs reach nothing:\n\n  %s",
			len(problems), strings.Join(problems, "\n\n  "))
	}
}

// tenantBridgeExempt names rendered tenants whose FACTs are known to reach
// nothing, with the issue that resolves it.
//
// DEFAULT-DENY, AND THIS ENTRY IS A DEBT RECORD RATHER THAN A DECISION. The
// arrangement it describes is broken, not chosen — it is listed so main stays
// green while the state is stated in exactly one place, and so the SECOND tenant
// cannot be onboarded into the same hole without failing the build. That is the
// property worth having: the defect currently duplicates itself once per
// onboarding and nothing anywhere says so.
var tenantBridgeExempt = map[string]string{
	"acme": "THE RETURN PATH DOES NOT EXIST YET — #637. acme is the only rendered tenant and its " +
		"FACTs reach no platform service. Resolving it is an estate-shape decision, not a manifest " +
		"edit: exporting the FACT subjects back to __system__ is not sufficient on its own, because " +
		"the consuming services are pinned to __system__ by a *_TENANT env var and internal/topic.For " +
		"refuses an envelope whose tenant_id differs — so each would have to become multi-tenant, " +
		"which re-opens #97's ruling that a pool is bound to one tenant for the process lifetime. " +
		"The alternative is rendering those services per tenant the way oms-acme is. #637 carries " +
		"that choice; this entry retires when it lands.",
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
