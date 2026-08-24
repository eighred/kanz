package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// EVERY TENANT-SCOPED TABLE IS UNDER ROW-LEVEL SECURITY, OR IT IS NAMED (#89).
//
// #89's second clause is a coverage claim: "onboarding verified RLS on 5 tables
// out of ~15, and the silence is closed but the COVERAGE is not." The five are
// checked by infra/onboarding/provision-tenant.sh against a running estate; the
// rest were asserted by convention.
//
// # Why the migrations are the oracle and the database is not
//
// The obvious test enumerates tables carrying a tenant_id column from a live
// database. It is the wrong oracle here, twice over:
//
//   - the suite's own fixtures create tenant-scoped tables (scope_probe,
//     and applySchema's stand-ins for portfolios/positions), so a live scan
//     grades the test harness alongside the estate;
//   - CI applies ONE service's migrations before the Go job, so most of the
//     estate's tables are simply absent there. A guard that skips what it cannot
//     find reports full coverage over whatever happens to exist, which is the
//     shape of the hole this issue is about.
//
// The migrations are the declaration. A table that carries tenant_id in a
// CREATE TABLE is tenant-scoped by construction, and the same migration set is
// where RLS is turned on. Both are literal text, both are in git, and neither
// depends on which database anybody ran what against.
//
// # THE TWO FORMS ARE BOTH REAL AND BOTH PARSED
//
//	ALTER TABLE fund_events ENABLE ROW LEVEL SECURITY;          -- direct
//
//	FOREACH t IN ARRAY ARRAY['ledger_entries', 'ledger_snapshots'] LOOP
//	  EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
//
// A guard that read only the first would report accounting and datamaster as
// uncovered while they are the most carefully covered of the set. The array is a
// literal, so the loop is as parseable as the statement.
//
// # WHAT IS CHECKED, AND WHY FORCE IS NOT OPTIONAL
//
// ENABLE alone is not isolation. The table's OWNER bypasses a policy unless
// FORCE is also set — and migrations run as the owner, as do several services in
// a small deployment. provision-tenant.sh checks relforcerowsecurity for exactly
// that reason, and its comment says a bare count "cannot say WHICH table lost
// FORCE RLS". So both are required here, per table, by name.
//
// A POLICY IS REQUIRED TOO, and its predicate must reference the session GUC. A
// table with RLS enabled and no policy denies everything (an outage, but not a
// leak); a policy of USING (true) is the leak, and it passes any check that only
// counts policies.

// rlsExempt is a DEFAULT-DENY allow-list: every tenant-scoped table declared in
// the migrations is checked unless it is named here with a reason.
//
// THREE ENTRIES, AND ALL THREE WERE ALREADY DECIDED — in a comment at the top of
// their own migration, where nothing checked that the comment still described the
// file. That is the change this guard makes to them: the reason stays where it
// was written, and the exemption now has to be restated here, so a table that
// quietly loses its reason is a red build rather than a paragraph nobody re-read.
var rlsExempt = map[string]string{
	// #364. "Login must find an account BEFORE it knows which tenant that account
	// belongs to. A tenant-scoped pool binds app.tenant_id once at connect, so a
	// credential lookup through one could only ever find users of whichever tenant
	// the pool was opened for — which is every tenant except the one signing in."
	//
	// THE ISOLATION IS NOT WEAKENED, IT STARTS ONE STEP LATER: identity_users.
	// tenant_id becomes the principal's tenant claim, and every downstream store is
	// RLS-scoped by it. The control on the unscoped access itself is
	// internal/pg.NewGlobalPool, which refuses to open without a written reason.
	"identity_users":   "#364 — login resolves a credential before the tenant is known; scoped at the token instead",
	"identity_invites": "#364 — redemption resolves an invite before the tenant is known; same seam as identity_users",

	// AUDIT-01a/b. "The audit log is a cross-cutting compliance record that an
	// auditor reads ACROSS tenants; tenant_id is a column for filtering/reporting,
	// not an isolation boundary here" — the same call market-data and
	// schema-registry make for universal data. Tenant-scoped read access is
	// enforced at the query API, and the table is WORM at the database layer: a
	// trigger makes UPDATE and DELETE raise, so it is insert-only even to a
	// compromised app role.
	"audit_log": "AUDIT-01a/b — a cross-tenant compliance record read by auditors; scoped at the query API, WORM in the database",
}

