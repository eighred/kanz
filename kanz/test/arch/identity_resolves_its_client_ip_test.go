package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// THE IDENTITY SERVICE MUST NOT READ ITS CALLER'S ADDRESS FROM A HEADER IT DOES
// NOT VERIFY (#888).
//
// # The premise that was false
//
// services/identity attributed every login attempt to X-Kanz-Client-IP, taken
// straight off the request. The comment on the constant justified that with the
// platform rule AGENTS.md states for every other upstream — "a NetworkPolicy
// makes the BFF the only caller that can reach this service".
//
// infra/security/runtime/network-policies.yaml says otherwise, in the same file
// and about the same port. allow-ingress-to-identity admits THREE peers to
// :8087 (the ingress-nginx namespace, the api-gateway pod, the web-bff pod) and
// allow-observability-scrape admits a fourth (the kanz-observability namespace).
// None is removable: the gateway's read of /jwks.json is what lets it verify any
// token at all, and /metrics is served on this same listener until #232 splits
// it. So the premise was not merely stale — it was never true for this service,
// and TestIdentityServiceTrustsNoPrincipalHeaders says so in its own words about
// the principal headers. That reasoning never stopped at X-Kanz-Principal-*.
//
// # What the unverified read cost
//
// allowAttempt bounds an unauthenticated attempt on two axes. The per-SUBJECT
// axis bounds guessing at one account and was never affected. The per-SOURCE
// axis is the one that bounds credential STUFFING — one guess sprayed across
// thousands of accounts, which is how leaked password lists are actually used —
// and it keys on this value. Read from an untrusted peer, a caller sends a
// different address per attempt, every attempt lands in a fresh bucket, and that
// axis stops existing while the metrics show a wide spread of well-behaved
// clients. The failure is invisible precisely because it looks like normal
// traffic.
//
// # Why a guard rather than a comment
//
// The repair is one call site, and the wrong version is the shorter, more
// obvious one: reading a header directly is what the code did for a year and
// what anyone adding a second attributed surface here would write first. It
// would also be silent — login keeps working, every existing test keeps passing,
// and the bound is gone. That is the shape this repository keeps paying for, and
// AGENTS.md's standing answer is that an invariant worth keeping is a guard.
//
// # What is checked
//
//  1. No production file under services/identity reads ClientIPHeader (or its
//     literal spelling) off a request header.
//  2. The literal "X-Kanz-Client-IP" appears exactly once in the service — the
//     constant's own declaration — so the read cannot come back spelled out.
//  3. The composition root builds a clientip.Resolver and hands it to the
//     server, because a resolver nobody wires is the same as no resolver and
//     reads identically from inside the package.
func TestIdentityResolvesTheClientAddressThroughATrustedPeerResolver(t *testing.T) {
	root := moduleRoot(t)

	// NON-VACUITY, half one: the two symbols this guard requires must exist. A
	// rename would otherwise turn every check below into a search for a string
	// that is gone, and the guard would pass on a service that trusted the header
	// again.
	for _, sym := range []struct{ dir, name string }{
		{"internal/clientip", "NewResolver"},
		{"services/identity/internal/server", "WithClientIP"},
	} {
		if !declaresFunc(t, filepath.Join(root, sym.dir), sym.name) {
			t.Fatalf("%s declares no func %s — this guard requires it by name, so a rename "+
				"leaves it checking for nothing. Point it at whatever replaced it.",
				sym.dir, sym.name)
		}
	}

	const headerConst = "ClientIPHeader"
	const headerLiteral = "X-Kanz-Client-IP"

	var (
		scanned      int
		literalSites []string
		problems     []string
	)

	// THE SCAN READS CODE, NOT PROSE. Files are parsed WITHOUT parser.ParseComments,
	// so the AST carries no comment text at all — the reasoning above names both
	// the constant and the literal, and a guard its own explanation can trip is a
	// guard people reword their way around.
	err := filepath.WalkDir(filepath.Join(root, "services", "identity"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skipWalkDir(d) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		scanned++
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)

		ast.Inspect(file, func(n ast.Node) bool {
			// (1) a header read keyed on this header, in any of its spellings.
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Get" || len(call.Args) != 1 {
				return true
			}
			// The receiver must itself be a .Header selection — r.Header.Get(x).
			inner, ok := sel.X.(*ast.SelectorExpr)
			if !ok || inner.Sel.Name != "Header" {
				return true
			}
			if namesTheHeader(call.Args[0], headerConst, headerLiteral) {
				problems = append(problems, rel+" reads "+headerConst+" straight off the request")
			}
			return true
		})

		// (2) every occurrence of the literal, so the const cannot be bypassed by
		// writing the string out.
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if v, uerr := strconv.Unquote(lit.Value); uerr == nil && v == headerLiteral {
				literalSites = append(literalSites, rel)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk services/identity: %v", err)
	}

	// NON-VACUITY, half two: the walk must have reached real files. A broken path
	// would otherwise report no problems on a service that had every one.
	if scanned < 5 {
		t.Fatalf("scanned only %d production Go files under services/identity — the walk is "+
			"broken, not the service", scanned)
	}
	// NON-VACUITY, half three: the literal must exist SOMEWHERE, or the header
	// this guard is about has been renamed out from under it.
	if len(literalSites) == 0 {
		t.Fatalf("the literal %q appears nowhere under services/identity — the header was "+
			"renamed and this guard now checks for a string that does not exist", headerLiteral)
	}
	if len(literalSites) > 1 {
		t.Errorf("the literal %q appears in %d places under services/identity:\n  %s\n\n"+
			"It must appear ONCE, in the ClientIPHeader declaration. A second occurrence is how "+
			"the constant gets bypassed: the resolver is configured with the constant and some "+
			"other code reads the string, so the two disagree and only one of them is peer-checked.",
			headerLiteral, len(literalSites), strings.Join(literalSites, "\n  "))
	}

	if len(problems) > 0 {
		t.Errorf("services/identity reads its caller's address from an unverified header:\n  %s\n\n"+
			"This service is NOT behind a policy that makes one caller its only reachable peer — "+
			"network-policies.yaml admits the ingress-nginx namespace, the api-gateway, the "+
			"web-bff and the kanz-observability namespace to :8087 — so a header arriving here is "+
			"a string the caller typed. Honouring it unconditionally gives every attempt a fresh "+
			"rate-limit bucket and ends the per-source half of the credential bound, which is the "+
			"half that bounds credential stuffing. Resolve through clientip.Resolver "+
			"(server.WithClientIP), which honours the header only from a configured peer and "+
			"answers the TCP peer otherwise.",
			strings.Join(problems, "\n  "))
	}

	// (3) THE COMPOSITION ROOT, which no unit test in this repository executes.
	// A resolver that exists and is never handed to the server leaves the header
	// ignored — safe, but the deployment believes it configured something, and
	// "configured and ignored" must not look like "configured".
	mainSrc := filepath.Join(root, "services", "identity", "cmd", "identity", "main.go")
	fset := token.NewFileSet()
	mainFile, perr := parser.ParseFile(fset, mainSrc, nil, parser.SkipObjectResolution)
	if perr != nil {
		t.Fatalf("parse %s: %v", mainSrc, perr)
	}
	wants := map[string]bool{"clientip.NewResolver": false, "server.WithClientIP": false}
	ast.Inspect(mainFile, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if _, tracked := wants[pkg.Name+"."+sel.Sel.Name]; tracked {
			wants[pkg.Name+"."+sel.Sel.Name] = true
		}
		return true
	})
	for name, found := range wants {
		if !found {
			t.Errorf("services/identity/cmd/identity/main.go never calls %s — the login limiter "+
				"then attributes every attempt to whatever the caller claims, or to nothing "+
				"configured at all. Both halves are required: building the resolver without "+
				"passing it to server.New leaves the server with no resolver, which reads "+
				"identically to a deployment that named no trusted peer.", name)
		}
	}
}

// namesTheHeader reports whether an argument expression is the ClientIPHeader
// constant or its literal value, in either package-qualified form.
func namesTheHeader(arg ast.Expr, constName, literal string) bool {
	switch e := arg.(type) {
	case *ast.Ident:
		return e.Name == constName
	case *ast.SelectorExpr:
		return e.Sel.Name == constName
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return false
		}
		v, err := strconv.Unquote(e.Value)
		return err == nil && v == literal
	}
	return false
}

// declaresFunc reports whether any non-test Go file directly in dir declares a
// top-level func with this name.
func declaresFunc(t *testing.T, dir, name string) bool {
	t.Helper()
	entries, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	for _, path := range entries {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name {
				return true
			}
		}
	}
	return false
}
