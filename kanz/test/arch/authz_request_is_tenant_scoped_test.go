package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// THE TWO HALVES OF #741, GUARDED AT THE CALL SITES.
//
// The copilot's tool gate resolved a portfolio's owning tenant and, on ANY error
// from that lookup, passed `Tenant: ""` into auth.Authorize. The authorizer read
// an empty resource tenant as "this resource is not tenant-scoped" and skipped
// the cross-tenant guard; portfolio scope and RBAC then passed on their own
// terms, and the tool read the data. A deadline on one dependency therefore
// switched off tenant isolation for that invocation — reachable through
// POST /v1/ask, which the gateway mounts at the baseline Read capability.
//
// Then the refusal leaked what the boundary was for: `"not authorized: " +
// dec.Reason`, and the authorizer's reason names the RESOURCE's owning tenant.
// So a t2 caller learned both that a portfolio existed and which tenant held it,
// from inside the message refusing them.
//
// Authorize now refuses a typed resource with no tenant outright, so the runtime
// half cannot regress. These two guards cover what a runtime check cannot: the
// call sites, where the mistake is made and where it reads as an omission rather
// than a decision. Both matter more than usual because pkg/auth is the package
// the MCP plane is to be generalized FROM — a hole left here is a hole copied.

const (
	// authzResourceType is the struct whose literals must name a tenant.
	authzResourceType = "Resource"
	// authzPkg is where Resource and Decision are declared. Excluded from both
	// scans: it is the definition site and tests there construct refusals on
	// purpose to assert they refuse.
	authzPkg = "pkg/auth"
)

// authzScanRoots are the trees searched. Everything that can import pkg/auth.
var authzScanRoots = []string{"pkg", "internal", "services", "cmd", "tools"}

// authzResourceExempt maps a module-relative file to the reason a typed
// auth.Resource literal in it may omit Tenant, and what retires the entry.
//
// IT IS EMPTY. There is no argued case: a literal that names a resource TYPE is
// by definition addressing somebody's data, and the tenant is either known — in
// which case write it — or unknown, in which case the empty string is a lie the
// authorizer used to believe.
var authzResourceExempt = map[string]string{}

// A typed auth.Resource literal must name the tenant that owns it.
func TestEveryTypedAuthzResourceNamesItsTenant(t *testing.T) {
	root := moduleRoot(t)

	var missing []string
	seenExempt := map[string]bool{}
	found := 0

	for _, gf := range authzParsedFiles(t, root) {
		ast.Inspect(gf.file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !authzIsResourceLit(lit.Type) {
				return true
			}
			keys := authzLiteralKeys(lit)
			// An UNKEYED literal names every field positionally, so a new field
			// is a compile error rather than a silent zero. Nothing to check.
			if keys == nil {
				return true
			}
			if !keys["Type"] {
				// Addresses no resource type: the tenant-agnostic shape, which
				// is exactly what stays allowed.
				return true
			}
			found++
			if keys["Tenant"] {
				return true
			}
			if reason, ok := authzResourceExempt[gf.rel]; ok {
				seenExempt[gf.rel] = true
				t.Logf("%s: exempt — %s", gf.rel, reason)
				return true
			}
			missing = append(missing, gf.rel)
			return true
		})
	}

	// NON-VACUITY: the estate authorizes portfolios, routes and datasets through
	// this struct. Finding none means the type moved or the match broke, and the
	// guard is asserting nothing at all.
	if found < 3 {
		t.Fatalf("found %d typed auth.Resource literal(s) across %v — expected at least 3 "+
			"(the copilot's portfolio, the gateway's route, lineage's dataset). The scan is broken, "+
			"not the estate", found, authzScanRoots)
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		missing = uniq(missing)
		t.Errorf("%d typed auth.Resource literal(s) do not name Tenant: %v.\n"+
			"An omitted Tenant is not 'tenant-agnostic' — it is the value a caller produces when its "+
			"ownership lookup FAILED, and the copilot produced it on every error, which turned a "+
			"dependency timeout into a cross-tenant read (#741). Authorize now refuses this at "+
			"runtime, so the omission is a refused request rather than a bypass; write the tenant "+
			"down so the refusal is not discovered in production.", len(missing), missing)
	}

	for f, reason := range authzResourceExempt {
		if !seenExempt[f] {
			t.Errorf("exemption for %q (%s) matches no offending literal — delete it", f, reason)
		}
	}
}

// authzReasonExempt maps a module-relative file to the reason it may put an
// authorization Decision's Reason somewhere other than a log line.
var authzReasonExempt = map[string]string{}

