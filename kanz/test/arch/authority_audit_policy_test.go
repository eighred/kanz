package arch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuthorityMigrationPolicyIsRemovedBeforeCommit(t *testing.T) {
	base := `CREATE TABLE evidence (
tenant_id TEXT NOT NULL
);
ALTER TABLE evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE evidence FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON evidence USING(tenant_id=app_current_tenant());`
	wide := `CREATE POLICY migration_only ON evidence FOR SELECT TO CURRENT_USER USING(true);`
	for _, tc := range []struct {
		name, sql string
		scoped    bool
	}{
		{"temporary migration", base + wide + `DROP POLICY migration_only ON evidence;`, true},
		{"forgotten migration policy", base + wide, false},
		{"later narrow policy cannot hide broad one", base + wide + `CREATE POLICY another ON evidence USING(tenant_id=app_current_tenant());`, false},
		{"missing runtime policy", base + `DROP POLICY tenant_scope ON evidence;`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tables := map[string]*tableDecl{}
			scanOneMigration(tables, "fixture.sql", tc.sql)
			if tables["evidence"].guarded != tc.scoped {
				t.Fatalf("wrong final policy state: %+v", tables["evidence"])
			}
		})
	}
}

func TestCustodyAuthorityRegistryGuard(t *testing.T) {
	const migration = "services/accounting/migrations/0030_custody_assignments.sql"
	src, err := os.ReadFile(filepath.Join(moduleRoot(t), migration))
	if err != nil {
		t.Fatal(err)
	}
	base := stripSQLComments(string(src))
	for _, tc := range []struct {
		name, table, change string
		valid               bool
	}{
		{"reader policy", "custody_assignment_readers", "", true},
		{"scope policy", "custody_proof_scopes", "", true},
		{"issuer policy", "custody_proof_issuers", "", true},
		{"reader RLS disabled", "custody_assignment_readers", "ALTER TABLE ONLY public.custody_assignment_readers DISABLE ROW LEVEL SECURITY;", false},
		{"scope reader condition removed", "custody_proof_scopes", "DROP POLICY assignment_scope_read ON custody_proof_scopes; CREATE POLICY assignment_scope_read ON custody_proof_scopes FOR SELECT USING(tenant_id=app_current_tenant());", false},
		{"reader enrollment changed", "custody_assignment_readers", "ALTER POLICY assignment_reader_self ON public.custody_assignment_readers USING(true);", false},
		{"issuer write policy", "custody_proof_issuers", "CREATE POLICY extra ON custody_proof_issuers FOR INSERT WITH CHECK(tenant_id=app_current_tenant());", false},
		{"provisioning disabled", "custody_proof_issuers", "ALTER TABLE custody_proof_issuers FORCE ROW LEVEL SECURITY;", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tables := map[string]*tableDecl{}
			scanOneMigration(tables, migration, base+tc.change)
			d := tables[tc.table]
			if d == nil || !d.tenantScope {
				t.Fatal("missing tenant-scoped declaration", tc.table)
			}
			if problem := rlsExemptionProblem(tc.table, d, tables); (problem == "") != tc.valid {
				t.Fatalf("%s registry accepted=%t: %s", tc.table, problem == "", problem)
			}
		})
	}
	// Any incidental FORCE in the file or trigger loop must not vouch for the
	// proof table. A nested JOIN USING in a write policy is not a read predicate.
	const force = "ALTER TABLE custody_account_proofs FORCE ROW LEVEL SECURITY;"
	if !strings.Contains(base, force) {
		t.Fatal("missing proof table FORCE baseline")
	}
	tables := map[string]*tableDecl{}
	scanOneMigration(tables, migration, strings.Replace(base, force, "", 1))
	if d := tables["custody_account_proofs"]; !d.guarded || d.forced {
		t.Fatalf("wrong proof-table predicate or FORCE attribution: %+v", d)
	}
}
