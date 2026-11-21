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
// Exceptions retire when RLS is introduced. Routing directories must remain
// tenant-ID-only with FORCE-RLS payload and delivery tables. Authentication and
// WORM/query authorization are also exercised by their owning runtime tests;
// this source guard does not replace restricted-role database verification.
var rlsExempt = map[string]string{
	"custody_assignment_readers":    "#1074: owner provisions reader enrollment; other logins can see only their own enrollment.",
	"custody_proof_issuers":         "#1074: owner provisions issuers; lookup requires the issuer login or an enrolled reader for the current tenant.",
	"custody_proof_scopes":          "#1074: owner provisions scopes; lookup requires the issuer login or an enrolled reader for the current tenant.",
	"identity_access_audit_tenants": "#1292: routing directory contains only tenant IDs; payload and pending records remain FORCE RLS scoped, with no directory API.",
	"bff_session_audit_tenants":     "#1292: routing directory contains only tenant IDs; payload and pending records remain FORCE RLS scoped, with no directory API.",
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
	// enforced at the query API. This exception also REQUIRES database WORM
	// protection against the runtime principal. The row trigger proves only
	// UPDATE/DELETE refusal; #1747 owns the credential/TRUNCATE boundary, and
	// #1748 owns retention/restore decisions. Passing source checks here do not
	// certify those unresolved premises or authorize cross-tenant callers.
	"audit_log": "AUDIT-01a/b decision: cross-tenant compliance record scoped at the query API; required WORM boundary unverified (#1747), retention/restore decision pending (#1748)",
}

var (
	// CREATE TABLE <name> ( ... — the name may be schema-qualified or quoted.
	createTable = regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?"?([a-z_][a-z0-9_]*)"?\s*\(`)
	// ALTER TABLE <name> ENABLE|FORCE ROW LEVEL SECURITY — the direct form.
	alterRLS        = regexp.MustCompile(`(?is)ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?(?:ONLY\s+)?(?:"?public"?\s*\.\s*)?"?([a-z_][a-z0-9_]*)"?\s+(ENABLE|DISABLE|FORCE|NO\s+FORCE)\s+ROW\s+LEVEL\s+SECURITY`)
	policyPredicate = regexp.MustCompile(`(?i)\b(?:USING|WITH\s+CHECK)\s*\(`)
	// Migrations format one statement for each table in a literal array.
	foreachTables = regexp.MustCompile(`(?is)\bFOREACH\s+(\w+)\s+IN\s+ARRAY\s+ARRAY\s*\[([^\]]+)\]\s+LOOP\b`)
	loopToken     = regexp.MustCompile(`(?i)\b(?:END\s+)?LOOP\b`)
	formatTable   = regexp.MustCompile(`(?is)\bEXECUTE\s+format\(\s*(?:'((?:''|[^'])*)'|\$f\$(.*?)\$f\$)\s*,\s*(\w+)\s*\)\s*;`)
	rlsTemplate   = regexp.MustCompile(`(?is)^\s*(?:ALTER\s+TABLE\s+%I\s+(?:ENABLE|DISABLE|(?:NO\s+)?FORCE)\s+ROW\s+LEVEL\s+SECURITY|(?:CREATE|DROP)\s+POLICY\s+\w+\s+ON\s+%I)\b`)
)

