package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A gateway route that names a RESOURCE must gate on who owns it.
//
// authz.Mux answers "does this caller hold the capability", and nothing else. It
// has no notion of which resource the capability applies to, and every
// authenticated caller holds Read (API_GATEWAY_REQUIRED_ROLE is kanz-user). So
// for the three portfolio routes the id came straight from r.PathValue, the
// principal was never consulted, and any authenticated user could read any
// portfolio's exposure, measures or scenario by naming its id (#222).
//
// The fix is writeOwned: it compares the reply's owner_tenant against the
// caller's tenant and 404s a mismatch. This guard is what stops the NEXT
// resource route from being added through plain write() and inheriting the hole
// — the failure is silent by nature, because such a route works perfectly for
// the person who wrote it.
//
// WHY AST AND NOT A GREP. The registration and the handler are in different
// places: Routes() maps a pattern to a method value, and the write call is
// inside that method's body. A textual scan would have to guess which function
// a pattern belongs to, and would either miss a renamed handler or flag a
// helper that happens to contain the word.
//
// SCOPE: patterns containing "{" — a path variable is what makes a route
// resource-scoped. "GET /v1/health" takes no id, addresses no tenant's data, and
// is correctly served by plain write().
var gatewayResourceAuthzExempt = map[string]string{}

func TestEveryResourceScopedGatewayRouteGatesOnOwnership(t *testing.T) {
	path := filepath.Join(moduleRoot(t), filepath.FromSlash(
		"services/api-gateway/internal/gateway/gateway.go"))

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse gateway.go: %v", err)
	}

	// pattern -> handler method name, for every mux.Handle(cap, pattern, h.method).
	routes := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 3 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Handle" {
			return true
		}
		lit, ok := call.Args[1].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		pattern, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		hsel, ok := call.Args[2].(*ast.SelectorExpr)
		if !ok {
			return true
		}
		routes[pattern] = hsel.Sel.Name
		return true
	})

	// NON-VACUITY: this file definitely registers routes. Finding none means the
	// registration shape moved and this guard is asserting nothing.
	if len(routes) < 3 {
		t.Fatalf("found %d routes in gateway.go — expected at least 3. The guard is not "+
			"reading the registrations, so it would pass whatever they do", len(routes))
	}

	// handler name -> does its body call h.writeOwned
	gated := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Body == nil {
			return true
		}
		ast.Inspect(fn.Body, func(in ast.Node) bool {
			sel, ok := in.(*ast.SelectorExpr)
			if ok && sel.Sel.Name == "writeOwned" {
				gated[fn.Name.Name] = true
			}
			return true
		})
		return true
	})

	var ungated []string
	resourceRoutes := 0
	for pattern, handler := range routes {
		if !strings.Contains(pattern, "{") {
			continue // no path variable: addresses no particular tenant's resource
		}
		resourceRoutes++
		if _, ok := gatewayResourceAuthzExempt[pattern]; ok {
			continue
		}
		if !gated[handler] {
			ungated = append(ungated, pattern+" -> "+handler)
		}
	}

	// NON-VACUITY, second half: if nothing matched "{", the scope filter is wrong
	// and every route slipped through it unexamined.
	if resourceRoutes == 0 {
		t.Fatal("no route pattern contains a path variable — the resource-scoped filter " +
			"matched nothing, so this guard examined no routes at all")
	}

	sort.Strings(ungated)
	if len(ungated) > 0 {
		t.Errorf("these resource-scoped routes reach the client without an ownership check:\n  %s\n\n"+
			"They resolve a resource from the path and authorize on ROLE alone, which every "+
			"authenticated caller holds — so any caller can read any tenant's resource by naming "+
			"its id (#222).\n\n"+
			"Route the reply through h.writeOwned instead of h.write. It compares the upstream "+
			"owner_tenant against the caller's tenant, treats an empty owner as a denial, and "+
			"returns the same 404 a genuine miss returns so id enumeration is not a cross-tenant "+
			"directory.", strings.Join(ungated, "\n  "))
	}

	// DEAD ENTRIES: an exemption for a route that is now gated, or no longer
	// exists, is stale permission — it would silently re-authorise the hole.
	var dead []string
	for pattern := range gatewayResourceAuthzExempt {
		handler, ok := routes[pattern]
		if !ok {
			dead = append(dead, pattern+" (no such route)")
			continue
		}
		if gated[handler] {
			dead = append(dead, pattern+" (now gated — exemption outlived the repair)")
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("gatewayResourceAuthzExempt has %d stale entr(y/ies):\n  %s",
			len(dead), strings.Join(dead, "\n  "))
	}
}
