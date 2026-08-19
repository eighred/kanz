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

// EVERY AUTHENTICATOR THIS GATEWAY BUILDS MUST BE SUBJECT TO REVOCATION (#532).
//
// # What this exists to stop
//
// Disabling an account used to take effect at the NEXT login. Nothing on the
// request path read account state — the gateway verifies a signature against
// JWKS and has no database at all — so an offboarded trader's outstanding token
// kept working for its full remaining lifetime, and so did one exfiltrated from
// a log, an operator's `curl` or a compromised pod. #532 closed that by putting
// middleware.Revoking between "this token verifies" and "this caller is
// admitted".
//
// # Why a guard and not a test
//
// The check is a DECORATOR, and a decorator protects only what it is wrapped
// around. The gateway has two authenticator arms today and will have more the
// day this estate federates; an arm added without the wrap authenticates
// perfectly, passes every unit test in the package, and silently honours a
// disabled account's token. Nothing fails. That is the same shape as #225 —
// where the production arm shipped without the caller's portfolio entitlement
// while the dev arm had a test — and the same shape as #535 and #539, where a
// control existed, was wired, and was in nobody's path.
//
// A per-arm test only ever covers the arms somebody remembered. This covers the
// ones they did not.
//
// # What it checks
//
// Every return from buildAuthenticator whose first result is not `nil` must
// produce that value from middleware.NewRevoking — directly, or through a
// variable assigned from it in the same function. Arms that legitimately have no
// revocation source are exempted BY NAME below, with the reason.
//
// # What it cannot check
//
//   - THAT THE CHECKER IS REAL. `middleware.NewRevoking(inner, x)` satisfies this
//     for any x. The constructor refuses a nil one and
//     TestAGatewayBuiltByTheCompositionRootRefusesARevokedToken puts a genuinely
//     revoked token through what buildAuthenticator returns; this guard is what
//     forces a new arm to name the wrap at all, at which point leaving it off is
//     a visible choice in a diff rather than an omission.
//   - REACHABILITY BEYOND THIS FUNCTION. If some future composition root builds
//     an authenticator somewhere else entirely, that construction is outside this
//     scan — which is why the non-vacuity floors below assert this function is
//     still the one place authenticators come from.
const revocationBuilderFile = "services/api-gateway/cmd/api-gateway"

// revocationBuilder is the function every gateway authenticator comes out of.
const revocationBuilder = "buildAuthenticator"

// revocationWrapper is the only constructor that produces a revocation-enforcing
// authenticator.
const revocationWrapper = "middleware.NewRevoking"

// revocationExempt maps a producer expression to the reason that arm carries no
// revocation check, and what would retire the entry.
//
// ONE ENTRY, AND IT IS THE REASON THE HS256 ARM MUST STAY UNREACHABLE WITHOUT
// API_GATEWAY_ALLOW_DEV_HS256. A dev rig runs no identity service, so there is
// no feed to check against; requiring one would make every local deployment
// permanently unready. The arm already declares itself as having no revocation
// path, in config.validateAuth (#242) and in its own startup WARN — this entry
// is the third place that says so, and the only one a compiler can enforce.
var revocationExempt = map[string]string{
	"middleware.NewJWTAuthenticator": "the dev HS256 arm — a symmetric secret with no identity " +
		"service behind it and therefore no revocation feed to read. Unreachable without " +
		"API_GATEWAY_ALLOW_DEV_HS256=true (#242). Retired if this estate ever runs identity in dev.",
}