// tableDecl is one CREATE TABLE and what the same migration set says about it.
type tableDecl struct {
	policies    map[string]string
	file        string
	definition  string
	altered     bool
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
			if problem := rlsExemptionProblem(name, d, decls); problem != "" {
				problems = append(problems, name+": "+problem)
			}
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

// The directory exceptions admit routing metadata, never subject names or audit
// payloads. A new column, a later ALTER, or a companion losing tenant isolation
// invalidates that permanent premise even while the directory still exists.
func rlsExemptionProblem(name string, d *tableDecl, decls map[string]*tableDecl) string {
	// 0030 keeps the three provisioning registries writable by their owner.
	// All other roles have precisely one SELECT policy; the proof and
	// assignment tables themselves still go through the generic FORCE check.
	var readerPolicy string
	switch name {
	case "custody_assignment_readers":
		readerPolicy = "FOR SELECT USING(login_role=session_user)"
	case "custody_proof_issuers", "custody_proof_scopes":
		readerPolicy = "FOR SELECT USING(tenant_id=app_current_tenant() AND (login_role=session_user OR EXISTS(" +
			"SELECT FROM custody_assignment_readers r WHERE r.login_role=session_user AND r.tenant_id=" + name + ".tenant_id)))"
	}
	if readerPolicy != "" {
		if d.file != "services/accounting/migrations/0030_custody_assignments.sql" || !d.enabled || d.forced || len(d.policies) != 1 {
			return "custody provisioning registry requires owner access and a single reader policy under enabled RLS"
		}
		for _, body := range d.policies {
			normalize := func(sql string) string { return strings.Join(strings.Fields(strings.ToLower(sql)), "") }
			if normalize(body) != normalize(readerPolicy) {
				return "custody provisioning registry must restrict lookups to its authorized session login"
			}
		}
		return ""
	}
	if d.enabled || d.forced || d.policy {
		return "RLS exemption is stale: table now declares RLS or a policy; review and retire the exception"
	}
	if !strings.HasSuffix(name, "_audit_tenants") {
		return ""
	}
	definition := strings.Join(strings.Fields(strings.ToLower(strings.Trim(d.definition, "() \t\r\n"))), " ")
	if definition != "tenant_id text primary key" || d.altered {
		return "routing directory exception requires exactly tenant_id TEXT PRIMARY KEY and no later ALTER"
	}
	payload := strings.TrimSuffix(name, "_tenants")
	for _, table := range []string{payload, payload + "_pending"} {
		companion := decls[table]
		if companion == nil || !companion.enabled || !companion.forced || !companion.policy || !companion.guarded {
			return "routing directory exception requires FORCE RLS and scoped policies on " + table
		}
	}
	return ""
}

// scanMigrationsForTenantTables reads every service's migrations and returns what
// each CREATE TABLE declares about itself.
//
// IN MIGRATION ORDER. os.ReadDir sorts by name and the files are NNNN_-prefixed,
// so reading them in that order is reading them in the order Postgres applied
// them. CREATE and DROP are folded by policy name so every surviving policy
// is checked; a later scoped policy cannot hide an earlier permissive one.
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
	d := &tableDecl{file: file, policies: map[string]string{}}
	out[name] = d
	return d
}

func scanOneMigration(out map[string]*tableDecl, file, sql string) {
	// CREATE TABLE bodies: a table is tenant-scoped when its own definition
	// carries a tenant_id column. Read from the parenthesised body rather than
	// from the whole file, so a later reference to some other table's tenant_id
	// cannot mark this one.
	for _, m := range createTable.FindAllStringSubmatchIndex(sql, -1) {
		name := strings.ToLower(sql[m[2]:m[3]])
		body := balancedParen(sql[m[1]-1:])
		d := decl(out, name, file)
		if d.definition != "" {
			d.definition += " "
		}
		d.definition += body
		if tenantColumn(body) {
			d.tenantScope = true
		}
	}
	// A later schema change can invalidate a directory's single-column premise
	// without changing its original CREATE TABLE. Other ALTER forms stay outside
	// this narrow premise; any ADD/DROP/ALTER/RENAME of its columns needs review.
	columnChanges := regexp.MustCompile(`(?is)ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?(?:ONLY\s+)?(?:"?[a-z_][a-z0-9_]*"?\s*\.\s*)?"?([a-z_][a-z0-9_]*)"?\s*\*?\s+(?:ADD|DROP|ALTER|RENAME)\b`)
	for _, m := range columnChanges.FindAllStringSubmatch(sql, -1) {
		decl(out, strings.ToLower(m[1]), file).altered = true
	}

	sql = expandTableRLSTemplates(sql)
	for _, m := range alterRLS.FindAllStringSubmatch(sql, -1) {
		d := decl(out, strings.ToLower(m[1]), file)
		switch strings.ToUpper(m[2]) {
		case "ENABLE":
			d.enabled = true
		case "DISABLE":
			d.enabled = false
		case "FORCE":
			d.forced = true
		default:
			d.forced = false
		}
	}

	// Fold policy creation and removal in statement order. Permissive policies
	// combine with OR: a later narrow policy cannot hide an earlier broad one.
	changes := regexp.MustCompile(`(?is)(CREATE|DROP|ALTER)\s+POLICY\s+(?:IF\s+EXISTS\s+)?"?([a-z_][a-z0-9_]*)"?\s+ON\s+(?:"?public"?\s*\.\s*)?"?([a-z_][a-z0-9_]*)"?([^;]*);`)
	for _, m := range changes.FindAllStringSubmatch(sql, -1) {
		d := decl(out, strings.ToLower(m[3]), file)
		name := strings.ToLower(m[2])
		switch strings.ToUpper(m[1]) {
		case "DROP":
			delete(d.policies, name)
		case "CREATE":
			d.policies[name] = m[4]
		case "ALTER":
			// ALTER may replace just one clause. Require review before
			// attributing the old definition to the resulting policy.
			d.policies[name] = ""
		}
	}
	for _, d := range out {
		d.policy = len(d.policies) > 0
		d.guarded = d.policy
		for _, body := range d.policies {
			d.guarded = d.guarded && policyScoped(body)
		}
	}
}

