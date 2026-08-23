package arch

import (
	"os"
	"path/filepath"
	"regexp"
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
	// RemedyTwo states WHY this role can be satisfied by an export/import
	// bridge, or is EMPTY when it cannot. Empty is the default and the safe one:
	// only a consumer that is genuinely multi-tenant IN CODE can read a
	// tenant-prefixed subject, and everything else must run inside the account.
	//
	// A STRING RATHER THAN A BOOL, deliberately. "audit: true" invites the next
	// reader to write "accounting: true" and move on; a sentence has to be
	// composed, and composing it is where somebody notices that accounting's pool
	// is bound to one tenant for the process lifetime.
	RemedyTwo string
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
		RemedyTwo: "audit is the ONLY role here that is multi-tenant in code: " +
			"internal/audit/projector.go takes TenantID off the ENVELOPE rather than from a " +
			"*_TENANT env var, and audit_log is deliberately not RLS'd because it IS the " +
			"cross-tenant record. So a subject arriving under a tenant.<t>. prefix is one it folds " +
			"correctly. Its default subject set is \">\" (config.DefaultSubjects), which is what " +
			"makes the prefixed space reachable without narrowing anything.",
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
	// RETIRED (#668). accounting now runs INSIDE the acme account — remedy 1,
	// the only one open to it: it is pinned to one tenant by ACCOUNTING_TENANT
	// and internal/pg.NewTenantPool (#97) and subscribes the LOGICAL subject
	// names, so the tfact.<tenant>. bridge below reaches it as nothing at all.
	// internal/tenantgen renders it, the manifest is committed under
	// infra/deploy/tenants/acme/, and tenancy.yaml admits accounting-acme's SVID;
	// this guard checks all three, and its dead-entry arm failed the build until
	// this entry was deleted.
	//
	// RETIRED (#668) — acme/audit, by the same mechanism. Its entry read "remedy 2
	// is the honest one for it: __system__ importing the tenant's FACTs under a
	// tenant.<t>. PREFIX, with a tenant.> stream and an AUDIT_SUBJECTS change".
	// All three landed — under `tfact.<t>.` rather than `tenant.<t>.`, because the
	// inbound command bridge already owns tenant.*.order.> and two streams may not
	// claim overlapping subjects. Neither exemption could be forgotten.
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

		// REMEDY 2: the return path is two halves and BOTH are required — an
		// export from the tenant account, and an import in __system__ that names
		// that account AND lands its subjects under a tenant prefix. Either alone
		// resolves to nothing: the broker accepts the config and delivers no
		// message, which is the failure mode this whole file is about.
		//
		// THIS USED TO BE THREE strings.Contains CALLS, and it was wrong in both
		// directions at once — measured, with the natural config written out:
		//
		//	prefix: "tenant.acme"              -> NOT detected. That is the NATS
		//	                                      syntax for exactly what remedy 2
		//	                                      prescribes (the broker appends the
		//	                                      dot), so the bridge could be built
		//	                                      correctly and this guard would still
		//	                                      demand an exemption for it.
		//	subject: "tenant.acme.accounting.>" -> detected, and credited to EVERY
		//	                                      role, because the old check ran once
		//	                                      per tenant. An import carrying only
		//	                                      audit's traffic marked accounting and
		//	                                      risk-engine repaired.
		//
		// The second is the dangerous one: it turns this guard green on the exact
		// state it exists to report.
		exported := systemImportsTenantUnderPrefix(system, block, tenant)

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

			// REMEDY 2 IS CREDITED ONLY TO A ROLE THAT CAN HONESTLY USE IT.
			// This file's header has always said so — "only honest for a consumer
			// that is genuinely multi-tenant in code" — and then credited the
			// export to every role in the list. A tenant-pinned consumer cannot
			// read a prefixed subject: accounting and risk-engine subscribe the
			// LOGICAL names and are bound to one tenant by *_TENANT and
			// internal/pg.NewTenantPool, so tenant.acme.accounting.> reaches them
			// as nothing at all. Saying otherwise is how this guard would report a
			// tenant's ledger repaired by an import that only carries audit's
			// traffic.
			viaExport := exported && role.RemedyTwo != ""
			if rendered || viaExport {
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
			why := "and the account exports nothing that __system__ imports under a tenant prefix"
			if exported && role.RemedyTwo == "" {
				why = "and although " + tenant + " DOES export to __system__ under a tenant prefix, that " +
					"bridge cannot serve this role: it is tenant-pinned (a *_TENANT env var plus " +
					"internal/pg.NewTenantPool, #97) and subscribes the LOGICAL subject names, so a " +
					"tenant-prefixed subject reaches it as nothing. It needs remedy 1"
			}
			problems = append(problems, key+": compute is rendered for this tenant but its FACTs cannot "+
				"reach "+role.Name+" — "+role.Name+" is not rendered into the "+tenant+" account "+
				"(internal/tenantgen.Services), "+why+".\n"+
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

// importEntry matches ONE entry inside an imports: [ ... ] list. NATS writes
// these on a single line in this file, e.g.
//
//	{ stream: { account: acme, subject: "accounting.>" }, prefix: "tenant.acme" }
//
// so an entry is everything between a `{ stream:` and the matching close. The
// pattern is deliberately loose about whitespace and key order, and strict about
// the two things that decide whether the bridge works: WHICH ACCOUNT the subject
// comes from, and WHERE it lands.
var importEntry = regexp.MustCompile(`\{\s*stream:\s*\{[^}]*\}[^}]*\}`)

var (
	importAccount = regexp.MustCompile(`account:\s*"?([A-Za-z0-9_.-]+)"?`)
	importPrefix  = regexp.MustCompile(`prefix:\s*"([^"]*)"`)
	importSubject = regexp.MustCompile(`subject:\s*"([^"]*)"`)
)

// systemImportsTenantUnderPrefix reports whether __system__ imports FACTs from
// the tenant's own account and lands them under that tenant's prefix.
//
// # Both halves, and the prefix, are load-bearing
//
// An export with no matching import — or an import with no export behind it —
// resolves to nothing: the broker accepts the config and delivers no message,
// which is the silent shape this whole file exists to report.
//
// The PREFIX is the third requirement and the one with teeth. Importing a
// tenant's FACTs unprefixed would deliver them onto the logical subject names
// that __system__'s OWN consumers subscribe: the platform archiver would NACK
// forever (internal/topic.For refuses an envelope whose tenant_id is not its
// own) and platform accounting would fold another tenant's event into
// __system__'s book (#223). So an unprefixed import is worse than no import,
// and must not read as a repair here.
//
// # Why both spellings are accepted
//
// NATS expresses "land it under a prefix" two ways, and the estate uses both:
// `prefix: "tenant.acme"` (the broker appends the dot) and an explicitly
// prefixed `subject: "tenant.acme.…"`. The check this replaced looked for the
// literal `tenant.acme.` anywhere in the __system__ block, which recognised the
// second and MISSED THE FIRST — the natural way to write exactly what remedy 2
// prescribes.
func systemImportsTenantUnderPrefix(system, tenantBlock, tenant string) bool {
	// The export half. Without it the import below names a subject the tenant
	// account never offers, and the broker is content to deliver nothing.
	if !strings.Contains(tenantBlock, "exports:") {
		return false
	}
	for _, entry := range importEntry.FindAllString(system, -1) {
		acct := importAccount.FindStringSubmatch(entry)
		if acct == nil || acct[1] != tenant {
			continue
		}
		// TENANT-SCOPED, NOT A PARTICULAR PREFIX WORD. What matters is that the
		// subjects land somewhere scoped to THIS tenant rather than on the
		// logical names __system__'s own consumers subscribe — the unprefixed
		// import that is worse than none. Which prefix family carries them is a
		// topology choice, and this guard was written when only `tenant.` existed:
		// it hardcoded that word, so the OUTBOUND bridge landing under `tfact.`
		// (#668 — the inbound command bridge already owns `tenant.*.order.>`, and
		// two streams may not claim overlapping subjects) read as no bridge at all.
		//
		// The rule is therefore structural: the prefix's LAST token names the
		// tenant. `tenant.acme` and `tfact.acme` both qualify; a bare `acme` does
		// too; an absent prefix does not, and neither does one naming a different
		// tenant.
		if p := importPrefix.FindStringSubmatch(entry); p != nil && lastToken(p[1]) == tenant {
			return true
		}
		if s := importSubject.FindStringSubmatch(entry); s != nil {
			if toks := strings.Split(s[1], "."); len(toks) > 2 && toks[1] == tenant {
				return true
			}
		}
	}
	return false
}

// lastToken returns the final dot-separated token of a NATS subject prefix, or
// "" for an empty one. `tfact.acme` -> `acme`.
func lastToken(prefix string) string {
	if prefix == "" {
		return ""
	}
	toks := strings.Split(prefix, ".")
	return toks[len(toks)-1]
}
