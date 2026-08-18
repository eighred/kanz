package arch

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A CAPABILITY A ROUTE DEMANDS AND NO ROLE CARRIES IS NOT A STRICT CONTROL. IT IS
// A TOTAL OUTAGE OF THAT CAPABILITY, AND FROM OUTSIDE THE TWO ARE IDENTICAL.
//
// #535. authz.Fund was declared (authz.go), demanded by POST /v1/portfolios/{id}/
// cash-movements (proxy.go), and granted to NO ROLE — the composition root's
// Grants map held three roles for four capabilities. So the one route on this
// gateway that moves the FUND'S OWN capital answered 403 to every principal that
// exists, from #415 until somebody tried to move cash.
//
// The 403 is what made it invisible. A route that is registered, documented,
// capability-gated and reachable looks exactly like one that works; "you may not"
// is a plausible answer, and nobody investigates a permission denial they assume
// they have the wrong token for. The truthful answer was "nobody may, ever, in
// this deployment", and nothing said it.
//
// # This is the estate's own named failure mode, for the third time
//
// CLAUDE.md: "'Nothing configured' and 'checked, and fine' must never look the
// same." A DECLARED THING WITH NO PRODUCER, after identity.StatusDisabled (#525 —
// a status enforced everywhere and written nowhere) and store.Resolution1h (#509
// — a series every query supported and nothing produced). Here the missing
// producer is a GRANT.
//
// # What this checks
//
// Every capability named by a route registration under services/api-gateway/
// appears in the Grants policy the api-gateway composition root builds.
//
// It is a comparison of two sets in one file, and it would have failed the day
// the cash-movement route landed.
//
// # Why the AST, and not a grep
//
// The word "Fund" appears in this repository's PROSE far more often than in its
// code: the capability's own declaration carries a nine-line argument, proxy.go
// carries another, and both spell it. A textual scan matches its own comments and
// its neighbours' — the failure that has produced three false-green guards here
// already (a-guard-that-matches-prose). Comments are not expressions and are not
// reachable by ast.Inspect, so the match is on identifiers only, and it is
// IMPORT-SCOPED: a file may name a capability only through the local name it
// imported the authz package under.
//
// # What counts as REQUIRED, and what counts as GRANTED
//
//	REQUIRED   authz.Mux.Handle(authz.Fund, "POST /v1/...", h)  — a 3-arg call
//	           whose first argument is a capability selector. Handle takes the
//	           capability as a required parameter (SEC-M2), so this is the only
//	           shape a route registration has.
//
//	GRANTED    a capability selector inside a composite literal whose type is
//	           authz.Grants or []authz.Capability, in the composition root.
//	           `authz.Grants{role: {authz.Read, authz.Trade}}` covers the map
//	           form (the inner elements have an elided type and are read as part
//	           of the same literal); `grants[role] = []authz.Capability{...}`
//	           covers the conditional form a role that may legitimately be unset
//	           has to use.
//
// A BARE MENTION IS NOT A GRANT. `authz.NewMux`, `authz.Grants` as a type, or a
// capability passed to some other function are all excluded by the literal-type
// filter — otherwise a guard could be satisfied by naming the constant anywhere
// in main.go, which is close to naming it in a comment.
//
// # What it cannot see, stated here rather than discovered later
//
//  1. IT IS STATIC, SO IT CANNOT SEE CONFIGURATION. Both halves of the fund
//     surface are conditional on API_GATEWAY_FUND_ROLE: the route is registered
//     only when a funder is named, and the grant is added only then. This guard
//     sees both unconditionally and pairs them. That is the right granularity for
//     what it exists to prove — that a capability has a grant EXPRESSION AT ALL —
//     and config.validateAuth holds the runtime half (fronting the book of record
//     with no fund role refuses to start). A capability whose route is registered
//     unconditionally and whose grant is behind an `if` that no deployment
//     satisfies would pass here; that is the residual, and it is why the
//     validation is not optional.
//  2. IT DOES NOT CHECK THAT A HUMAN HOLDS THE ROLE. Grants maps role names to
//     capabilities; whether the identity provider ever issues that role, and
//     whether one subject holds two roles that were meant to be separate, is
//     outside this module entirely. A distinct capability is NECESSARY BUT NOT
//     SUFFICIENT for a two-person control.
//  3. IT DOES NOT CHECK THE REVERSE. A role granted a capability no route
//     demands is dead policy rather than a dead control, and it fails safe.

