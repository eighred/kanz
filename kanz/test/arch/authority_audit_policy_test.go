package arch

import "testing"

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
