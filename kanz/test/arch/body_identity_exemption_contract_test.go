package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A pre-authentication handler does not exempt its entire file. These files
// also register authenticated mutations. Resolve each registered handler through
// the same-package call graph; only the existing credential/challenge exchanges
// may omit a bearer/session read. Runtime authorization tests still establish
// whether that read is enforced before effects rather than merely reachable.
func TestBodyIdentityExceptionsKeepTheirAuthenticationBoundary(t *testing.T) {
	root := moduleRoot(t)
	for rel := range bodyIdentityExempt {
		p := parseDrainPackage(t, root, "services/"+filepath.ToSlash(filepath.Dir(rel)))
		for _, f := range p.files {
			if filepath.Base(p.fset.Position(f.Pos()).Filename) != filepath.Base(rel) {
				continue
			}
			if problems := bodyIdentityRouteProblems(p, f, rel); len(problems) > 0 {
				t.Errorf("%s: pre-authentication exception no longer fits its routes:\n  %s", rel, strings.Join(problems, "\n  "))
			}
		}
	}
}

func bodyIdentityRouteProblems(p *drainPkg, file *ast.File, rel string) []string {
	var problems []string
	routes := 0
	// Restrict receiver resolution to the Server being registered. An unrelated
	// type with a same-named method must not supply its authorization evidence.
	server := *p
	server.methods = map[string]map[string]*ast.FuncDecl{"Server": p.methods["Server"]}
	var walk func(ast.Node, map[string][]string)
	walk = func(node ast.Node, bindings map[string][]string) {
		ast.Inspect(node, func(n ast.Node) bool {
			if loop, ok := n.(*ast.RangeStmt); ok {
				values, literal := loop.X.(*ast.CompositeLit)
				name, named := loop.Value.(*ast.Ident)
				if literal && named {
					next := make(map[string][]string, len(bindings)+1)
					for k, v := range bindings {
						next[k] = v
					}
					for _, e := range values.Elts {
						if value, ok := literalString(e); ok {
							next[name.Name] = append(next[name.Name], value)
						}
					}
					walk(loop.Body, next)
					return false
				}
			}
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") {
				return true
			}
			patterns := bodyIdentityPatterns(call.Args[0], bindings)
			if len(patterns) == 0 {
				problems = append(problems, "unresolved route pattern at "+p.where(call.Pos()))
				return true
			}
			for _, pattern := range patterns {
				method, _, found := strings.Cut(pattern, " ")
				if found && (method == "GET" || method == "HEAD" || method == "OPTIONS") {
					continue
				}
				// The only methodless read handler is the SPA's file server. A
				// methodless proxy or new registration can receive writes and must
				// satisfy the same caller check as an explicit POST.
				if !found && pattern == "/" && renderSelector(call.Args[1]) == "s.static" && strings.HasPrefix(rel, "web-bff/") {
					continue
				}
				routes++
				allowed := bodyIdentityPreAuthHandler(rel, pattern)
				if allowed != "" && bodyIdentityHandlerRef(call.Args[1], allowed) {
					continue
				}
				if !bodyIdentityHandlerReadsCaller(&server, call.Args[1], rel) {
					problems = append(problems, fmt.Sprintf("%s neither matches its credential/challenge handler nor reaches a caller verification", pattern))
				}
			}
			return true
		})
	}
	walk(file, map[string][]string{})
	if routes == 0 {
		problems = append(problems, "found no mutating routes; the file exception is stale or the registration scan is blind")
	}
	sort.Strings(problems)
	return problems
}

func bodyIdentityPatterns(expr ast.Expr, bindings map[string][]string) []string {
	if value, ok := literalString(expr); ok {
		return []string{value}
	}
	if id, ok := expr.(*ast.Ident); ok {
		return bindings[id.Name]
	}
	if binary, ok := expr.(*ast.BinaryExpr); ok && binary.Op == token.ADD {
		left, right := bodyIdentityPatterns(binary.X, bindings), bodyIdentityPatterns(binary.Y, bindings)
		if len(left)*len(right) > 64 {
			return nil
		}
		var values []string
		for _, a := range left {
			for _, b := range right {
				values = append(values, a+b)
			}
		}
		return values
	}
	return nil
}

func bodyIdentityHandlerRef(expr ast.Expr, name string) bool {
	return bodyIdentityRouteMethod(expr) == name
}

