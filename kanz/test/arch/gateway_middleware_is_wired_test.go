package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// EVERY GATEWAY MIDDLEWARE CONSTRUCTOR IS CALLED FROM THE COMPOSITION ROOT
// (#835).
//
// # The defect this exists to make impossible
//
// middleware.RateLimit was a complete per-key token bucket — lazy refill, its
// own bucket set, three green unit tests — and NOTHING in the module called it.
// The wired chain was Version → Signing → Auth → Measure → Quota → Idempotency,
// and Quota is per-TENANT so it runs after Auth. There was therefore no rate
// limit of any kind in front of authentication: every unauthenticated request
// reached an HMAC verification over its whole body and a token validation, with
// no bucket to exhaust first, on the process that is the sole entry point for
// POST /v1/orders.
//
// A reviewer reading middleware.go found a rate limiter and reasonably concluded
// the gateway was rate-limited. Every other gate agreed with them: it compiled,
// it vetted, it was covered by tests, and it lint-cleaned. Only "does the
// composition root call this?" separates a control from its costume, and no
// existing guard asked that of the edge chain. #229's
// store_capability_caller_test.go asks it of store interfaces; this is the same
// question one layer out.
//
// # WHY THE RETURN TYPE IS THE SELECTOR, RATHER THAN A LIST OF NAMES
//
// `func(http.Handler) http.Handler` is what it MEANS to be a link in this chain,
// so the set is derived from the source rather than transcribed into this file.
// A hand-written list is a second copy of the thing that broke: the next
// middleware would be added to the package, forgotten in the chain, and also
// forgotten here — and the guard would pass by not knowing about it.
//
// # WHAT A PASS DOES AND DOES NOT PROVE
//
// It proves the constructor is REFERENCED from package main under
// services/api-gateway/cmd. It does not prove the middleware is in the chain
// rather than, say, assigned and dropped, and it cannot prove the ORDER is right
// — that a limiter sits ahead of the authentication it protects. Those are
// asserted where they can be: cmd/api-gateway's
// TestBuildRouterBoundsAnUnauthenticatedFloodBeforeItReachesAuth drives the real
// router and counts how often the authenticator was asked. This guard's job is
// narrower and is the one nothing else covers: a middleware that is never
// mentioned at all.
func TestEveryGatewayMiddlewareIsWiredIntoTheChain(t *testing.T) {
	root := moduleRoot(t)
	pkgDir := filepath.Join(root, "services", "api-gateway", "internal", "middleware")
	rootDir := filepath.Join(root, "services", "api-gateway", "cmd")

	constructors := middlewareConstructors(t, pkgDir)
	// NON-VACUITY, half one. A moved or renamed package would make every
	// assertion below trivially true — the chain has six links today, so a scan
	// finding fewer than five is not measuring the package.
	if len(constructors) < 5 {
		t.Fatalf("found %d middleware constructors in %s — the package moved or the "+
			"`func(http.Handler) http.Handler` shape changed, and this guard is asserting nothing",
			len(constructors), pkgDir)
	}

	pkgRefs, methodRefs := compositionRootReferences(t, rootDir)
	// NON-VACUITY, half two. A composition root that parsed to nothing would make
	// every constructor look unreferenced, which fails loudly — but one that
	// parsed to a handful would pass whatever it happened to contain.
	if len(pkgRefs) < 10 {
		t.Fatalf("found %d middleware.* references under %s — the composition root moved, and "+
			"this guard cannot tell wired from unwired", len(pkgRefs), rootDir)
	}

	seenExempt := map[string]bool{}
	names := make([]string, 0, len(constructors))
	for n := range constructors {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		if reason, ok := gatewayMiddlewareExempt[name]; ok {
			seenExempt[name] = true
			t.Logf("exempt: middleware %s is not wired (%s)", name, reason)
			continue
		}
		referenced := pkgRefs[name]
		if constructors[name] { // a method: called on a value, not on the package
			referenced = methodRefs[name]
		}
		if !referenced {
			t.Errorf("middleware %s returns func(http.Handler) http.Handler and NOTHING under "+
				"services/api-gateway/cmd names it.\n"+
				"That is #835: a complete rate limiter lived in this package, called by nobody, "+
				"while the gateway had no limit at all in front of authentication — and every "+
				"other gate read it as a working control.\n"+
				"Wire it into buildRouter's chain, delete it, or add it to "+
				"gatewayMiddlewareExempt with the issue that retires it.", name)
		}
	}

	// DEAD-ENTRY ARM. An exemption that no longer matches a real constructor
	// outlived its repair, and would silently permit a future middleware that
	// happened to take the same name.
	for name := range gatewayMiddlewareExempt {
		if !seenExempt[name] {
			t.Errorf("gatewayMiddlewareExempt has %q, but no such middleware constructor exists — "+
				"the exemption outlived its repair; delete it", name)
		}
	}
}

// gatewayMiddlewareExempt is the default-deny allow-list of middleware
// constructors permitted to have no composition-root caller. Each entry needs
// the issue that retires it.
//
// IT IS EMPTY, AND THAT IS THE POINT. #835 closed with two removals rather than
// two exemptions: RateLimit (superseded by PreAuth, which is keyed on the source
// and wired ahead of Auth) and Idempotency (a convenience twin of
// IdempotencyWith, which is the form the composition root has always used
// because it can be handed the cross-pod claim store). An entry here is a
// declared gap; an unreferenced constructor with no entry is a false guarantee.
var gatewayMiddlewareExempt = map[string]string{}

// middlewareConstructors returns name → isMethod for every exported function in
// the package that returns exactly `func(http.Handler) http.Handler`.
func middlewareConstructors(t *testing.T, dir string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		fset := token.NewFileSet()
		// Mode 0: comments are not attached to the AST, so the paragraphs in this
		// package that NAME the removed RateLimit cannot be mistaken for a
		// declaration of it. A guard that greps raw source matches its own prose.
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || !fd.Name.IsExported() || !returnsHTTPMiddleware(fd.Type) {
				continue
			}
			out[fd.Name.Name] = fd.Recv != nil
		}
	}
	return out
}

// returnsHTTPMiddleware reports whether ft's sole result is
// `func(http.Handler) http.Handler`.
func returnsHTTPMiddleware(ft *ast.FuncType) bool {
	if ft.Results == nil || len(ft.Results.List) != 1 {
		return false
	}
	inner, ok := ft.Results.List[0].Type.(*ast.FuncType)
	if !ok {
		return false
	}
	return isOneHTTPHandler(inner.Params) && isOneHTTPHandler(inner.Results)
}

func isOneHTTPHandler(fl *ast.FieldList) bool {
	if fl == nil || len(fl.List) != 1 {
		return false
	}
	sel, ok := fl.List[0].Type.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Handler" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "http"
}

// compositionRootReferences parses every non-test .go file under dir and returns
// the names reached through the `middleware` package qualifier, plus every
// selector name used at all (which is how a method like Measure, called on a
// value, is seen).
func compositionRootReferences(t *testing.T, dir string) (pkg, method map[string]bool) {
	t.Helper()
	pkg, method = map[string]bool{}, map[string]bool{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			// The estate's one answer to "does this directory belong to us" —
			// .claude among them, so an agent worktree's copy of this composition
			// root can never be read as this one's.
			if skipWalkDir(d) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			method[sel.Sel.Name] = true
			if id, isIdent := sel.X.(*ast.Ident); isIdent && id.Name == "middleware" {
				pkg[sel.Sel.Name] = true
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return pkg, method
}
