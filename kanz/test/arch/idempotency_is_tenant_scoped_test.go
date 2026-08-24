package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// THE GATEWAY'S IDEMPOTENCY KEY MUST CARRY THE TENANT (#721).
//
// middleware.Idempotency keyed a process-wide map on the raw client-supplied
// Idempotency-Key header. The consequence was not a cache miss:
//
//   - Two tenants that chose the same key within the window SHARED an entry.
//     Because the replay short-circuits BEFORE the handler, the second tenant
//     read the first tenant's response body — on /v1/orders, another fund's
//     order id — and its own request was never executed. It received 202
//     Accepted for an order that does not exist, so it believed it held exposure
//     it did not hold. A cross-tenant leak and a silent order loss in one
//     response.
//   - It needed no attacker. `1`, `retry-1` and a client's own sequence number
//     collide between tenants by accident.
//
// Middleware.Quota, one line above it in the same chain and reading the same
// principal, already scoped by tenant. That is what made this an omission rather
// than a constraint, and it is why a guard is worth more than a comment: the
// correct pattern was already present and adjacent, and the defect landed anyway.
//
// # WHY THIS PARSES THE AST
//
// A guard that greps for the word "tenant" in this file would be satisfied by
// the paragraph above. The sibling redis-tag guard was, in fact, passing on a
// Dockerfile COMMENT that contained the flag it was checking for — found by
// mutation while writing this one. So the checks below resolve identifiers:
// which function derives the key, what it calls, and which value reaches the
// claim store.

const idemMiddlewareFile = "../../services/api-gateway/internal/middleware/middleware.go"

// tenantSource is the package's ONE tenant accessor: the authenticated tenant,
// else "anonymous". The quota limiter reads the tenant through it too, and two
// answers to "which tenant is this" is how one of them drifts.
//
// Deliberately NOT a set that also accepts PrincipalFromContext. An earlier
// draft of this guard did, and a mutation deleting the tenant from the key
// SURVIVED it: idempotencyScope still called PrincipalFromContext to read the
// subject, so "reads the principal" was satisfied while the tenant was gone.
// A guard proves what it RESOLVED, not what its name claims.
const tenantSource = "tenantLabel"

// claimAndCacheCalls are the methods whose FIRST argument is the key. Every one
// of them must receive the derived scope, never the header as the client sent it.
var claimAndCacheCalls = map[string]bool{
	"Claim": true, "Commit": true, "Release": true, "get": true, "put": true,
}

func TestGatewayIdempotencyKeyCarriesTheTenant(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.FromSlash(idemMiddlewareFile), nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", idemMiddlewareFile, err)
	}

	funcs := map[string]*ast.FuncDecl{}
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Body != nil {
			funcs[fn.Name.Name] = fn
		}
	}

	// NON-VACUITY. If these have been renamed or moved, this guard is asserting
	// things about a file that no longer holds the seam — move the guard with it
	// rather than leaving it green over nothing.
	scope, ok := funcs["idempotencyScope"]
	if !ok {
		t.Fatal("no idempotencyScope function in " + idemMiddlewareFile + ". The key derivation has " +
			"moved or been renamed; MOVE THIS GUARD WITH IT rather than leaving it passing over a " +
			"file it no longer understands")
	}
	entry, ok := funcs["IdempotencyWith"]
	if !ok {
		t.Fatal("no IdempotencyWith function in " + idemMiddlewareFile + "; the middleware has been " +
			"restructured and this guard no longer describes it")
	}

	// CHECK 1: the tenant reaches the RETURNED key.
	//
	// Checking the whole body would accept `_ = tenantLabel(r)` sitting beside a
	// key that does not use it. The return expression is where the key is built,
	// so that is where the call has to appear.
	ret := returnExpr(scope.Body)
	if ret == nil {
		t.Fatal("idempotencyScope has no return expression; this guard cannot see how the key is built")
	}
	if !callsFunc(ret, tenantSource) {
		t.Errorf("the key idempotencyScope returns does not include %s(r).\n\n"+
			"An idempotency key is chosen by the CLIENT. Without the tenant in the scope, two tenants "+
			"that pick the same string share a cache entry — and because the replay short-circuits "+
			"before the handler, the second one reads the first one's response body and its own "+
			"request is never executed. On /v1/orders that is another fund's order id returned with "+
			"202 for an order that was never placed (#721).", tenantSource)
	}

	// CHECK 2: the raw header never reaches the claim store or the replay cache.
	//
	// Finding the variable the scope was assigned to, and requiring THAT variable
	// at every key position, is what stops a future edit from passing the header
	// straight through while idempotencyScope sits unused beside it.
	scopeVar := assignedFrom(entry.Body, "idempotencyScope")
	if scopeVar == "" {
		t.Fatal("IdempotencyWith never assigns the result of idempotencyScope to a variable, so this " +
			"guard cannot tell which value is used as the key")
	}
	for _, bad := range keyArgsOtherThan(entry.Body, scopeVar) {
		t.Errorf("IdempotencyWith passes %q as the key to %s(), not the tenant-scoped %q.\n\n"+
			"The key position must always carry the derived scope. Passing the client's header "+
			"through is the #721 defect exactly: a process-wide store keyed on a string the caller "+
			"chooses, shared across every tenant on the replica.", bad.arg, bad.method, scopeVar)
	}
}

// returnExpr returns the first return statement's single expression, or nil.
func returnExpr(body *ast.BlockStmt) ast.Node {
	var out ast.Node
	ast.Inspect(body, func(n ast.Node) bool {
		r, ok := n.(*ast.ReturnStmt)
		if !ok || out != nil || len(r.Results) != 1 {
			return true
		}
		out = r.Results[0]
		return false
	})
	return out
}

// callsFunc reports whether the expression calls the named function.
func callsFunc(root ast.Node, name string) bool {
	found := false
	ast.Inspect(root, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == name {
				found = true
			}
		case *ast.SelectorExpr:
			if fn.Sel.Name == name {
				found = true
			}
		}
		return !found
	})
	return found
}

// assignedFrom returns the name of the first variable assigned the result of
// calling fn, or "" when there is none.
func assignedFrom(body *ast.BlockStmt, fn string) string {
	name := ""
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || name != "" || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || id.Name != fn {
			return true
		}
		if lhs, ok := as.Lhs[0].(*ast.Ident); ok {
			name = lhs.Name
		}
		return true
	})
	return name
}

type badKeyArg struct{ method, arg string }

// keyArgsOtherThan returns every claim/cache call whose first argument is a bare
// identifier that is not want. A non-identifier first argument (a composed
// expression) is reported too, under its rendered name, because the key position
// should carry the derived scope and nothing else.
func keyArgsOtherThan(body *ast.BlockStmt, want string) []badKeyArg {
	var out []badKeyArg
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !claimAndCacheCalls[sel.Sel.Name] {
			return true
		}
		id, ok := call.Args[0].(*ast.Ident)
		if !ok {
			out = append(out, badKeyArg{method: sel.Sel.Name, arg: "a composed expression"})
			return true
		}
		if id.Name != want {
			out = append(out, badKeyArg{method: sel.Sel.Name, arg: id.Name})
		}
		return true
	})
	return out
}