var (
	// CREATE TABLE <name> ( ... — the name may be schema-qualified or quoted.
	createTable = regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?"?([a-z_][a-z0-9_]*)"?\s*\(`)
	// ALTER TABLE <name> ENABLE|FORCE ROW LEVEL SECURITY — the direct form.
	alterRLS = regexp.MustCompile(`(?is)ALTER\s+TABLE\s+"?([a-z_][a-z0-9_]*)"?\s+(ENABLE|FORCE)\s+ROW\s+LEVEL\s+SECURITY`)
	// ARRAY['a', 'b'] — the dynamic form's table list.
	arrayList = regexp.MustCompile(`(?is)ARRAY\s*\[([^\]]*)\]`)
	// CREATE POLICY <name> ON <table>
	createPolicy = regexp.MustCompile(`(?is)CREATE\s+POLICY\s+[a-z_][a-z0-9_]*\s+ON\s+(?:%I|"?([a-z_][a-z0-9_]*)"?)`)
	quoted       = regexp.MustCompile(`'([^']*)'`)
)

// tableDecl is one CREATE TABLE and what the same migration set says about it.
type tableDecl struct {
	file        string
	tenantScope bool
	enabled     bool
	forced      bool
	policy      bool
	guarded     bool // a policy predicate that references the tenant GUC
}

func TestEveryTenantScopedTableIsUnderForcedRLS(t *testing.T) {
	root := moduleRoot(t)
	decls := scanMigrationsForTenantTables(t, root)

	// NON-VACUITY. A moved migrations tree or a broken regexp would leave this
	// guard grading an empty set and passing — the failure it exists to prevent,
	// one level up. #89 says "~15"; anything far below that means the scan broke.
	scoped := 0
	for _, d := range decls {
		if d.tenantScope {
			scoped++
		}
	}
	if scoped < 10 {
		t.Fatalf("found only %d tenant-scoped tables across services/*/migrations — #89 counts "+
			"about fifteen, so this scan is broken and is asserting almost nothing", scoped)
	}

	var problems []string
	seenExempt := map[string]bool{}
	names := make([]string, 0, len(decls))
	for name := range decls {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		d := decls[name]
		if !d.tenantScope {
			continue
		}
		if _, ok := rlsExempt[name]; ok {
			seenExempt[name] = true
			continue
		}
		switch {
		case !d.enabled:
			problems = append(problems, name+" ("+d.file+"): carries tenant_id and RLS is never ENABLED — "+
				"every tenant reads every other tenant's rows")
		case !d.forced:
			problems = append(problems, name+" ("+d.file+"): RLS is enabled and NOT FORCED — the table "+
				"owner bypasses the policy, and migrations plus several services connect as the owner")
		case !d.policy:
			problems = append(problems, name+" ("+d.file+"): RLS is on with NO POLICY — the table denies "+
				"everything, which is an outage rather than a leak, but it is not isolation either")
		case !d.guarded:
			problems = append(problems, name+" ("+d.file+"): its policy predicate does not reference "+
				"the session tenant (app.tenant_id / app_current_tenant()) — a policy that does not "+
				"name the session's tenant is not scoping to it")
		}
	}

	if len(problems) > 0 {
		t.Errorf("%d tenant-scoped table(s) are not under forced row-level security:\n  %s\n\n"+
			"MT-01d is the whole multi-tenancy guarantee: a shared database in which one customer's "+
			"connection cannot read another's rows. A table that carries tenant_id and no policy is "+
			"not a smaller version of that — it is the guarantee not holding, for that table, "+
			"silently, until somebody queries it (#89).",
			len(problems), strings.Join(problems, "\n  "))
	}

	// DEAD-ENTRY ARM, the same shape every exemption map in this directory has.
	for name := range rlsExempt {
		if !seenExempt[name] {
			t.Errorf("rlsExempt names %q, which is not a tenant-scoped table in any migration — it "+
				"was renamed, dropped, or its tenant_id column went away. Delete the entry.", name)
		}
	}
}

