package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// AN AUTHENTICATOR THAT BUILDS A PRINCIPAL MUST BUILD ALL OF IT (#225).
//
// The api-gateway has two authenticators and one edge Principal. The dev HS256
// arm populated all four fields; the OIDC arm — the PRODUCTION one, selected
// whenever API_GATEWAY_OIDC_ISSUER is set, which infra/deploy does — populated
// three and dropped Portfolios, the caller's portfolio entitlement.
//
// NOTHING ABOUT THAT FAILED. Go zero-fills an omitted field in a composite
// literal, so there was no compile error, no nil, no log line: the caller simply
// arrived downstream scoped to nothing. The gateway binds that list onto every
// order command it publishes, so the OMS then refused every cancel and amend
// NOT_ENTITLED while admitting a submit into any portfolio in the tenant — a
// position you can open through the API and cannot close through it.
//
// The mutation asymmetry is what makes this guard the right shape rather than
// another unit test: delete Portfolios from the dev arm and a named test failed;
// delete it from the production arm and NOTHING failed, because that arm had no
// test file at all. A per-authenticator test only ever covers the authenticators
// somebody remembered. This covers the ones they did not.
//
// WHAT IT CHECKS: every composite literal of middleware.Principal in non-test
// api-gateway code names every exported field of the struct. Keyed literals only
// — an unkeyed one is already a compile error the day a field is added, which is
// the same protection by a different mechanism.
//
// WHAT IT CANNOT CHECK: that the value assigned is the RIGHT one.
// `Portfolios: nil` satisfies this guard. The per-authenticator tests
// (TestJWTAuthenticator_CarriesPortfolioClaim,
// TestEdgePrincipal_CarriesEveryFieldOfTheAuthenticatedPrincipal) carry that
// half, and this guard is what forces a new authenticator to have one — it
// cannot compile past this without writing the field name down, at which point
// writing nil is a visible choice in a diff rather than an omission.

// principalCompletenessType is the struct every literal below must fill.
const principalCompletenessType = "Principal"

// principalCompletenessDecl is where that struct is declared, module-relative.
const principalCompletenessDecl = "services/api-gateway/internal/middleware/auth.go"

// principalCompletenessScope is the tree searched for literals of it. The type
// is gateway-internal (Go's internal rule makes it unimportable elsewhere), so
// this is the whole reachable set by construction.
const principalCompletenessScope = "services/api-gateway"

// principalCompletenessExempt maps a module-relative file path to the reason a
// literal in it may be partial, and the issue that retires the entry.
//
// IT IS EMPTY, AND THAT IS THE POINT. A partial edge Principal is a caller
// silently stripped of an authorization dimension; there is no version of that
// which is fine for one file. The map and the dead-entry arm below exist so a
// future exemption has to be argued in a diff, not so one is expected.
var principalCompletenessExempt = map[string]string{}

func TestEveryAuthenticatorPopulatesTheWholePrincipal(t *testing.T) {
	root := moduleRoot(t)

	want := principalFields(t, filepath.Join(root, filepath.FromSlash(principalCompletenessDecl)))
	// NON-VACUITY: the struct has four fields today. A rename or a move that made
	// this come back empty would turn the whole guard into a no-op that passes.
	if len(want) < 4 {
		t.Fatalf("middleware.Principal has %d exported fields (%v) — expected at least 4. "+
			"The declaration moved or was renamed and this guard is asserting nothing",
			len(want), want)
	}

	seen := map[string]bool{}
	for _, gf := range goFilesUnder(t, filepath.Join(root, filepath.FromSlash(principalCompletenessScope))) {
		if strings.HasSuffix(gf.rel, "_test.go") {
			continue
		}
		rel := principalCompletenessScope + "/" + gf.rel
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, rel, gf.body, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isPrincipalLit(lit) {
				return true
			}
			seen[rel] = true
			if reason, ok := principalCompletenessExempt[rel]; ok {
				t.Logf("%s: exempt — %s", rel, reason)
				return true
			}
			got := map[string]bool{}
			for _, el := range lit.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					return true // unkeyed: the compiler already requires every field
				}
				if id, ok := kv.Key.(*ast.Ident); ok {
					got[id.Name] = true
				}
			}
			var missing []string
			for _, fieldName := range want {
				if !got[fieldName] {
					missing = append(missing, fieldName)
				}
			}
			if len(missing) > 0 {
				t.Errorf("%s:%d builds a middleware.Principal without %v.\n"+
					"An omitted field is zero-filled with no error anywhere: the caller reaches "+
					"the OMS stripped of that authorization dimension, which is #225 exactly. "+
					"Name the field — assigning nil is fine if it is deliberate, because then a "+
					"reviewer sees it.",
					rel, fset.Position(lit.Pos()).Line, missing)
			}
			return true
		})
	}

	// NON-VACUITY: at least the two authenticators must have been found. Zero
	// means the literal shape moved (a constructor function, an embed) and this
	// guard now watches nothing.
	if len(seen) < 2 {
		t.Fatalf("found middleware.Principal literals in %d files (%v) — expected at least 2 "+
			"(the OIDC bridge in cmd/api-gateway and the dev HS256 arm in internal/middleware). "+
			"The construction shape moved and this guard is asserting nothing", len(seen), keysOf(seen))
	}

	// DEAD-ENTRY ARM: an exemption that no longer matches a real literal has
	// outlived its repair and must be deleted, or the next violation in that file
	// is waved through silently.
	for rel, reason := range principalCompletenessExempt {
		if !seen[rel] {
			t.Errorf("exemption for %q (%s) matches no middleware.Principal literal — "+
				"delete it", rel, reason)
		}
	}
}

// isPrincipalLit reports whether lit constructs the edge Principal, named either
// bare (inside package middleware) or qualified (everywhere else).
func isPrincipalLit(lit *ast.CompositeLit) bool {
	switch t := lit.Type.(type) {
	case *ast.Ident:
		return t.Name == principalCompletenessType
	case *ast.SelectorExpr:
		pkg, ok := t.X.(*ast.Ident)
		return ok && pkg.Name == "middleware" && t.Sel.Name == principalCompletenessType
	}
	return false
}

// principalFields returns the exported field names of the Principal struct
// declared in path, in declaration order.
//
// Delegates to exportedStructFields (workerdeps_completeness_test.go): the
// WorkerDeps guard asks the identical question of a different struct, and two
// copies of a field-name reader is the shape AGENTS.md names as how a fix stops
// spreading.
func principalFields(t *testing.T, path string) []string {
	t.Helper()
	return exportedStructFields(t, path, principalCompletenessType)
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
