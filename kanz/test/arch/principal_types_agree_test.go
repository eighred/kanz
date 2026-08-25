package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// THE TWO PRINCIPAL TYPES MUST AGREE, OR SAY WHY NOT (#642).
//
// The platform has two Principal types: pkg/auth.Principal, "the canonical
// identity the whole platform shares", and the gateway-local
// middleware.Principal. The split is deliberate — see that type's doc — but
// nothing related their field sets, and that is the gap this closes.
//
// # WHAT WENT WRONG ONE LEVEL DOWN, AND WOULD HAVE RECURRED HERE
//
// #225 was a dropped field. The OIDC authenticator — the PRODUCTION one —
// populated three fields of the edge Principal and omitted Portfolios. Go
// zero-fills an omitted field in a composite literal, so there was no compile
// error and no nil: the caller simply arrived downstream scoped to nothing, and
// the OMS refused every cancel and amend while admitting submits. A position you
// could open through the API and could not close through it.
//
// TestEveryAuthenticatorPopulatesTheWholePrincipal now stops that, but only
// WITHIN the edge type: it checks that every literal of middleware.Principal
// names every field OF middleware.Principal. Add a seventh field to
// pkg/auth.Principal and nothing fails — edgePrincipal keeps compiling and keeps
// dropping it. That is #225's mechanism exactly, one level up from where the
// completeness guard watches.
//
// # TWO ARMS, BECAUSE THE FIELD CAN BE LOST IN EITHER DIRECTION
//
//   - canonical → edge: a field on pkg/auth.Principal absent from
//     middleware.Principal. The gateway never learns it exists.
//   - edge → canonical: the two reverse conversions in orders.go and authz.go
//     build an auth.Principal from an edge one. Both are keyed literals that
//     name four fields, so a new canonical field is dropped there silently even
//     if the edge type gained it. The issue that filed this noted the direction
//     it names is the one with no guard.
//
// # OMISSIONS ARE ALLOWED, IN WRITING
//
// Both maps are default-deny with named reasons, and both are checked for STALE
// entries — an exemption that no longer describes reality fails, so it cannot
// outlive the thing it excused.

// principalCanonicalDecl and principalEdgeDecl are where the two types live.
const (
	principalCanonicalDecl = "pkg/auth/principal.go"
	principalEdgeDecl      = "services/api-gateway/internal/middleware/auth.go"
)

// edgeOmits are canonical fields the EDGE type deliberately does not carry.
var edgeOmits = map[string]string{
	"Claims": "the raw IdP claim bag. Narrowing it at the edge is a security " +
		"boundary, not an omission: it is attacker-influenced input that would " +
		"otherwise travel the whole request chain, into every downstream middleware, " +
		"log line and mesh header, to gain nothing any of them read. Everything the " +
		"chain does need is present on both types.",
}

// reverseOmits are canonical fields the edge→canonical conversions cannot or
// must not supply. Narrower than it looks: a NEW canonical field is not in here,
// so both conversion sites go red the day one is added, which is the point.
var reverseOmits = map[string]string{
	"Claims": "not carried on the edge type at all (see edgeOmits), so there is " +
		"nothing to convert from.",
	"IssuedAt": "deliberately absent past this seam, and pkg/auth/meshheader.go " +
		"argues it in place: IssuedAt exists so the GATEWAY can date a token against " +
		"a revocation mark (#532). An upstream reads no revocation feed and makes no " +
		"such decision, and the gateway has already refused a revoked caller before " +
		"the principal is rebuilt. It arrives as the zero value, which the revocation " +
		"check reads as \"undatable\" — so the absence FAILS CLOSED. Carrying it here " +
		"would contradict that seam's design, not tighten it.",
}

// TestTheTwoPrincipalTypesAgree is the canonical → edge arm.
func TestTheTwoPrincipalTypesAgree(t *testing.T) {
	root := moduleRoot(t)

	canonical := exportedFieldsOf(t, filepath.Join(root, filepath.FromSlash(principalCanonicalDecl)), "Principal")
	edge := exportedFieldsOf(t, filepath.Join(root, filepath.FromSlash(principalEdgeDecl)), "Principal")

	// NON-VACUITY. Either type failing to parse, or being renamed, would leave
	// both sets empty and every comparison below trivially satisfied.
	if len(canonical) < 4 || len(edge) < 4 {
		t.Fatalf("parsed %d canonical and %d edge fields — at least one Principal was not found, "+
			"so this guard is comparing nothing (looked in %s and %s)",
			len(canonical), len(edge), principalCanonicalDecl, principalEdgeDecl)
	}

	edgeHas := make(map[string]bool, len(edge))
	for _, f := range edge {
		edgeHas[f] = true
	}

	for _, f := range canonical {
		if edgeHas[f] {
			continue
		}
		if _, excused := edgeOmits[f]; excused {
			continue
		}
		t.Errorf("pkg/auth.Principal has %[1]s and middleware.Principal does not.\n\n"+
			"The gateway cannot carry a dimension it does not declare, and Go zero-fills the "+
			"omission silently — that is #225's mechanism, which cost a position that could be "+
			"opened through the API and not closed. Add %[1]s to %[2]s, or add it to edgeOmits "+
			"in this file with the reason it is deliberately not carried.",
			f, principalEdgeDecl)
	}

	// STALE ENTRIES. An excuse for a field the edge type now carries, or for one
	// the canonical type no longer has, is a comment pretending to be a control.
	canonicalHas := make(map[string]bool, len(canonical))
	for _, f := range canonical {
		canonicalHas[f] = true
	}
	for _, f := range sortedKeys(edgeOmits) {
		switch {
		case !canonicalHas[f]:
			t.Errorf("edgeOmits excuses %q, which pkg/auth.Principal no longer declares. Delete "+
				"the entry — an exemption must not outlive its subject.", f)
		case edgeHas[f]:
			t.Errorf("edgeOmits excuses %q, but middleware.Principal now carries it. Delete the "+
				"entry, so the next reader is not told a field is withheld when it is not.", f)
		}
	}
}