// gatewayGrantExempt maps a capability to the argument for why no role carries
// it, and to the issue that will grant it.
//
// THE ONLY ARGUMENT THIS LIST ACCEPTS IS THAT THE ROUTE IS UNREACHABLE BY
// CONSTRUCTION — not "it is coming soon", which is precisely the state #535
// found and is indistinguishable from a working control. If a capability has no
// funder, no operator, no anybody, the remedy is to grant it or to stop
// registering the route.
var gatewayGrantExempt = map[string]string{}

const gatewayAuthzPkg = "services/api-gateway/internal/authz"

func TestEveryCapabilityARouteDemandsIsGrantedToSomeRole(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	declared := gatewayDeclaredCapabilities(t, root, fset)
	// NON-VACUITY, FIRST DIRECTION: the capability declaration. If the const-block
	// walk breaks, nothing is a capability, both sets below are empty, and the
	// guard passes having compared nothing to nothing.
	if len(declared) < 3 {
		t.Fatalf("found %d capabilities declared in %s/authz.go — the const walk is broken, not the "+
			"estate. Read, Trade, Operate and Fund must all be found", len(declared), gatewayAuthzPkg)
	}

	required := gatewayRequiredCapabilities(t, root, fset, declared)
	// NON-VACUITY, SECOND DIRECTION: the route scan. Every /v1 route on this
	// gateway declares a capability by construction, so finding a handful means
	// the registration shape moved and a new dead capability would sail past.
	if len(required) == 0 {
		t.Fatal("no route registration named a capability anywhere under services/api-gateway — " +
			"the Handle(cap, pattern, h) shape moved and this guard is comparing an empty set")
	}
	routes := 0
	for _, patterns := range required {
		routes += len(patterns)
	}
	if routes < 15 {
		t.Fatalf("found only %d capability-gated route registration(s) — the gateway serves well "+
			"over twenty. The scan is broken, so an ungranted capability would not be seen", routes)
	}

	granted := gatewayGrantedCapabilities(t, root, fset, declared)
	// NON-VACUITY, THIRD DIRECTION: the grants scan. An empty granted set makes
	// EVERY capability look dark at once, which is how a bug in this file gets
	// mistaken for a finding and answered with a page of exemptions.
	if len(granted) < 3 {
		t.Fatalf("found %d granted capabilit(y/ies) in the api-gateway composition root — the "+
			"Grants literal moved or was renamed. Read, Trade and Operate are granted there today, "+
			"so this guard is not reading the policy it exists to compare against", len(granted))
	}

	var dark []string
	seenExempt := map[string]bool{}
	for capName, patterns := range required {
		if granted[capName] {
			continue
		}
		if reason, ok := gatewayGrantExempt[capName]; ok {
			seenExempt[capName] = true
			t.Logf("authz.%s: demanded by %v and granted to no role — tracked: %s", capName, patterns, reason)
			continue
		}
		sort.Strings(patterns)
		dark = append(dark, "authz."+capName+" (demanded by "+strings.Join(patterns, ", ")+")")
	}

	if len(dark) > 0 {
		sort.Strings(dark)
		t.Errorf("%d capabilit(y/ies) are demanded by a registered route and carried by NO ROLE in "+
			"the api-gateway composition root's Grants map:\n  %s\n\n"+
			"Grants.grantingRole iterates the caller's roles and returns the first match, and there "+
			"is no fallback — so every principal that exists is refused, and the route answers 403. "+
			"That is not a strict control; it is a total outage of the capability, and a route that "+
			"is registered, documented and capability-gated looks exactly like one that works. "+
			"authz.Fund sat in that state from #415 to #535 while POST /v1/portfolios/{id}/"+
			"cash-movements was unreachable by anybody.\n\n"+
			"Grant it to a role in services/api-gateway/cmd/api-gateway/main.go — with a config "+
			"field, an env var and a validateAuth collision check, the way the trade and operator "+
			"roles have — or stop registering the route, so the deployment answers 404 (\"there is "+
			"no such surface here\") instead of 403 (\"you may not\"). Do NOT add an entry to "+
			"gatewayGrantExempt to buy time: that is the exact state this guard exists to end.",
			len(dark), strings.Join(dark, "\n  "))
	}

	// DEAD-ENTRY ARM. An exemption for a capability that is now granted, or that no
	// route demands any more, keeps vouching for a state that no longer exists —
	// and the next reader takes it as evidence that somebody looked.
	for capName, reason := range gatewayGrantExempt {
		if !seenExempt[capName] {
			t.Errorf("exemption for authz.%s is stale — the capability is now granted, or no route "+
				"demands it. Delete the entry (%s)", capName, reason)
		}
	}
}

