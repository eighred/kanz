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
func TestNoLaterMigrationReintroducesMissingOK(t *testing.T) {
	root := moduleRoot(t)
	bad := regexp.MustCompile(`current_setting\(\s*'app\.tenant_id'\s*,\s*true\s*\)`)

	guardVersion := map[string]int{}
	err := filepath.WalkDir(filepath.Join(root, "services"), walkSQL(func(path string, body []byte) {
		if strings.Contains(filepath.Base(path), "tenant_scope_required") {
			guardVersion[serviceOf(root, path)] = migrationVersion(path)
		}
	}))
	if err != nil {
		t.Fatal(err)
	}

	var offenders []string
	err = filepath.WalkDir(filepath.Join(root, "services"), walkSQL(func(path string, body []byte) {
		svc := serviceOf(root, path)
		guard, guarded := guardVersion[svc]
		if !guarded || migrationVersion(path) <= guard || !bad.Match(body) {
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
