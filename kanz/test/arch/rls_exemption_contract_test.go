package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/identity"
)

// Redemption discovers the tenant from the stored invitation before there is a
// principal to scope a connection with. The account must retain that authority;
// Signer.Mint's verifier contract then carries the account tenant into the token.
// The Postgres redemption suite separately proves the row and single-use step.
func TestInvitationRLSExceptionRetainsStoredAuthority(t *testing.T) {
	inv := &identity.Invite{Subject: "user:invitee", Tenant: "inviting-tenant", Roles: []string{"kanz-user"}, Portfolios: []string{"portfolio-a"}}
	u := identity.UserFromInvite(inv, identity.Hash("fixture-credential-hash"), time.Unix(1, 0))
	if u.Subject != inv.Subject || u.Tenant != inv.Tenant || strings.Join(u.Roles, ",") != "kanz-user" || strings.Join(u.Portfolios, ",") != "portfolio-a" {
		t.Fatalf("redeemed account did not retain the stored invitation's subject, tenant and entitlements")
	}
	inv.Roles[0] = "kanz-operator"
	inv.Portfolios[0] = "foreign-portfolio"
	if strings.Join(u.Roles, ",") != "kanz-user" || strings.Join(u.Portfolios, ",") != "portfolio-a" {
		t.Fatal("an invitation mutated after redemption changed the account's authority")
	}
}

func TestRoutingDirectoryExemptionRejectsChangedPremises(t *testing.T) {
	const base = `CREATE TABLE identity_access_audit_tenants (tenant_id TEXT PRIMARY KEY);
CREATE TABLE identity_access_audit (tenant_id TEXT NOT NULL);
CREATE TABLE identity_access_audit_pending (tenant_id TEXT NOT NULL);
ALTER TABLE identity_access_audit ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity_access_audit FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON identity_access_audit USING(tenant_id=app_current_tenant());
ALTER TABLE identity_access_audit_pending ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity_access_audit_pending FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON identity_access_audit_pending USING(tenant_id=app_current_tenant());`
	for _, tc := range []struct {
		name, from, to, appendSQL string
		want                      bool
	}{
		{name: "unchanged directory"},
		{name: "inline payload", from: "(tenant_id TEXT PRIMARY KEY)", to: "(tenant_id TEXT PRIMARY KEY, decision JSONB)", want: true},
		{name: "later payload", appendSQL: "ALTER TABLE identity_access_audit_tenants ADD COLUMN decision JSONB;", want: true},
		{name: "if exists", appendSQL: "ALTER TABLE IF EXISTS identity_access_audit_tenants ADD COLUMN decision JSONB;", want: true},
		{name: "only", appendSQL: "ALTER TABLE ONLY identity_access_audit_tenants ADD COLUMN decision JSONB;", want: true},
		{name: "qualified", appendSQL: `ALTER TABLE IF EXISTS ONLY "public"."identity_access_audit_tenants" ADD COLUMN decision JSONB;`, want: true},
		{name: "payload loses FORCE", from: "ALTER TABLE identity_access_audit FORCE ROW LEVEL SECURITY;", want: true},
		{name: "pending loses scope", from: "USING(tenant_id=app_current_tenant());", to: "USING(true);", want: true},
		{name: "exception gains RLS", appendSQL: "ALTER TABLE identity_access_audit_tenants ENABLE ROW LEVEL SECURITY;", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sql := base
			if tc.from != "" {
				if !strings.Contains(sql, tc.from) {
					t.Fatal("mutation no longer matches its baseline")
				}
				sql = strings.Replace(sql, tc.from, tc.to, 1)
			}
			tables := map[string]*tableDecl{}
			scanOneMigration(tables, "fixture.sql", sql+tc.appendSQL)
			problem := rlsExemptionProblem("identity_access_audit_tenants", tables["identity_access_audit_tenants"], tables)
			if (problem != "") != tc.want {
				t.Fatalf("routing premise rejected=%v, want %v: %s", problem != "", tc.want, problem)
			}
		})
	}
}

// Directory enumeration is an internal delivery operation, not a tenant-facing
// read API. Only the two existing owner functions may reference its SQL shape;
// a new direct query in a server or another worker requires a reviewed boundary.
func TestAuditRoutingDirectoryHasNoReadAPI(t *testing.T) {
	root := moduleRoot(t)
	for path, source := range productionSources(t, root) {
		if !strings.Contains(source, "_tenants") {
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
		if err != nil {
			t.Fatal(err)
		}
		if problems := auditDirectoryQueryProblems(filepath.ToSlash(rel), file); len(problems) > 0 {
			t.Errorf("unreviewed audit routing directory surface: %s", strings.Join(problems, ", "))
		}
	}
}

func auditDirectoryQueryProblems(rel string, file *ast.File) []string {
	var problems []string
	for _, decl := range file.Decls {
		fn, function := decl.(*ast.FuncDecl)
		name := "package declaration"
		if function {
			name = fn.Name.Name
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			literal, ok := n.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil {
				return true
			}
			if !strings.Contains(value, "_audit_tenants") && !strings.Contains(value, "%s_tenants") && value != "_tenants" {
				return true
			}
			allowed := rel == "internal/auditdelivery/worker.go" &&
				((name == "New" && value == "_tenants") ||
					(name == "Run" && strings.Join(strings.Fields(value), " ") ==
						"SELECT tenant_id FROM %s_tenants WHERE tenant_id>$1 ORDER BY tenant_id LIMIT 1"))
			if !allowed {
				problems = append(problems, rel+":"+name)
			}
			return true
		})
	}
	return problems
}

func TestAuditDirectoryGuardRejectsADirectReadSurface(t *testing.T) {
	const query = "SELECT tenant_id FROM %s_tenants WHERE tenant_id>$1 ORDER BY tenant_id LIMIT 1"
	for _, tc := range []struct {
		file, function string
		want           bool
	}{
		{"internal/auditdelivery/worker.go", "Run", false},
		{"services/identity/internal/server/directory.go", "List", true},
		{"internal/auditdelivery/worker.go", "List", true},
	} {
		source := "package fixture\nfunc " + tc.function + "() { query(" + strconv.Quote(query) + ") }"
		file, err := parser.ParseFile(token.NewFileSet(), tc.file, source, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(auditDirectoryQueryProblems(tc.file, file)) > 0; got != tc.want {
			t.Fatalf("%s:%s rejected=%v, want %v", tc.file, tc.function, got, tc.want)
		}
	}
	file, err := parser.ParseFile(token.NewFileSet(), "worker.go", "package fixture\nfunc Run() { query("+strconv.Quote(strings.Replace(query, "SELECT tenant_id", "SELECT *", 1))+") }", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(auditDirectoryQueryProblems("internal/auditdelivery/worker.go", file)) == 0 {
		t.Fatal("a changed directory read in the owner must require review too")
	}
}
