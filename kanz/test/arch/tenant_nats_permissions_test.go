package arch

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/eighred/kanz/internal/tenantgen"
)

// A TENANT USER'S PERMISSIONS ARE DERIVED FROM THE PLATFORM'S, AND NOTHING
// CHECKED THAT THEY STILL ARE (#676).
//
// # What was guarded, and what was not
//
// TestServicePublishesOnlySubjectsItsTenancyPermissionsAllow derives each
// service's PUBLISH set from its Go source and fails the build when tenancy.yaml
// does not allow a subject the code names. It reads __system__ ONLY:
// servicePublishPermissions is built on systemUserEntries, which isolates the
// __system__ account by brace depth and, by construction, cannot see a tenant
// account. TestEveryNATSUserCarriesPermissions (#630) does read every account,
// but it asks a presence question — does this user have a boundary at all — and
// answers nothing about WHAT that boundary allows.
//
// So the acme account's three users carried hand-written allow-lists that no
// guard compared to anything. tenancy.yaml states, three times in its own
// commentary, that they are DERIVED: "The allow-lists MIRROR the platform
// risk-engine's entry above", "THE ALLOW-LISTS ARE THE PLATFORM OMS's, AND THAT
// IS NOT A GUESS", "THE ALLOW-LISTS ARE THE PLATFORM ARCHIVER'S, DERIVED NOT
// COPIED". Prose in a config file is a claim, not a mechanism; this guard is the
// mechanism.
//
// # Why the mirror is the right expectation, and not a convenience
//
// MT-02 per-tenant compute renders the tenant's pod from the SAME base manifest
// and the same digest-pinned image as the platform deployment
// (internal/tenantgen.Render; test/arch/tenant_compute_test.go diffs the whole
// rendered document byte-for-byte). Same binary, same defaults, same subjects.
// And the account's imports remap tenant.<t>.order.* onto the LOGICAL subject
// before any workload sees it, so the tenant pod subscribes exactly the strings
// the platform pod does — no tenant-scoped copy of the subject set exists to
// diverge from.
//
// Equality against the platform entry therefore makes the tenant permissions
// CODE-DERIVED transitively: the platform entry is checked against the service's
// source by the guard named above, and this one checks the tenant entry against
// the platform entry. Drift on either side fails a build.
//
// # Why this failure mode is worse in a tenant account than in __system__
//
// A NATS identity with the wrong permissions authenticates FINE and fails on the
// first event, as a permissions violation that reads like a broken feature
// rather than a missing grant (#630 is the same shape one layer up: users that
// were unrestricted in their account looked perfectly healthy). The tenant path
// adds two things that keep it hidden longer — nobody exercises it on a laptop,
// and CI's test/backing/nats-dev.conf rewrites tenant.* subjects at ingress, so
// a tenant-routed subscription can pass locally and time out in CI for a reason
// that looks unrelated to permissions.
//
// It is also duplicated per onboarding: acme is the template every future tenant
// is provisioned from, which is precisely how three users came to be
// unrestricted at once in #630.
//
// # What this guard does NOT do
//
// It does not decide which services a tenant gets — that is
// internal/tenantgen.Services, and test/arch/tenant_bridge_parity_test.go is
// where a missing consumer role is argued. It does not check that a granted
// subject is reachable inside the account (tenant_bridge_test.go owns the
// import set). It compares the four permission arrays of a tenant user against
// the platform user it is derived from, and nothing else.
func TestEveryTenantNATSUserMirrorsItsPlatformPermissions(t *testing.T) {
	accounts := natsAccountUserEntries(t)

	system, ok := accounts["__system__"]
	if !ok || len(system) == 0 {
		t.Fatal("parsed no __system__ users from tenancy.yaml's tenants.conf — the account walk in " +
			"natsAccountUserEntries is broken, and every comparison below would be vacuous")
	}
	// THE TWO WALKERS MUST AGREE ABOUT __system__. This guard compares a tenant
	// entry parsed by natsAccountUserEntries against a platform entry parsed the
	// same way; systemUserEntries is the OTHER reader of the same file, and the
	// permission guards depend on it. If the two ever see a different set of
	// users, one of them has silently stopped tracking the file's shape — and
	// whichever it is, an equality check between them is no longer trustworthy.
	existing := systemUserEntries(t, filepath.Join(moduleRoot(t), "infra", "nats", "tenancy.yaml"))
	if missing := svidsMissingFrom(existing, system); len(missing) > 0 {
		t.Fatalf("natsAccountUserEntries and systemUserEntries disagree about __system__: %s "+
			"seen by one walker and not the other. tenancy.yaml's shape changed under one of them; "+
			"the permission guards that read systemUserEntries are no longer reliable either",
			strings.Join(missing, ", "))
	}

	var tenants []string
	for name := range accounts {
		if !isPlatformNATSAccount(name) {
			tenants = append(tenants, name)
		}
	}
	sort.Strings(tenants)
	// NON-VACUITY (accounts). acme exists today. A walk that found no tenant
	// account would report every tenant permission correct, however wrong the
	// file had become — the exact false green this package has paid for before.
	if len(tenants) == 0 {
		t.Fatal("found no tenant accounts in tenancy.yaml (every account was filtered out as " +
			"platform) — the account walk or isPlatformNATSAccount is broken and this guard proves " +
			"nothing. If MT-01c tenant accounts were genuinely withdrawn, delete this guard rather " +
			"than letting it pass empty.")
	}

	var problems []string
	compared := 0
	live := map[string]bool{}

	for _, tenant := range tenants {
		users := accounts[tenant]
		if len(users) == 0 {
			problems = append(problems, tenant+": the account declares no users at all, so no workload "+
				"can map into it — every pod presenting a "+tenant+" SVID authenticates and reaches NO "+
				"account (SEC-M3). Either the account is dead and should be deleted, or the walk is broken.")
			continue
		}
		var svids []string
		for svid := range users {
			svids = append(svids, svid)
		}
		sort.Strings(svids)

		for _, svid := range svids {
			service, platformSVID, why := platformCounterpartSVID(tenant, svid)
			if why != "" {
				problems = append(problems, tenant+": "+svid+" — "+why)
				continue
			}
			key := tenant + "/" + service
			live[key] = true
			platformEntry, known := system[platformSVID]
			if !known {
				if _, exempt := tenantOnlyNATSUserExempt[key]; exempt {
					continue
				}
				problems = append(problems, tenant+"/"+service+": the tenant user "+svid+" has no "+
					"platform counterpart ("+platformSVID+" is not a __system__ user), so its allow-lists "+
					"are derived from nothing and no guard checks them against the service's code. "+
					"tenancy.yaml's own commentary claims every tenant entry mirrors a platform one; "+
					"either add the platform entry or exempt this pair with the reason.")
				continue
			}
			if reason, exempt := tenantOnlyNATSUserExempt[key]; exempt {
				problems = append(problems, "the tenant-only exemption for "+key+" is DEAD: a platform "+
					"counterpart now exists, so compare the permissions normally and delete the exemption.\n"+
					"      reason on file: "+reason)
				continue
			}

			diffs := permissionDiffs(users[svid], platformEntry)
			if reason, exempt := tenantPermissionParityExempt[key]; exempt {
				if len(diffs) == 0 {
					problems = append(problems, "the exemption for "+key+" is DEAD: the tenant user now "+
						"mirrors "+platformSVID+" exactly, so the entry only hides this pair from the "+
						"guard. Delete it.\n      reason on file: "+reason)
				}
				continue
			}

			compared++
			if len(diffs) > 0 {
				problems = append(problems, key+": the tenant user "+svid+" no longer mirrors its "+
					"platform counterpart "+platformSVID+", which tenancy.yaml states it is derived "+
					"from:\n        "+strings.Join(diffs, "\n        ")+
					"\n      consequence: the tenant pod runs the SAME binary as the platform one "+
					"(internal/tenantgen renders it from the same base, same digest), so a subject the "+
					"platform grant carries and this one does not is one the pod WILL name — and the "+
					"broker denies it at runtime, after a clean authentication, with the pod Ready. A "+
					"grant this one carries and the platform's does not is authority no code asked for, "+
					"inside the account that carries this tenant's order flow.")
			}
		}
	}

	// DEAD EXEMPTIONS: an entry for a tenant or service this run never reached is
	// stale permission, and the next reader takes it for a decision.
	for key := range tenantPermissionParityExempt {
		if !live[key] {
			problems = append(problems, "the exemption for "+key+" is DEAD: this run never consulted "+
				"it. Either that tenant account is gone from tenancy.yaml, or it no longer carries a "+
				"user for that service.")
		}
	}
	for key := range tenantOnlyNATSUserExempt {
		if !live[key] {
			problems = append(problems, "the tenant-only exemption for "+key+" is DEAD: this run never "+
				"consulted it. Delete the stale identity decision or restore the tenant user it names.")
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%d tenant NATS permission problem(s):\n\n  %s", len(problems), strings.Join(problems, "\n\n  "))
	}
	// NON-VACUITY (comparisons). The two arms above catch a walk that found no
	// accounts; this one catches a run that found them and still asserted
	// nothing — every tenant user exempted, which is a guard in name only.
	if compared == 0 {
		t.Fatal("compared zero tenant users against a platform counterpart — every tenant user was " +
			"exempted or matched nothing, so this guard is asserting nothing at all")
	}
}

// tenantPermissionParityExempt names "<tenant>/<service>" pairs whose tenant
// user deliberately does NOT mirror its platform counterpart, with the issue
// that retires each one.
//
// DEFAULT-DENY, AND EMPTY IS THE CORRECT STATE. Every tenant user in
// tenancy.yaml today is an exact mirror, which is what makes the platform
// entries' code derivation reach the tenant accounts at all. An entry here
// severs that chain for one pair, so it must say which subjects differ, why the
// tenant's needs are genuinely not the platform's, and what closes the gap — not
// merely that they differ. The dead-entry arm above deletes it the moment the
// pair mirrors again.
var tenantPermissionParityExempt = map[string]string{}

// tenantOnlyNATSUserExempt is for a workload that intentionally has no
// __system__ deployment and therefore no platform permission set to mirror.
// Each entry must name the issue and explain why adding a platform identity
// would be excess authority. The loop above rejects the exemption the moment a
// counterpart appears and the dead-entry arm rejects a vanished tenant user.
var tenantOnlyNATSUserExempt = map[string]string{}

// permissionDiffs returns a human-readable difference per permission array
// between a tenant user's entry text and its platform counterpart's. An empty
// result means the two grant exactly the same thing.
//
// Both sides are parsed by the SAME two functions the __system__ permission
// guards use (parsePublishPerm / parseSubscribePerm), so a change in how this
// file is read moves both sides of the comparison together and can never
// manufacture a difference that is not in the config.
//
// DENY IS COMPARED TOO, not only allow. archiver's retired `publish: { deny:
// [">"] }` is why: a deny list is the only thing in this file that actually
// restricts, `allow: []` having been proven a no-op, so a tenant copy that lost
// one would be strictly MORE permissive while looking identical in review.
func permissionDiffs(tenantEntry, platformEntry string) []string {
	tp, pp := parsePublishPerm(tenantEntry), parsePublishPerm(platformEntry)
	ts, ps := parseSubscribePerm(tenantEntry), parseSubscribePerm(platformEntry)

	var out []string
	for _, c := range []struct {
		what             string
		tenant, platform []string
	}{
		{"publish.allow", tp.allow, pp.allow},
		{"publish.deny", tp.deny, pp.deny},
		{"subscribe.allow", ts.allow, ps.allow},
		{"subscribe.deny", ts.deny, ps.deny},
	} {
		onlyTenant := setDifference(c.tenant, c.platform)
		onlyPlatform := setDifference(c.platform, c.tenant)
		if len(onlyTenant) == 0 && len(onlyPlatform) == 0 {
			continue
		}
		var parts []string
		if len(onlyPlatform) > 0 {
			parts = append(parts, "MISSING from the tenant: "+strings.Join(onlyPlatform, ", "))
		}
		if len(onlyTenant) > 0 {
			parts = append(parts, "EXTRA in the tenant: "+strings.Join(onlyTenant, ", "))
		}
		out = append(out, c.what+" — "+strings.Join(parts, "; "))
	}
	return out
}

func setDifference(a, b []string) []string {
	in := map[string]bool{}
	for _, s := range b {
		in[s] = true
	}
	var out []string
	seen := map[string]bool{}
	for _, s := range a {
		if !in[s] && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func svidsMissingFrom(a map[string]string, b map[string]string) []string {
	var out []string
	for svid := range a {
		if _, ok := b[svid]; !ok {
			out = append(out, svid+" (systemUserEntries only)")
		}
	}
	for svid := range b {
		if _, ok := a[svid]; !ok {
			out = append(out, svid+" (natsAccountUserEntries only)")
		}
	}
	sort.Strings(out)
	return out
}

var spiffeWorkload = regexp.MustCompile(`^spiffe://kanz\.internal/ns/([^/]+)/sa/([^/]+)$`)

// platformCounterpartSVID maps a user in tenant's account to the __system__ user
// its permissions are derived from, returning the service name, that SVID, and —
// when the SVID fits neither shape — why.
//
// THERE ARE EXACTLY TWO SHAPES, and the difference is not cosmetic:
//
//   - MT-02 COMPUTE, `ns/kanz-services/sa/<service>-<tenant>`. The pod is a
//     name-suffixed ServiceAccount in the SHARED kanz-services namespace, not in
//     tenant-<t> (infra/deploy/tenants/README.md). The service must be DECLARED
//     in internal/tenantgen.Services and the SVID must equal what
//     Service.ComputeSPIFFEID renders — asking the renderer rather than
//     re-deriving the string here is what stops this guard and the generator
//     from agreeing on a name neither actually produces.
//
//   - THE TENANT'S OWN WORKLOADS, `ns/tenant-<tenant>/sa/<service>`, minted into
//     the tenant's own namespace by infra/tenancy/tenantctl.sh. acme's
//     risk-engine is this shape.
//
// Anything else authenticates into an account it was not designed for, or into
// none at all, so it is reported rather than skipped: a user this function
// cannot classify is a user nothing compares.
func platformCounterpartSVID(tenant, svid string) (service, platformSVID, why string) {
	m := spiffeWorkload.FindStringSubmatch(svid)
	if m == nil {
		return "", "", "this is not a kanz.internal workload SVID " +
			"(spiffe://kanz.internal/ns/<namespace>/sa/<name>), so no platform entry can be matched to it"
	}
	ns, name := m[1], m[2]

	switch {
	case ns == "tenant-"+tenant:
		service = name
	case ns == "kanz-services":
		base := strings.TrimSuffix(name, "-"+tenant)
		if base == name {
			return "", "", "it sits in the shared kanz-services namespace but its ServiceAccount is not " +
				"suffixed with this tenant (" + name + "), so it is a PLATFORM identity admitted to a " +
				"tenant account — that pod would read this tenant's subjects while pinned to __system__ " +
				"by its own *_TENANT env var (#223)"
		}
		svc, declared := tenantgen.ServiceByName(base)
		if !declared {
			return "", "", "MT-02 compute for a service internal/tenantgen does not declare (" + base +
				"). Nothing renders a manifest for it, so the account admits an identity no pod presents — " +
				"and the permissions below are derived from nothing"
		}
		if want := svc.ComputeSPIFFEID(tenant); want != svid {
			return "", "", "MT-02 compute whose SVID is not the one internal/tenantgen renders for it " +
				"(want " + want + "). The pod authenticates and maps to NO account (SEC-M3)"
		}
		service = base
	default:
		return "", "", "its namespace (" + ns + ") is neither this tenant's own (tenant-" + tenant +
			") nor the shared kanz-services namespace MT-02 compute is rendered into, so nothing in " +
			"this repository is known to present it"
	}
	return service, "spiffe://kanz.internal/ns/kanz-services/sa/" + service, ""
}

// natsAccountUserEntries returns, for EVERY account in tenancy.yaml, each user's
// own entry text keyed by SVID — the account-aware twin of systemUserEntries,
// which by design sees only __system__.
//
// It is a separate function rather than a widening of systemUserEntries for the
// reason nats_every_user_is_restricted_test.go already states: the __system__
// isolation those permission guards depend on must not become a parameter, or a
// caller can accidentally ask "what may this service publish" of the wrong
// account. The __system__ agreement check at the top of the test above is what
// keeps the two honest with each other.
//
// The ConfigMap is unmarshalled as real YAML to reach data["tenants.conf"] (the
// same read tenant_compute_test.go's natsAccountUsers does), and comments are
// stripped from that value BEFORE any brace is counted. Both steps are load
// bearing: the file argues about permissions in prose at length and quotes NATS
// config shapes — `permissions: { publish: { allow: [] } }` — inside comments,
// and counting those braces corrupts the depth tracking that finds the entries.
// Three guards in this package have passed while the thing they checked was
// deleted, because a scan matched their own commentary.
func natsAccountUserEntries(t *testing.T) map[string]map[string]string {
	t.Helper()
	path := filepath.Join(moduleRoot(t), "infra", "nats", "tenancy.yaml")
	var cm tenantConfigMap
	if err := yaml.Unmarshal([]byte(readFile(t, path)), &cm); err != nil {
		t.Fatalf("parse %s as YAML: %v", path, err)
	}
	conf, ok := cm.Data["tenants.conf"]
	if !ok {
		t.Fatalf(`%s has no data["tenants.conf"] key — has the ConfigMap shape changed?`, path)
	}
	conf = stripYAMLComments(strings.ReplaceAll(conf, "\r\n", "\n"))

	idx := strings.Index(conf, "accounts")
	if idx < 0 {
		t.Fatalf("%s: tenants.conf has no `accounts` block — has the format changed?", path)
	}
	open := strings.IndexByte(conf[idx:], '{')
	if open < 0 {
		t.Fatalf("%s: the `accounts` block has no opening brace — has the format changed?", path)
	}

	out := map[string]map[string]string{}
	account := ""
	entryStart := -1
	depth := 1 // positioned just past `accounts {`
	for i := idx + open + 1; i < len(conf); i++ {
		switch conf[i] {
		case '{':
			depth++
			switch depth {
			case 2:
				account = trailingIdentifier(conf[:i])
				if out[account] == nil {
					out[account] = map[string]string{}
				}
			case 3:
				entryStart = i
			}
		case '}':
			if depth == 3 && entryStart >= 0 {
				entry := conf[entryStart : i+1]
				if m := userLine.FindStringSubmatch(entry); m != nil && account != "" {
					out[account][m[1]] = entry
				}
				entryStart = -1
			}
			depth--
			if depth == 1 {
				account = ""
			}
			if depth == 0 {
				return out // the `accounts` block's own closing brace
			}
		}
	}
	t.Fatalf("%s: the `accounts` block never closes — the brace walk ran off the end of the file", path)
	return nil
}

var trailingIdentifierRe = regexp.MustCompile(`([A-Za-z0-9_$]+)\s*$`)

// trailingIdentifier returns the bare identifier immediately preceding an
// opening brace — the account name in `acme {`. Returns "" when the brace is not
// preceded by one, which keeps an anonymous block from being recorded as an
// account whose users would then be compared against the wrong thing.
func trailingIdentifier(before string) string {
	m := trailingIdentifierRe.FindStringSubmatch(before)
	if m == nil {
		return ""
	}
	return m[1]
}
