package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A TENANT POOL ASKS POSTGRES WHETHER RLS APPLIES TO IT (#634).
//
// # What went wrong without it
//
// Every tenant-isolation policy on this platform is FORCE ROW LEVEL SECURITY
// plus USING (tenant_id = app_current_tenant()). Postgres exempts a SUPERUSER
// and a BYPASSRLS role from RLS UNCONDITIONALLY — FORCE reaches neither.
//
// So the entire guarantee rested on one property of the DSN. That property was
// asserted in fifteen comments across the deploy manifests, cmd/kanz-migrate,
// secretproviderclass.yaml and CLAUDE.md; it was SET by a single CREATE ROLE
// line in a manifest labelled `kanz.eighred.com/posture: dev-only`; and it was
// checked by nothing that runs. A repo-wide grep for the only ways to ask —
// rolsuper, rolbypassrls, pg_roles, pg_has_role — returned zero hits in any .go
// file.
//
// The failure that produces is the one CLAUDE.md's standards forbid by name: a
// Vault entry written with the wrong role gives a service that starts cleanly,
// passes readiness, sets app.tenant_id on every connection, logs nothing
// unusual, and serves every tenant's rows to every tenant. No error, no metric,
// no denial — "nothing configured" and "checked, and fine" are the same
// observable event.
//
// It also compromised the EVIDENCE. The Postgres-gated isolation tests pass
// identically against a superuser TEST_POSTGRES_URL; CLAUDE.md warns about
// exactly that ("the isolation tests pass falsely"). Because those tests open
// their pools through NewTenantPool, the refusal added there now fails them
// instead — a green RLS suite means the suite ran against a role RLS applies to.
//
// # Why a guard rather than the test that would prove it
//
// The behaviour needs a real Postgres, and the box this is developed on has
// none (no Docker; TEST_POSTGRES_URL unset ⇒ every Postgres-gated test SKIPS
// and reports ok). So the runtime path is CI's to exercise. What can be held
// here is that the check has not been deleted or hollowed out — which is the
// realistic regression, given it took fifteen comments and zero code to arrive
// at this state once already.
//
// It reads STRING LITERALS FROM THE AST, so it cannot be satisfied by this
// file's own prose or by a comment in pool.go describing the check that used to
// be there. Three guards in this package have passed while the checked thing was
// deleted, for exactly that reason.
func TestTenantPoolAsksWhetherRLSAppliesToIt(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, "internal", "pg", "pool.go")

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0) // no ParseComments: comments must not count
	if err != nil {
		t.Fatalf("parse internal/pg/pool.go: %v", err)
	}

	var fn *ast.FuncDecl
	for _, d := range file.Decls {
		if f, ok := d.(*ast.FuncDecl); ok && f.Name != nil && f.Name.Name == "NewTenantPool" {
			fn = f
			break
		}
	}
	if fn == nil {
		t.Fatal("internal/pg/pool.go declares no NewTenantPool — this guard is blind. If the tenant " +
			"pool moved, move this guard with it rather than deleting it.")
	}

	// Every string literal in the function body, concatenated. The SQL is code,
	// not prose, and ParseFile without ParseComments cannot see a comment.
	var sql strings.Builder
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if v, uerr := strconv.Unquote(lit.Value); uerr == nil {
			sql.WriteString(v)
			sql.WriteString("\n")
		}
		return true
	})
	body := sql.String()

	// The two attributes that defeat RLS. Both, because they are separate
	// properties: a role can be BYPASSRLS without being SUPERUSER, and checking
	// only rolsuper would report that role as safe.
	for _, want := range []string{"rolsuper", "rolbypassrls"} {
		if !strings.Contains(body, want) {
			t.Errorf("NewTenantPool never mentions %q in any SQL it runs.\n\n"+
				"Postgres exempts such a role from row-level security unconditionally, so a pool "+
				"opened on one applies app.tenant_id and isolates nothing — and says nothing about "+
				"it. This is the check that stops that pool from being handed out.", want)
		}
	}

	// pg_has_role, not a lookup on current_user alone: BYPASSRLS is INHERITED
	// through role membership, so a login role that is not itself rolbypassrls but
	// is a member of one that is bypasses RLS just the same. A check on the login
	// role's own attributes would report that estate as safe.
	if !strings.Contains(body, "pg_has_role") {
		t.Error("NewTenantPool's check does not use pg_has_role, so it only inspects the login role's " +
			"OWN attributes. BYPASSRLS is inherited through role membership: a role that is a member " +
			"of a BYPASSRLS role bypasses RLS identically, and this check would call it safe.")
	}

	// AND IT MUST REFUSE. A check whose result is logged leaves the service
	// running and serving, which is the state being fixed. The refusal is an
	// error return inside AfterConnect, so pgx discards the connection and the
	// pool hands out nothing.
	if !strings.Contains(body, "refusing a tenant pool") {
		t.Error("NewTenantPool queries the role's RLS exemption but no longer REFUSES on it. " +
			"Logging the finding leaves a service running that serves every tenant's rows to every " +
			"tenant — the check has to deny, not report.")
	}
}
