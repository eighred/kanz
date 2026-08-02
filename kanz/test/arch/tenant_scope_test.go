package arch

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// NO MIGRATION MAY READ THE TENANT WITH missing_ok (MT-01e).
//
// Isolation was written as
//
//	USING (tenant_id = current_setting('app.tenant_id', true))
//
// and that `true` is missing_ok: a session that never set the GUC gets NULL, the
// predicate goes NULL, and the query returns ZERO ROWS — silently. An unscoped read
// and a tenant with genuinely no data become the same observable event, and the
// dangerous one is the invisible one. accounting shipped exactly that and nobody
// noticed: a pool with no AfterConnect, a ledger that could not read or write a row,
// and a green suite throughout.
//
// app_current_tenant() RAISES instead. The historical migrations still CONTAIN the
// old text — they are checksummed and cannot be edited, and the tenant_scope_required
// migration supersedes their policies at run time. What must never happen is a
// migration added AFTER the guard that quietly goes back. So: per service, find the
// guard's version and audit everything that comes after it.
//
// THE ANCHOR IS THE FUNCTION, NOT THE FILENAME (#239). This used to record a service
// as guarded only if some migration's NAME contained "tenant_scope_required", which
// silently exempted every service that installs the raising function another way.
// tv-sync is exactly that service: it defines app_current_tenant() inline in
// 0001_fact_log.sql, so it satisfies the sibling guard below, had no entry in this
// map, and was skipped by the `!guarded` arm — permanently, and with nothing saying
// so. 9 services declare RLS and only 8 carry a *_tenant_scope_required.sql, so the
// gap was not hypothetical. A later tv-sync migration going back to missing_ok would
// have made an unscoped read return ZERO ROWS instead of erroring: the precise defect
// this guard's header says accounting shipped unnoticed, uncaught by the guard
// written to catch it.
//
// EARLIEST, NOT LATEST. A service is protected from the moment it HAS the raising
// function, so the anchor is the lowest version that mentions it — anything later
// going back is a regression regardless of which migration installed the function.
// (For oms and accounting the function appears in two migrations; taking the latest
// would stop auditing the one in between.)
//
// COMMENTS ARE STRIPPED, for the same reason test/arch/migration_table_discovery_test.go
// strips them: the tenant_scope_required migrations quote the offending predicate in
// prose to explain the defect, and now that those files can fall after their own
// service's anchor, a guard matching prose would fire on its own post-mortem and be
// deleted rather than fixed.
//
// KNOWN INTERACTION. A migration that REDEFINES app_current_tenant() will trip this,
// because the function's body legitimately reads the GUC with missing_ok before
// raising on NULL. That has not happened; if it ever should, the redefinition belongs
// in the migration that already owns the function, and if it genuinely cannot, this
// guard should learn about that case deliberately rather than have its matcher
// loosened.
func TestNoLaterMigrationReintroducesMissingOK(t *testing.T) {
	root := moduleRoot(t)
	bad := regexp.MustCompile(`current_setting\(\s*'app\.tenant_id'\s*,\s*true\s*\)`)

	guardVersion := map[string]int{}
	err := filepath.WalkDir(filepath.Join(root, "services"), walkSQL(func(path string, body []byte) {
		if !strings.Contains(stripSQLComments(string(body)), "app_current_tenant()") {
			return
		}
		svc, v := serviceOf(root, path), migrationVersion(path)
		if cur, seen := guardVersion[svc]; !seen || v < cur {
			guardVersion[svc] = v
		}
	}))
	if err != nil {
		t.Fatal(err)
	}

	// NON-VACUITY. Every arm below is keyed off this map: a service absent from it is
	// skipped outright, so an empty map means the guard audits NOTHING and reports
	// green — the state it spent its whole existence in for tv-sync. The sibling test
	// has carried this check; this one did not, which is why the gap survived.
	if len(guardVersion) == 0 {
		t.Fatal("no migration mentions app_current_tenant() — this guard is keyed off that " +
			"function, so it would pass no matter what any later migration does. Either the " +
			"migration layout moved, or the raising guard has been removed from the whole tree; " +
			"both mean unscoped reads silently return zero rows and nothing is checking.")
	}

	var offenders []string
	err = filepath.WalkDir(filepath.Join(root, "services"), walkSQL(func(path string, body []byte) {
		svc := serviceOf(root, path)
		guard, guarded := guardVersion[svc]
		if !guarded || migrationVersion(path) <= guard || !bad.MatchString(stripSQLComments(string(body))) {
			return
		}
		rel, _ := filepath.Rel(root, path)
		offenders = append(offenders, filepath.ToSlash(rel))
	}))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Fatalf("these migrations reintroduce missing_ok AFTER the tenant-scope guard:\n\n  %s\n\n"+
			"current_setting('app.tenant_id', true) returns NULL when the GUC is unset, so an UNSCOPED QUERY "+
			"RETURNS ZERO ROWS instead of failing. Use app_current_tenant(), which RAISES: an unscoped read must "+
			"be an error, never an empty answer.", strings.Join(offenders, "\n  "))
	}
	t.Logf("%d services anchored on app_current_tenant(); every later migration audited", len(guardVersion))
}

// migrationVersion reads the NNNN prefix of a migration filename.
func migrationVersion(path string) int {
	n := 0
	for _, r := range filepath.Base(path) {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// EVERY SERVICE WITH RLS MUST CARRY THE RAISING GUARD.
//
// A service that enables row-level security but never gets the tenant_scope_required
// migration keeps the old silent-empty behaviour, and nothing else would say so.
func TestEveryServiceWithRLSRequiresTheTenantScope(t *testing.T) {
	root := moduleRoot(t)

	withRLS := map[string]bool{}
	withGuard := map[string]bool{}
	err := filepath.WalkDir(filepath.Join(root, "services"), walkSQL(func(path string, body []byte) {
		svc := serviceOf(root, path)
		if strings.Contains(strings.ToUpper(string(body)), "ROW LEVEL SECURITY") {
			withRLS[svc] = true
		}
		if strings.Contains(string(body), "app_current_tenant()") {
			withGuard[svc] = true
		}
	}))
	if err != nil {
		t.Fatal(err)
	}

	var missing []string
	for svc := range withRLS {
		if !withGuard[svc] {
			missing = append(missing, svc)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("these services have RLS tables but no tenant-scope guard:\n\n  %s\n\n"+
			"Without app_current_tenant(), an unscoped query against their tables returns ZERO ROWS instead of "+
			"erroring — the failure is silent, and it looks exactly like a tenant with no data.",
			strings.Join(missing, "\n  "))
	}
	if len(withRLS) == 0 {
		t.Fatal("no RLS migrations found at all — this test would pass vacuously")
	}
	t.Logf("%d services enforce RLS, all guarded by app_current_tenant()", len(withRLS))
}