// An authorization Decision's Reason is audit-facing, and may only be LOGGED.
//
// Reason names the resource's owning tenant on a cross-tenant deny — that is
// deliberate, because the audit record has to reconstruct what the refusal was
// about. It is therefore the one field that must never be rendered back to the
// party being refused. This guard permits exactly the shape that keeps it in the
// audit plane: passed as an argument to a structured logger. Concatenating it,
// returning it, or storing it into a response struct is flagged.
func TestAuthzDecisionReasonOnlyEverReachesALog(t *testing.T) {
	root := moduleRoot(t)

	var leaks []string
	decisionIdents := 0
	logged := 0
	seenExempt := map[string]bool{}

	for _, gf := range authzParsedFiles(t, root) {
		ast.Inspect(gf.file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			decisions := authzDecisionIdents(fn)
			decisionIdents += len(decisions)
			if len(decisions) == 0 {
				return true
			}
			// Walk the body, tracking whether each .Reason read sits directly
			// under a logging call.
			authzWalkReasonUses(fn.Body, decisions, func(sel *ast.SelectorExpr, inLogCall bool) {
				if inLogCall {
					logged++
					return
				}
				if reason, ok := authzReasonExempt[gf.rel]; ok {
					seenExempt[gf.rel] = true
					t.Logf("%s: exempt — %s", gf.rel, reason)
					return
				}
				leaks = append(leaks, gf.rel+":"+fn.Name.Name)
			})
			return true
		})
	}

	// NON-VACUITY, BOTH WAYS. There must be authorization decisions to inspect,
	// and at least one of them must actually log its reason — a tree where the
	// reason is read nowhere at all would satisfy this guard while meaning the
	// audit trail had quietly lost the why.
	if decisionIdents < 2 {
		t.Fatalf("found %d authorization Decision value(s) across %v — expected at least 2 (the "+
			"copilot's tool gate and lineage's dataset gate). The scan is broken", decisionIdents, authzScanRoots)
	}
	if logged == 0 {
		t.Errorf("no caller logs its Decision's Reason. The caller-facing text is now a fixed " +
			"closed set, so the log line is the ONLY place an operator can read why a refusal " +
			"happened — sanitizing the message is only safe while that line exists")
	}

	if len(leaks) > 0 {
		sort.Strings(leaks)
		leaks = uniq(leaks)
		t.Errorf("%d site(s) put an authorization Decision's Reason somewhere other than a log: %v.\n"+
			"Reason names the RESOURCE's owning tenant on a cross-tenant deny. The copilot "+
			"concatenated it into the tool result the model reads, so a refused caller was told "+
			"both that the portfolio existed and which tenant owned it, by the refusal (#741). "+
			"Branch on Decision.Code and render your own fixed text; log the reason.", len(leaks), leaks)
	}

	for f, reason := range authzReasonExempt {
		if !seenExempt[f] {
			t.Errorf("exemption for %q (%s) matches no leak — delete it", f, reason)
		}
	}
}

// --- scanning helpers (authz-prefixed: package arch is one namespace) ---

type authzFile struct {
	rel  string
	file *ast.File
}

// authzParsedFiles parses every non-test Go file under the scan roots, skipping
// pkg/auth itself.
func authzParsedFiles(t *testing.T, root string) []authzFile {
	t.Helper()
	var out []authzFile
	fset := token.NewFileSet()
	for _, scan := range authzScanRoots {
		dir := filepath.Join(root, filepath.FromSlash(scan))
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", ".claude", "vendor", "node_modules", ".gotmp", "testdata":
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return rerr
			}
			slash := filepath.ToSlash(rel)
			if strings.HasPrefix(slash, authzPkg+"/") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if perr != nil {
				t.Fatalf("parse %s: %v — the guard cannot check what it cannot parse", slash, perr)
			}
			out = append(out, authzFile{rel: slash, file: f})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", scan, err)
		}
	}
	if len(out) == 0 {
		t.Fatalf("parsed zero files under %v — the scan is broken", authzScanRoots)
	}
	return out
}

// authzIsResourceLit reports whether a composite literal's type is
// auth.Resource (qualified — pkg/auth's own files are out of scope).
func authzIsResourceLit(typ ast.Expr) bool {
	sel, ok := typ.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != authzResourceType {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "auth"
}

// authzLiteralKeys returns the field names a keyed literal names, or nil if the
// literal is unkeyed (or empty).
func authzLiteralKeys(lit *ast.CompositeLit) map[string]bool {
	if len(lit.Elts) == 0 {
		return map[string]bool{}
	}
	keys := map[string]bool{}
	for _, e := range lit.Elts {
		kv, ok := e.(*ast.KeyValueExpr)
		if !ok {
			return nil // unkeyed: positional, and a compile error if a field is added
		}
		if id, ok := kv.Key.(*ast.Ident); ok {
			keys[id.Name] = true
		}
	}
	return keys
}

// authzDecisionIdents returns the names in fn that hold an authorization
// Decision: assigned from a .Authorize(...) call, or declared as an
// auth.Decision parameter.
func authzDecisionIdents(fn *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	if fn.Type.Params != nil {
		for _, p := range fn.Type.Params.List {
			sel, ok := p.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Decision" {
				continue
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "auth" {
				for _, n := range p.Names {
					out[n.Name] = true
				}
			}
		}
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Authorize" {
			return true
		}
		for _, lhs := range as.Lhs {
			if id, ok := lhs.(*ast.Ident); ok && id.Name != "_" {
				out[id.Name] = true
			}
		}
		return true
	})
	return out
}

// authzIsLogCall reports whether a call is a structured-logger call — the one
// destination a Decision's Reason may reach.
func authzIsLogCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	name := strings.TrimSuffix(sel.Sel.Name, "Context")
	switch name {
	case "Debug", "Info", "Warn", "Error", "Log", "With":
		return true
	}
	return false
}

// authzWalkReasonUses reports every `<decision>.Reason` read in body, saying
// whether it sits directly among a logging call's arguments.
func authzWalkReasonUses(body *ast.BlockStmt, decisions map[string]bool, visit func(*ast.SelectorExpr, bool)) {
	// Collect the .Reason selectors that ARE direct logging arguments first, so
	// the general walk can tell them apart by identity.
	inLog := map[*ast.SelectorExpr]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !authzIsLogCall(call) {
			return true
		}
		for _, arg := range call.Args {
			if sel, ok := arg.(*ast.SelectorExpr); ok && authzIsDecisionReason(sel, decisions) {
				inLog[sel] = true
			}
		}
		return true
	})
	ast.Inspect(body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || !authzIsDecisionReason(sel, decisions) {
			return true
		}
		visit(sel, inLog[sel])
		return true
	})
}

func authzIsDecisionReason(sel *ast.SelectorExpr, decisions map[string]bool) bool {
	if sel.Sel.Name != "Reason" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && decisions[id.Name]
}
