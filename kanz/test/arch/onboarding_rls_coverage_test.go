package arch

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Onboarding RLS COVERAGE.
//
// TestProvisionTenantStorageTablesMatchMigrations proves the script's table
// lists agree with the migrations — but only for the two clusters someone typed
// into serviceMigrationsRelPath, and its regex is literally
// `(kanz-risk|kanz-books)`. NINE services declare FORCE ROW LEVEL SECURITY. So
// the guard could never see the other seven, and the script verified 5 tables
// out of ~15 while step 1 announced isolation for the tenant as a whole.
//
// That is under-coverage the operator cannot see, which is the actual defect:
// a tenant is handed over as "isolation holds" with most of the tenant-scoped
// estate never confirmed live.
//
// Closing it fully needs ONBOARD-M6 — the repo gives several answers for which
// DATABASE each service's tables live in, and this script's own header refuses
// to guess a database name, because a wrong guess reports a HEALTHY cluster as
// "MT-01d not deployed". So the gap cannot simply be filled by extending the
// case statement.
//
// What CAN be closed now is the SILENCE. Every tenant-scoped table must be
// either VERIFIED by the script or DECLARED unverified with a reason — the same
// shape as the archiver's unbackedByDesign map. A new service that adds an
// RLS'd table then fails this test until someone triages it, instead of
// silently widening the unchecked surface.

// allTenantScopedServices maps every service that declares FORCE ROW LEVEL
// SECURITY to its migrations directory. Derived from the migrations, not from
// what the script happens to name.
var allTenantScopedServices = map[string]string{
	"accounting":    "services/accounting/migrations",
	"alternatives":  "services/alternatives/migrations",
	"datamaster":    "services/datamaster/migrations",
	"oms":           "services/oms/migrations",
	"risk-engine":   "services/risk-engine/migrations",
	"tv-sync":       "services/tv-sync/migrations",
	"venue-binance": "services/venue-binance/migrations",
	"venue-okx":     "services/venue-okx/migrations",
	"wealth":        "services/wealth/migrations",
}

// unverifiedDeclPattern matches the script's declaration of the tables step 1
// knowingly does NOT check, e.g.
//
//	UNVERIFIED_RLS_TABLES="oms:orders,positions tv-sync:tv_facts"
var unverifiedDeclPattern = regexp.MustCompile(`UNVERIFIED_RLS_TABLES="([^"]*)"`)

// scriptUnverifiedTables returns the set of tables the script DECLARES it does
// not verify.
func scriptUnverifiedTables(content string) map[string]bool {
	out := map[string]bool{}
	m := unverifiedDeclPattern.FindStringSubmatch(content)
	if m == nil {
		return out
	}
	for _, group := range strings.Fields(m[1]) {
		service, list, found := strings.Cut(group, ":")
		if !found {
			continue
		}
		for _, tb := range strings.Split(list, ",") {
			if tb = strings.TrimSpace(tb); tb != "" {
				// SERVICE-QUALIFIED on purpose: `positions` exists in BOTH
				// risk-engine and oms, so a bare table name would let one
				// service's verified table silently vouch for another's
				// unchecked one — the exact hole this guard exists to close.
				out[service+"."+tb] = true
			}
		}
	}
	return out
}

// verifiedClusterService maps a cluster arm in step 1's case statement to the
// service whose migrations declare those tables.
var verifiedClusterService = map[string]string{
	"kanz-risk":  "risk-engine",
	"kanz-books": "accounting",
}

// Every FORCE-RLS table in every service is either verified by step 1 or
// declared unverified with a reason. Silence is the one thing not allowed.
func TestProvisionTenantAccountsForEveryTenantScopedTable(t *testing.T) {
	root := moduleRoot(t)
	content := readOnboardingScript(t, root, provisionTenantRelPath)

	verified := map[string]bool{}
	for cluster, tables := range scriptForceRLSTables(content) {
		service, ok := verifiedClusterService[cluster]
		if !ok {
			t.Fatalf("step 1 verifies cluster %q but this guard does not know which service's "+
				"migrations declare its tables — map it in verifiedClusterService", cluster)
		}
		for _, tb := range tables {
			verified[service+"."+tb] = true
		}
	}
	// NON-VACUITY: if the script names nothing, every table below would look
	// "unaccounted" for the wrong reason, and a script that named nothing at all
	// must not be able to satisfy this test by declaring everything unverified.
	if len(verified) == 0 {
		t.Fatal("provision-tenant.sh declares no verified FORCE-RLS tables at all — step 1 checks nothing")
	}
	declared := scriptUnverifiedTables(content)

	services := make([]string, 0, len(allTenantScopedServices))
	for s := range allTenantScopedServices {
		services = append(services, s)
	}
	sort.Strings(services)

	var unaccounted []string
	for _, service := range services {
		migrated := migrationForceRLSTables(t, root, allTenantScopedServices[service])
		if len(migrated) == 0 {
			t.Fatalf("no FORCE ROW LEVEL SECURITY table parsed under %s — the migrations changed shape "+
				"and this guard silently stopped covering %q", allTenantScopedServices[service], service)
		}
		for _, tb := range migrated {
			if key := service + "." + tb; !verified[key] && !declared[key] {
				unaccounted = append(unaccounted, service+"."+tb)
			}
		}
	}

	if len(unaccounted) > 0 {
		sort.Strings(unaccounted)
		t.Fatalf("provision-tenant.sh neither verifies nor DECLARES these tenant-scoped tables: %v\n"+
			"Step 1 tells the operator isolation is active while never checking them. Either add the "+
			"table to a verified cluster arm, or list it in UNVERIFIED_RLS_TABLES with the reason "+
			"(ONBOARD-M6: the database name is unresolved and this script refuses to guess one). "+
			"An unchecked table must be a DECLARED gap, never a silent one.", unaccounted)
	}
}