func TestEveryGatewayAuthenticatorIsSubjectToRevocation(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, filepath.FromSlash(revocationBuilderFile))

	fset := token.NewFileSet()
	var fn *ast.FuncDecl
	for _, gf := range goFilesUnder(t, dir) {
		if strings.HasSuffix(gf.rel, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, revocationBuilderFile+"/"+gf.rel, gf.body, 0)
		if err != nil {
			t.Fatalf("parse %s/%s: %v — the guard cannot check what it cannot read",
				revocationBuilderFile, gf.rel, err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if ok && fd.Recv == nil && fd.Name.Name == revocationBuilder {
				fn = fd
			}
		}
	}
	if fn == nil {
		t.Fatalf("%s not found in %s — it was renamed or moved, and this guard is now asserting "+
			"nothing about a gateway that authenticates every caller in this estate",
			revocationBuilder, revocationBuilderFile)
	}

	// assignments maps an identifier to the producer expression it was assigned
	// from, so `revoking, err := middleware.NewRevoking(...); return revoking, …`
	// is recognised as the wrap it is.
	assignments := map[string]string{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		producer := exprName(call.Fun)
		if producer == "" {
			return true
		}
		for _, lhs := range as.Lhs {
			if id, ok := lhs.(*ast.Ident); ok && id.Name != "_" && id.Name != "err" {
				assignments[id.Name] = producer
			}
		}
		return true
	})

	var (
		problems   []string
		producers  []string
		seenExempt = map[string]bool{}
		wrapped    int
	)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || len(ret.Results) == 0 {
			return true
		}
		first := ret.Results[0]
		// `return nil, …` is a refusal, not an authenticator.
		if id, ok := first.(*ast.Ident); ok && id.Name == "nil" {
			return true
		}

		producer := ""
		switch e := first.(type) {
		case *ast.CallExpr:
			producer = exprName(e.Fun)
		case *ast.Ident:
			producer = assignments[e.Name]
		case *ast.CompositeLit:
			// A struct literal returned directly — an unwrapped adapter, which is
			// exactly what unwrapping the OIDC arm looks like. Named by its TYPE so
			// the failure below reports the arm rather than "untraceable".
			producer = exprName(e.Type)
		case *ast.UnaryExpr:
			if cl, ok := e.X.(*ast.CompositeLit); ok {
				producer = exprName(cl.Type)
			}
		}
		if producer == "" {
			// COUNTED AS A PRODUCER ANYWAY, so an unrecognised shape trips the
			// "not subject to revocation" arm below and not the non-vacuity floor.
			// The floor is there to catch a BROKEN SCAN; an arm this guard cannot
			// read is a finding, and reporting it as a broken scan would send the
			// next reader to fix the guard rather than the gateway.
			producers = append(producers, "<untraceable>")
			problems = append(problems, "a return whose authenticator this guard cannot trace to a "+
				"constructor (line "+fset.Position(ret.Pos()).String()+")")
			return true
		}
		producers = append(producers, producer)
		switch {
		case producer == revocationWrapper:
			wrapped++
		case revocationExempt[producer] != "":
			seenExempt[producer] = true
		default:
			problems = append(problems, producer+" (line "+fset.Position(ret.Pos()).String()+")")
		}
		return true
	})

	// NON-VACUITY, BOTH DIRECTIONS. If the return scan breaks, every arm passes
	// and this guard reports nothing while protecting nothing; if the wrap
	// detector breaks the other way, the exemption list would have to swallow the
	// function.
	if len(producers) < 2 {
		t.Fatalf("%s returns %d recognisable authenticators (%v) — expected at least 2 (the OIDC "+
			"arm and the dev HS256 arm). The scan is broken, not the gateway",
			revocationBuilder, len(producers), producers)
	}
	if wrapped == 0 {
		t.Fatalf("no arm of %s returns a %s authenticator. Every caller this gateway admits is "+
			"admitted without any check on whether their account has been disabled — a token "+
			"exfiltrated from a log outlives the disable by its full remaining lifetime (#532)",
			revocationBuilder, revocationWrapper)
	}

	sort.Strings(problems)
	if len(problems) > 0 {
		t.Errorf("%d authenticator arm(s) in %s are not subject to revocation:\n  %s\n\n"+
			"middleware.Revoking is a DECORATOR: it protects what it wraps and nothing else. An arm "+
			"returned unwrapped authenticates perfectly, passes every unit test in the package, and "+
			"silently honours the outstanding token of an account an operator has disabled — which "+
			"is #532 restored by an omission that looks like nothing in a diff.\n\n"+
			"Wrap it with %s, or add the producer to revocationExempt naming the reason it has no "+
			"feed to check against.",
			len(problems), revocationBuilder, strings.Join(problems, "\n  "), revocationWrapper)
	}

	// DEAD-ENTRY ARM. An exemption that outlives the arm it describes is a
	// standing permission to skip the check, and the next reader takes it as
	// evidence that somebody looked.
	for producer := range revocationExempt {
		if !seenExempt[producer] {
			t.Errorf("revocationExempt names %q, which %s no longer returns — delete the entry. A "+
				"stale exemption reads as a decision somebody made about the gateway as it is now",
				producer, revocationBuilder)
		}
	}
}

// exprName renders `pkg.Func` or `Func` for a call target, and "" for anything
// else (a method value, a closure) — which this guard treats as untraceable and
// therefore a problem, rather than quietly passing.
func exprName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		if pkg, ok := v.X.(*ast.Ident); ok {
			return pkg.Name + "." + v.Sel.Name
		}
	}
	return ""
}