// gatewayDeclaredCapabilities returns the exported constants of type Capability
// declared in the authz package.
//
// Read from SOURCE rather than imported, and not by choice: services/api-gateway/
// internal/authz is under an internal/ tree rooted at the service, so no package
// outside services/api-gateway may import it. test/arch never can.
func gatewayDeclaredCapabilities(t *testing.T, root string, fset *token.FileSet) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	walkGoFiles(t, root, gatewayAuthzPkg, fset, func(_ string, f *ast.File) {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || vs.Type == nil {
					continue
				}
				id, ok := vs.Type.(*ast.Ident)
				if !ok || id.Name != "Capability" {
					continue
				}
				for _, n := range vs.Names {
					if n.IsExported() {
						out[n.Name] = true
					}
				}
			}
		}
	})
	return out
}

// gatewayRequiredCapabilities maps each capability to the route patterns that
// demand it, over every handler package the gateway registers routes from.
//
// The scan covers all of services/api-gateway rather than a list of handler
// packages: a list is a thing to forget to extend, and forgetting it here would
// mean the newest route — the one most likely to demand a capability nobody has
// granted yet — is the one route not checked.
func gatewayRequiredCapabilities(
	t *testing.T, root string, fset *token.FileSet, declared map[string]bool,
) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	walkGoFiles(t, root, "services/api-gateway", fset, func(rel string, f *ast.File) {
		local, ok := gatewayAuthzImportName(f)
		if !ok {
			return
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 3 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Handle" {
				return true
			}
			capName, ok := gatewayCapabilityName(call.Args[0], local, declared)
			if !ok {
				return true
			}
			pattern := "a route in " + rel
			if lit, isLit := call.Args[1].(*ast.BasicLit); isLit && lit.Kind == token.STRING {
				if p, uerr := strconv.Unquote(lit.Value); uerr == nil {
					pattern = p
				}
			}
			out[capName] = append(out[capName], pattern)
			return true
		})
	})
	return out
}

// gatewayGrantedCapabilities returns the capabilities the composition root's
// policy actually confers on a role.
//
// SCOPED TO COMPOSITE LITERALS OF THE POLICY TYPES, which is what makes this a
// grant rather than a mention. authz.Grants{...} is the map form; []authz.
// Capability{...} is the form a conditionally-added role must use, because Grants
// is keyed by the role STRING and a role that is legitimately unset would
// otherwise put a "" key in the policy — which any token carrying an empty role
// entry would match.
func gatewayGrantedCapabilities(
	t *testing.T, root string, fset *token.FileSet, declared map[string]bool,
) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	walkGoFiles(t, root, "services/api-gateway/cmd/api-gateway", fset, func(_ string, f *ast.File) {
		local, ok := gatewayAuthzImportName(f)
		if !ok {
			return
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !gatewayIsPolicyLiteral(lit.Type, local) {
				return true
			}
			// The whole subtree: the map form nests one elided literal per role,
			// and those inner elements are where the capabilities are.
			ast.Inspect(lit, func(in ast.Node) bool {
				if capName, isCap := gatewayCapabilityName(in, local, declared); isCap {
					out[capName] = true
				}
				return true
			})
			return true
		})
	})
	return out
}

// gatewayIsPolicyLiteral reports whether a composite literal's type is the grants
// map or a capability slice — the two shapes that confer a capability on a role.
func gatewayIsPolicyLiteral(typ ast.Expr, local string) bool {
	switch x := typ.(type) {
	case *ast.SelectorExpr: // authz.Grants{...}
		id, ok := x.X.(*ast.Ident)
		return ok && id.Name == local && x.Sel.Name == "Grants"
	case *ast.ArrayType: // []authz.Capability{...}
		sel, ok := x.Elt.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		id, isIdent := sel.X.(*ast.Ident)
		return isIdent && id.Name == local && sel.Sel.Name == "Capability"
	}
	return false
}

// gatewayCapabilityName reads a capability reference, import-scoped: `authz.Fund`
// where authz is this file's local name for the package and Fund is a declared
// capability. Anything else — a bare identifier, another package's constant, a
// call — is not one.
func gatewayCapabilityName(e ast.Node, local string, declared map[string]bool) (string, bool) {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || id.Name != local || !declared[sel.Sel.Name] {
		return "", false
	}
	return sel.Sel.Name, true
}

// gatewayAuthzImportName returns the local name a file imports the authz package
// under, honouring an alias. Files that do not import it cannot name a
// capability, and are skipped whole.
func gatewayAuthzImportName(f *ast.File) (string, bool) {
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != modulePath+"/"+gatewayAuthzPkg {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name, true
		}
		return filepath.Base(path), true
	}
	return "", false
}