// scanMigrationsForTenantTables reads every service's migrations and returns what
// each CREATE TABLE declares about itself.
//
// IN MIGRATION ORDER. os.ReadDir sorts by name and the files are NNNN_-prefixed,
// so reading them in that order is reading them in the order Postgres applied
// them — which is what makes "the last policy definition" the one that is
// actually in force.
func scanMigrationsForTenantTables(t *testing.T, root string) map[string]*tableDecl {
	t.Helper()
	out := map[string]*tableDecl{}

	services, err := os.ReadDir(filepath.Join(root, "services"))
	if err != nil {
		t.Fatalf("read services/: %v", err)
	}
	for _, svc := range services {
		if !svc.IsDir() {
			continue
		}
		dir := filepath.Join(root, "services", svc.Name(), "migrations")
		files, err := os.ReadDir(dir)
		if err != nil {
			continue // a service with no migrations
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".sql") {
				continue
			}
			rel := filepath.ToSlash(filepath.Join("services", svc.Name(), "migrations", f.Name()))
			body, err := os.ReadFile(filepath.Join(dir, f.Name()))
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			scanOneMigration(out, rel, stripSQLComments(string(body)))
		}
	}
	return out
}

func decl(out map[string]*tableDecl, name, file string) *tableDecl {
	if d, ok := out[name]; ok {
		return d
	}
	d := &tableDecl{file: file}
	out[name] = d
	return d
}

func scanOneMigration(out map[string]*tableDecl, file, sql string) {
	// CREATE TABLE bodies: a table is tenant-scoped when its own definition
	// carries a tenant_id column. Read from the parenthesised body rather than
	// from the whole file, so a later reference to some other table's tenant_id
	// cannot mark this one.
	for _, m := range createTable.FindAllStringSubmatchIndex(sql, -1) {
		name := sql[m[2]:m[3]]
		body := balancedParen(sql[m[1]-1:])
		d := decl(out, name, file)
		if tenantColumn(body) {
			d.tenantScope = true
		}
	}

	// The direct form.
	for _, m := range alterRLS.FindAllStringSubmatch(sql, -1) {
		d := decl(out, m[1], file)
		if strings.EqualFold(m[2], "ENABLE") {
			d.enabled = true
		} else {
			d.forced = true
		}
	}

	// The dynamic form: every table named in an ARRAY[...] that the same file
	// then ALTERs through format(%I). The array is a literal, so the loop is as
	// readable as the statement — and reading only the direct form would report
	// the two most carefully covered services as uncovered.
	if strings.Contains(sql, "%I") {
		enable := strings.Contains(strings.ToUpper(sql), "ENABLE ROW LEVEL SECURITY")
		force := strings.Contains(strings.ToUpper(sql), "FORCE  ROW LEVEL SECURITY") ||
			strings.Contains(strings.ToUpper(sql), "FORCE ROW LEVEL SECURITY")
		policy := strings.Contains(strings.ToUpper(sql), "CREATE POLICY")
		// THE PREDICATE IS READ FROM THE POLICY, NOT FROM THE FILE.
		//
		// Asking whether the FILE mentions the tenant GUC was the first version of
		// this, and it was wrong in the direction that matters: 0002_tenant_scope_
		// required.sql defines app_current_tenant() and raises on an unset GUC, so
		// it mentions app.tenant_id a dozen times — and a CREATE POLICY of
		// USING (true) inside it would have been vouched for by its own error
		// message. Proven by mutation, which is the only reason it is not still
		// written that way.
		guarded := false
		for _, body := range dynamicPolicyBodies(sql) {
			if policyScoped(body) {
				guarded = true
			}
		}
		for _, a := range arrayList.FindAllStringSubmatch(sql, -1) {
			for _, q := range quoted.FindAllStringSubmatch(a[1], -1) {
				name := strings.TrimSpace(q[1])
				if name == "" {
					continue
				}
				d := decl(out, name, file)
				d.enabled = d.enabled || enable
				d.forced = d.forced || force
				// LAST DEFINITION WINS, because migrations are sequential and a later
				// CREATE POLICY supersedes an earlier one. OR-ing these was the second
				// mistake this guard made: it asked whether the table had EVER been
				// given a scoped policy, so widening the current one to USING (true)
				// was vouched for by the definition it replaced. Both mutations
				// survived until this line changed.
				if policy {
					d.policy = true
					d.guarded = guarded
				}
			}
		}
	}

	// CREATE POLICY, direct form. The predicate must name the session GUC: a
	// policy of USING (true) is the leak that counting policies cannot see.
	for _, m := range createPolicy.FindAllStringSubmatchIndex(sql, -1) {
		if m[2] < 0 {
			continue // the %I form, handled above
		}
		name := sql[m[2]:m[3]]
		d := decl(out, name, file)
		d.policy = true
		// Last definition wins — see the dynamic branch above for why.
		d.guarded = policyScoped(policyBody(sql[m[1]:]))
	}
}

