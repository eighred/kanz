package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// THE SIGNAL FAN-OUT MAY NOT BE REACHED WITHOUT A STRATEGY→FUND→TENANT BINDING
// (#632).
//
// # What went wrong
//
// internal/signal/translate is the ONE path from an advisory signal to executable
// order commands, shared by webhook-ingest (internet-facing, HMAC-authenticated)
// and pkg/alpha (the native engines). It resolved the tenant of every order it
// published through an OPTIONAL seam:
//
//	TenantOf func(fundID string) string   // nil ⇒ the fund_id IS the tenant
//
// No composition root in this module ever assigned it. So the tenant of a
// signal-originated order was the `fund_id` string out of the request body — a
// field the caller chooses — while the perimeter had only ever authenticated the
// STRATEGY. That subject, `tenant.<fund_id>.order.order.submit`, is the routing
// key onto a tenant's own broker account. One leaked strategy secret placed
// orders in every tenant the deployment served.
//
// # What this guard checks, and why it is not the runtime check
//
// translate.New now REFUSES a nil Authority, and that refusal is what actually
// stops a process. But it fires at construction, in a composition root — and
// composition roots are this repository's measured blind spot: cmd/*/main.go
// wiring escapes every unit test, and two crashes have shipped past a green
// suite that way. A binary nobody starts in CI would carry the omission to a
// cluster.
//
// So this asserts the property STATICALLY: every non-test `translate.Options{…}`
// literal in the module names Authority, with something other than nil. A field
// that is merely absent is the exact shape of the original defect, and absence is
// what an AST can see.
//
// # What it does NOT check
//
//   - That the value assigned resolves a real binding table. `Authority: x` where
//     x is a permissive stub satisfies this. The behavioural tests carry that half
//     (translate's TestASignedSignalForAnotherTenantsFundIsRefusedAndPublishesNothing
//     and webhook-ingest's TestPerimeter_ASignedAlertForAnotherTenantsFundIsForbidden).
//   - Construction by any route other than a composite literal — a struct built
//     field by field, or copied from another Options value. Neither shape exists
//     in this tree; both are visible in a diff, which is more than the deleted
//     seam ever was.
func TestEveryTranslatorIsWiredWithAFundBinding(t *testing.T) {
	root := moduleRoot(t)
	sites := translateOptionsSites(t, root)

	// NON-VACUITY. Two composition roots build a translator today —
	// services/webhook-ingest/internal/ingest (the webhook pipeline) and pkg/alpha
	// (the native engine runner). A scan finding fewer than that has lost sight of
	// one and would stay green while it published untenanted orders.
	if len(sites) < 2 {
		t.Fatalf("found %d non-test translate.Options literal(s) (%v) — expected at least 2 "+
			"(webhook-ingest's pipeline and pkg/alpha's runner). The construction shape moved "+
			"and this guard is asserting nothing", len(sites), sites)
	}

	var problems []string
	for _, s := range sites {
		if s.setsAuthority {
			continue
		}
		problems = append(problems, fmt.Sprintf("%s:%d builds a translate.Options without an "+
			"Authority", s.file, s.line))
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%d translator(s) are wired with no strategy→fund→tenant binding:\n\n  %s\n\n"+
			"Every order this translator publishes is routed on a subject derived from the fund "+
			"the CALLER named. On the webhook path the caller is authenticated as a STRATEGY, not "+
			"as a fund and not as a tenant, so an absent binding is one leaked secret away from "+
			"orders in another tenant's book (#632). Pass a translate.FundAuthority built from "+
			"configuration; there is deliberately no default.",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// translateOptionsSite is one non-test construction of a translate.Options.
type translateOptionsSite struct {
	file          string // module-relative, forward slashes
	line          int
	setsAuthority bool
}

func (s translateOptionsSite) String() string { return fmt.Sprintf("%s:%d", s.file, s.line) }

// translateOptionsSites walks the module and returns every non-test
// `translate.Options{…}` composite literal, plus the unqualified `Options{…}`
// literals inside package translate itself — where the field is DEFINED and where
// a helper constructing one would be just as capable of omitting it.
func translateOptionsSites(t *testing.T, root string) []translateOptionsSite {
	t.Helper()

	const translatePkgDir = "internal/signal/translate"
	var out []translateOptionsSite
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", ".gotmp":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// A file this guard cannot parse is a hole it cannot see through.
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		inTranslate := filepath.ToSlash(filepath.Dir(rel)) == translatePkgDir

		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			switch typ := lit.Type.(type) {
			case *ast.SelectorExpr:
				pkg, ok := typ.X.(*ast.Ident)
				if !ok || pkg.Name != "translate" || typ.Sel.Name != "Options" {
					return true
				}
			case *ast.Ident:
				// Unqualified `Options{}` counts only inside package translate; every
				// other package has Options types of its own (ingest.Options, and
				// several service configs), and crediting those would let this guard
				// report sites it is not checking.
				if !inTranslate || typ.Name != "Options" {
					return true
				}
			default:
				return true
			}
			out = append(out, translateOptionsSite{
				file:          rel,
				line:          fset.Position(lit.Pos()).Line,
				setsAuthority: literalSetsNonNilAuthority(lit),
			})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].line < out[j].line
	})
	return out
}

// literalSetsNonNilAuthority reports whether lit names Authority with something
// other than the nil identifier. `Authority: nil` is the omission spelled longer,
// so it does not count — the same stance producer_tenant_test.go takes on
// `Tenant: ""`.
func literalSetsNonNilAuthority(lit *ast.CompositeLit) bool {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue // unkeyed literal: no field named, so nothing to credit
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "Authority" {
			continue
		}
		if id, ok := kv.Value.(*ast.Ident); ok && id.Name == "nil" {
			return false
		}
		return true
	}
	return false
}

// TestFundBindingGuardDetectsTheShapesItClaimsTo is the guard's own proof of work
// (#601's stance, applied here).
//
// The non-vacuity floor above shows the scan found SOMETHING. It does not show
// that the scan can tell a WIRED literal from an OMITTED one — and a detector
// that answers "wired" to everything passes on a tree with the defect fully
// restored. This drives the two predicates over literals assembled at runtime, so
// the shapes being detected are not sitting in this file waiting to be matched by
// accident.
func TestFundBindingGuardDetectsTheShapesItClaimsTo(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want bool
	}{
		{"authority from a config field", "translate.Options{Authority: cfg.Authority}", true},
		{"authority from a constructor", "translate.Options{Authority: newBinding()}", true},
		{"authority omitted entirely", "translate.Options{Publisher: p, Gate: g}", false},
		{"authority explicitly nil", "translate.Options{Authority: nil}", false},
		{"an unkeyed literal", "translate.Options{p, g}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expr, err := parser.ParseExpr(tc.src)
			if err != nil {
				t.Fatalf("parse fixture: %v", err)
			}
			lit, ok := expr.(*ast.CompositeLit)
			if !ok {
				t.Fatalf("fixture is %T, not a composite literal", expr)
			}
			if got := literalSetsNonNilAuthority(lit); got != tc.want {
				t.Errorf("literalSetsNonNilAuthority(%s) = %v, want %v", tc.src, got, tc.want)
			}
		})
	}
}