// Resolve the handler expression, not an arbitrary selector somewhere in it.
// choose(s.authenticated, s.unchecked) must not credit the first argument to
// whichever handler the unknown wrapper actually returns.
func bodyIdentityRouteMethod(expr ast.Expr) string {
	if paren, ok := expr.(*ast.ParenExpr); ok {
		return bodyIdentityRouteMethod(paren.X)
	}
	if call, ok := expr.(*ast.CallExpr); ok {
		if wrapper, ok := call.Fun.(*ast.Ident); ok {
			if (wrapper.Name == "administrativeBody" && len(call.Args) == 1) ||
				(wrapper.Name == "boundedAdministrativeBody" && len(call.Args) == 2) {
				return bodyIdentityRouteMethod(call.Args[0])
			}
			return ""
		}
		// These two existing methods return a handler for one literal MFA mode.
		name := bodyIdentityRouteMethod(call.Fun)
		if (name == "mfaBegin" || name == "mfaFinish") && len(call.Args) == 1 {
			if _, ok := literalString(call.Args[0]); ok {
				return name
			}
		}
		return ""
	}
	if sel, ok := expr.(*ast.SelectorExpr); ok {
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "s" {
			return sel.Sel.Name
		}
	}
	return ""
}

func bodyIdentityHandlerReadsCaller(p *drainPkg, expr ast.Expr, rel string) bool {
	method := p.methods["Server"][bodyIdentityRouteMethod(expr)]
	if method == nil {
		return false
	}
	return p.reaches(method, func(call *ast.SelectorExpr) bool {
		name := renderSelector(call)
		if strings.HasPrefix(rel, "identity/") {
			return name == "s.provisioning.Verifier.Verify"
		}
		return name == "s.sessions.Get" || name == "s.sessions.List" || name == "s.sessions.Revoke"
	})
}

func bodyIdentityPreAuthHandler(rel, pattern string) string {
	if strings.HasPrefix(rel, "identity/") {
		switch pattern {
		// #364: subject/password and invitation proof establish identity.
		case "POST /login":
			return "login"
		case "POST /invites/redeem":
			return "redeem"
		// MFA and recovery authenticate a bounded challenge, not a bearer.
		case "POST /mfa/login/finish":
			return "mfaFinish"
		case "POST /mailbox/verify":
			return "verifyMailbox"
		case "POST /recovery":
			return "requestRecovery"
		case "POST /recovery/consume":
			return "resetCredential"
		}
	}
	if strings.HasPrefix(rel, "web-bff/") {
		switch pattern {
		// The browser tier delegates the same proofs to identity. Logout also
		// works for an absent/expired cookie and grants no new authority.
		case "POST /auth/login":
			return "handleCredentialLogin"
		case "POST /auth/redeem":
			return "handleRedeem"
		case "POST /auth/logout":
			return "handleLogout"
		case "POST /auth/mfa/login/finish":
			return "handleMFA"
		case "POST /auth/mailbox/verify", "POST /auth/recovery", "POST /auth/recovery/consume":
			return "handleRecovery"
		}
	}
	return ""
}

func TestBodyIdentityExceptionRejectsAnAdditionalUnauthenticatedMutation(t *testing.T) {
	const rel = "identity/internal/server/server.go"
	const source = `package server
func (s *Server) Routes(mux any) { mux.HandleFunc("POST /login", s.login) }
func (s *Server) login(w, r any) {}
func (s *Server) forged(w, r any) { /* s.provisioning.Verifier.Verify() is only prose */ }
func (s *Server) authenticated(w, r any) { s.provisioning.Verifier.Verify() }
`
	for _, tc := range []struct {
		name, from, to string
		want           bool
	}{
		{"credential", "POST /login", "POST /login", false},
		{"new-route-reusing-credential-handler", "POST /login", "POST /forged", true},
		{"replaced-handler", "s.login)", "s.forged)", true},
		{"methodless-route", "POST /login", "/changed", true},
		{"unknown-wrapper", "s.login)", "choose(s.authenticated, s.forged))", true},
		{"body-wrapper", "s.login)", "administrativeBody(s.login))", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, rel, strings.Replace(source, tc.from, tc.to, 1), 0)
			if err != nil {
				t.Fatal(err)
			}
			p := newDrainPkg(fset, "", []*ast.File{file})
			if got := len(bodyIdentityRouteProblems(p, file, rel)) > 0; got != tc.want {
				t.Fatalf("route mutation rejected=%v, want %v", got, tc.want)
			}
		})
	}
}