// tenantColumn reports whether a CREATE TABLE body declares a tenant_id column.
func tenantColumn(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		f := strings.Fields(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), ",")))
		if len(f) > 0 && strings.Trim(f[0], `"`) == "tenant_id" {
			return true
		}
	}
	return false
}

// policyBody returns the text up to the statement terminator, so one policy's
// predicate cannot be read off the next one.
func policyBody(s string) string {
	if i := strings.Index(s, ";"); i >= 0 {
		return s[:i]
	}
	return s
}

// dynamicPolicyBodies returns the text of each CREATE POLICY ... ON %I statement
// in a migration, so its predicate can be read on its own rather than through
// whatever else the file happens to say.
//
// The statement is wrapped in a dollar-quoted string that the loop passes to
// format(), so it ends at the closing tag rather than at a semicolon — the
// predicate itself contains none.
func dynamicPolicyBodies(sql string) []string {
	var out []string
	for i := 0; ; {
		j := strings.Index(sql[i:], "CREATE POLICY")
		if j < 0 {
			return out
		}
		start := i + j
		rest := sql[start:]
		end := len(rest)
		for _, term := range []string{"$f$", "$$", ";"} {
			if k := strings.Index(rest, term); k >= 0 && k < end {
				end = k
			}
		}
		out = append(out, rest[:end])
		i = start + len("CREATE POLICY")
	}
}

// policyScoped reports whether a policy body scopes BOTH of its clauses to the
// session's tenant.
//
// CLAUSE BY CLAUSE, NOT "does the body mention it somewhere". USING governs what
// a tenant can READ and WITH CHECK what it can WRITE, and they fail differently:
// a permissive USING is the cross-tenant read — the leak — while a permissive
// WITH CHECK lets one tenant plant rows in another's scope. Asking whether the
// body mentioned the GUC anywhere passed a policy whose USING had been widened to
// (true) while its WITH CHECK still named the tenant, which is a total read leak
// wearing half a control. That version survived its mutation; this one does not.
func policyScoped(body string) bool {
	using, hasUsing := policyClause(body, "USING")
	check, hasCheck := policyClause(body, "WITH CHECK")
	if !hasUsing && !hasCheck {
		return false
	}
	if hasUsing && !referencesTenantGUC(using) {
		return false
	}
	if hasCheck && !referencesTenantGUC(check) {
		return false
	}
	return true
}

// policyClause returns the parenthesised expression following keyword.
func policyClause(body, keyword string) (string, bool) {
	upper := strings.ToUpper(body)
	i := strings.Index(upper, keyword)
	if i < 0 {
		return "", false
	}
	rest := body[i+len(keyword):]
	j := strings.Index(rest, "(")
	if j < 0 {
		return "", false
	}
	return balancedParen(rest[j:]), true
}

// referencesTenantGUC reports whether a policy predicate scopes to the session's
// tenant.
//
// TWO SPELLINGS, BOTH REAL AND BOTH CORRECT. accounting writes
// current_setting('app.tenant_id', true) inline; datamaster and the tenant-scope
// migration wrap it as app_current_tenant(), which RAISES when the GUC is unset
// rather than returning NULL — a stricter fail-closed. A guard that knew only
// one of them would report the stricter half of the estate as unprotected, which
// is the direction that gets a guard deleted rather than fixed.
func referencesTenantGUC(s string) bool {
	return strings.Contains(s, "app.tenant_id") || strings.Contains(s, "app_current_tenant")
}

// balancedParen returns the parenthesised block starting at s[0] == '('.
func balancedParen(s string) string {
	depth := 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[:i+1]
			}
		}
	}
	return s
}