// TestEveryCanonicalPrincipalLiteralIsComplete is the edge → canonical arm: the
// reverse conversions must name every field of the type they are building.
//
// Keyed literals only. An unkeyed one is already a compile error the day a field
// is added — the same protection by a different mechanism, which is the stance
// the sibling completeness guard takes too.
func TestEveryCanonicalPrincipalLiteralIsComplete(t *testing.T) {
	root := moduleRoot(t)

	canonical := exportedFieldsOf(t, filepath.Join(root, filepath.FromSlash(principalCanonicalDecl)), "Principal")
	if len(canonical) < 4 {
		t.Fatalf("parsed %d fields of pkg/auth.Principal — the guard is asserting nothing", len(canonical))
	}

	type site struct {
		where string
		named map[string]bool
	}
	var sites []site

	for _, f := range goFilesUnder(t, root) {
		if strings.HasSuffix(f.rel, "_test.go") || strings.HasPrefix(f.rel, "pkg/auth/") {
			continue // the type's own package builds it freely; tests are not the estate
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, f.rel, f.body, 0)
		if err != nil {
			continue
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			s, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || s.Sel.Name != "Principal" {
				return true
			}
			pkg, ok := s.X.(*ast.Ident)
			if !ok || pkg.Name != "auth" {
				return true
			}
			named := map[string]bool{}
			keyed := false
			for _, el := range lit.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				keyed = true
				if id, ok := kv.Key.(*ast.Ident); ok {
					named[id.Name] = true
				}
			}
			if !keyed {
				return true
			}
			sites = append(sites, site{
				where: f.rel + ":" + itoa(fset.Position(lit.Pos()).Line),
				named: named,
			})
			return true
		})
	}

	// NON-VACUITY. The two reverse conversions are known to exist; finding none
	// means the walk or the matcher broke, and every check below would pass.
	if len(sites) == 0 {
		t.Fatal("found no keyed auth.Principal literals outside pkg/auth — the two reverse " +
			"conversions in services/api-gateway are known to exist, so this guard is broken " +
			"rather than satisfied")
	}

	everyoneNames := make(map[string]bool, len(canonical))
	for _, f := range canonical {
		everyoneNames[f] = true
	}

	for _, s := range sites {
		for _, f := range canonical {
			if s.named[f] {
				continue
			}
			everyoneNames[f] = false
			if _, excused := reverseOmits[f]; excused {
				continue
			}
			t.Errorf("%s builds an auth.Principal without naming %[2]s.\n\n"+
				"A field the canonical type declares and this conversion omits is zero-filled "+
				"with no compile error and no nil — the caller arrives upstream missing a "+
				"dimension it was authenticated with. Name %[2]s here, or add it to "+
				"reverseOmits in this file with the reason it must not cross this seam.",
				s.where, f)
		}
	}

	// STALE ENTRIES, from both directions.
	canonicalHas := make(map[string]bool, len(canonical))
	for _, f := range canonical {
		canonicalHas[f] = true
	}
	for _, f := range sortedKeys(reverseOmits) {
		switch {
		case !canonicalHas[f]:
			t.Errorf("reverseOmits excuses %q, which pkg/auth.Principal no longer declares. "+
				"Delete the entry.", f)
		case everyoneNames[f]:
			t.Errorf("reverseOmits excuses %q, but every conversion site now names it. Delete "+
				"the entry — otherwise the day one site stops naming it, nothing fails.", f)
		}
	}
}

// exportedFieldsOf returns the exported field names of the named struct, in
// declaration order.
func exportedFieldsOf(t *testing.T, path, typeName string) []string {
	t.Helper()

	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	ast.Inspect(parsed, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != typeName {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, field := range st.Fields.List {
			for _, name := range field.Names {
				if name.IsExported() {
					out = append(out, name.Name)
				}
			}
		}
		return false
	})
	return out
}