// tenantColumn reports whether a CREATE TABLE body declares a tenant_id column.
func tenantColumn(body string) bool {
	// Column declarations are separated by top-level commas. A tenant_id in
	// a nested key or foreign-key column list does not declare a column.
	start, depth := 1, 0
	for i := 1; i < len(body); i++ {
		switch body[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
				continue
			}
			fallthrough
		case ',':
			if depth == 0 {
				fields := strings.Fields(body[start:i])
				if len(fields) >= 2 && strings.EqualFold(strings.Trim(fields[0], `"`), "tenant_id") {
					return true
				}
				start = i + 1
			}
		}
	}
	return false
}

// Expand only the RLS statements formatted with the current FOREACH variable.
// A trigger loop in the same migration supplies no policy or FORCE evidence.
func expandTableRLSTemplates(sql string) string {
	var out strings.Builder
	for {
		m := foreachTables.FindStringSubmatchIndex(sql)
		if m == nil {
			out.WriteString(sql)
			return out.String()
		}
		variable, tables := sql[m[2]:m[3]], sql[m[4]:m[5]]
		out.WriteString(sql[:m[1]])
		sql = sql[m[1]:]
		end, depth := 0, 1
		for _, token := range loopToken.FindAllStringIndex(sql, -1) {
			if strings.EqualFold(sql[token[0]:token[1]], "loop") {
				depth++
			} else {
				depth--
			}
			if depth == 0 {
				end = token[1]
				break
			}
		}
		body := formatTable.ReplaceAllStringFunc(sql[:end], func(call string) string {
			args := formatTable.FindStringSubmatch(call)
			stmt := strings.ReplaceAll(args[1], "''", "'") + args[2]
			if !strings.EqualFold(args[3], variable) || !rlsTemplate.MatchString(stmt) || strings.Count(stmt, "%I") != 1 {
				return call
			}
			var statements strings.Builder
			for _, table := range arrayLiteralElemPattern.FindAllStringSubmatch(tables, -1) {
				statements.WriteString(strings.Replace(stmt, "%I", table[1], 1) + ";\n")
			}
			return statements.String()
		})
		out.WriteString(body)
		sql = sql[end:]
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
	// Skip each complete clause before looking for the next one. JOIN USING
	// inside a WITH CHECK expression belongs to that expression.
	found := false
	for {
		m := policyPredicate.FindStringIndex(body)
		if m == nil {
			return found
		}
		expression := balancedParen(body[m[1]-1:])
		if !referencesTenantGUC(expression) {
			return false
		}
		found = true
		body = body[m[1]-1+len(expression):]
	}
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
