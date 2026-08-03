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

// A SERVICE THAT ASKS FOR THE CALLER MUST INSTALL THE THING THAT SUPPLIES ONE (#268).
//
// The api-gateway injects X-Kanz-Principal-Subject/Tenant/Roles on every
// forwarded request. For as long as that existed, NOTHING on the upstream side
// read them back: auth.WithPrincipal was called in exactly one non-test place,
// inside the gateway's own process. Two services asked for the caller anyway,
// through the correct accessor, and silently got nobody —
//
//	lineage  — server.go did `p, _ := auth.PrincipalFromContext(...)` and handed the
//	           nil straight to governance.CheckAccess, whose non-PII branch returned
//	           "public dataset — allow" before it ever looked at p. Every proxied
//	           read was served the whole non-PII lineage graph unauthenticated, and
//	           a real steward was simultaneously DENIED their own PII lineage,
//	           because the roles that would have granted it never arrived.
//	copilot  — handleAsk refused a nil principal, so /v1/ask answered 401 to every
//	           request that ever reached the service, and the end-user principal it
//	           forwards to lineage for PII governance was never there to forward.
//
// Neither produced an error, a missing import or a compile failure. #258's audit
// did not find them because it searched for DECLARATIONS OF THE HEADER, and these
// two services never mention it — they consume the context the header should have
// populated. That is the asymmetry this guard closes: TestPrincipalHeadersLiveOnlyInPkgAuth
// watches who may name the header; this watches who may depend on its effect.
//
// THE RULE: a service with a non-test file calling auth.PrincipalFromContext must
// have a non-test file installing auth.RequirePrincipal.
//
// SCOPE AND ITS LIMIT. Per-SERVICE, not per-file — the reader and the middleware
// installation are necessarily in different packages (a handler and its server's
// constructor), so a file-level rule could only ever be satisfied by putting them
// in the same file. The consequence is that this cannot tell a service that wraps
// the RIGHT mux from one that builds the middleware and drops it on the floor;
// what it can tell, and what actually went wrong, is a service that never had it
// at all. The per-service HTTP tests (lineage's server_test.go, copilot's) carry
// the rest, and are why this guard does not need to parse the wiring.
//
// services/ ONLY. pkg/auth defines both names, and the api-gateway populates its
// own context in-process through services/api-gateway/internal/middleware — a
// different Principal type reached by a different accessor, which is why it does
// not match and does not need an exemption.
const upstreamPrincipalScope = "services"

// upstreamPrincipalConsumer is a read of the SHARED principal off the context —
// the dependency that is worthless without the reconstruction. Matched on the
// AST, not on the text: the first draft of this guard used a regex, and it PASSED
// a mutation that deleted the middleware from lineage, because the struct field's
// comment still said "auth.RequirePrincipal". A guard whose subject is "does this
// code call X" must read code, or prose about the repair keeps the repair's
// permission alive after the repair is gone.
const upstreamPrincipalConsumer = "PrincipalFromContext"

// upstreamPrincipalInstall is the call that installs the reconstruction middleware.
const upstreamPrincipalInstall = "RequirePrincipal"

// upstreamPrincipalPkg is the package qualifier both must be reached through.
// A dot-import or a local alias would evade this; neither exists in this module
// and golangci-lint's revive would reject the dot-import.
const upstreamPrincipalPkg = "auth"

// callsAuthFunc reports whether f contains a CALL of auth.<name>(...) — a
// SelectorExpr in call position, so a comment, a string, or a mere mention in a
// doc block is not a match.
func callsAuthFunc(f *ast.File, name string) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == upstreamPrincipalPkg {
			found = true
			return false
		}
		return true
	})
	return found
}

// upstreamPrincipalExempt maps a service directory name to the reason it may read
// the shared principal without installing auth.RequirePrincipal, and the issue
// that retires the entry.
//
// IT IS EMPTY. Both services #268 found were wired rather than grandfathered. An
// exemption here says "this service authorizes against a caller that something
// other than the mesh middleware supplies" — which is a real possibility (a NATS
// consumer that builds its own principal is not an HTTP surface), and is exactly
// the claim that should have to be written down with a reason a reviewer sees.
var upstreamPrincipalExempt = map[string]string{}

func TestServicesReadingThePrincipalInstallTheReconstruction(t *testing.T) {
	root := moduleRoot(t)
	servicesDir := filepath.Join(root, upstreamPrincipalScope)

	var scanned int
	consumers := map[string][]string{} // service → files reading the principal
	installers := map[string]bool{}    // service → installs the middleware

	err := filepath.WalkDir(servicesDir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			if name := d.Name(); name == "testdata" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, servicesDir+string(os.PathSeparator)))
		svc, _, ok := strings.Cut(rel, "/")
		if !ok {
			return nil
		}
		scanned++

		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", rel, perr)
		}
		if callsAuthFunc(f, upstreamPrincipalConsumer) {
			consumers[svc] = append(consumers[svc], rel)
		}
		if callsAuthFunc(f, upstreamPrincipalInstall) {
			installers[svc] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", servicesDir, err)
	}

	// NON-VACUITY 1 — the walk ran. services/ holds 26 services and 252 non-test
	// .go files today; a scan that sees a handful is a broken path, and this guard
	// would then pass on a repository where nobody reconstructs anything.
	if scanned < 200 {
		t.Fatalf("scanned only %d non-test .go files under %s — expected at least 200. "+
			"The walk is broken and this guard is asserting nothing", scanned, servicesDir)
	}

	// NON-VACUITY 2 — the middleware still exists under the name this guard looks
	// for. Rename auth.RequirePrincipal and the install arm matches nothing, so
	// every consumer becomes a violation; DELETE it and the consumers are back to
	// the #268 state while the regex quietly matches nothing at all.
	meshHeader := filepath.Join(root, "pkg", "auth", "meshheader.go")
	src, rerr := os.ReadFile(meshHeader)
	if rerr != nil {
		t.Fatalf("read pkg/auth/meshheader.go: %v", rerr)
	}
	if !strings.Contains(string(src), "func RequirePrincipal(") ||
		!strings.Contains(string(src), "func PrincipalFromHeaders(") {
		t.Fatal("pkg/auth/meshheader.go no longer defines RequirePrincipal and PrincipalFromHeaders. " +
			"They are the upstream half of the trusted-header seam and the only thing that puts a " +
			"caller on an upstream's request context (#268). If they moved, move this guard with them; " +
			"if they were removed, every service behind the gateway is authorizing as nobody again.")
	}

	// NON-VACUITY 3 — services actually depend on the context principal. Two do
	// today (copilot, lineage). Zero means the surfaces that need an identity are
	// gone, or they have gone back to reading the raw header, and this guard is
	// watching nothing.
	if len(consumers) == 0 {
		t.Fatal("no service under services/ reads auth.PrincipalFromContext. Either the governed " +
			"read surfaces are gone or they now hand-read the header (which " +
			"TestPrincipalHeadersLiveOnlyInPkgAuth forbids); either way this guard is no longer " +
			"watching the upstream half of the identity seam")
	}

	var violations []string
	for svc, files := range consumers {
		if _, exempt := upstreamPrincipalExempt[svc]; exempt {
			continue
		}
		if installers[svc] {
			continue
		}
		sort.Strings(files)
		violations = append(violations, svc+" reads it in "+strings.Join(files, ", "))
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Errorf("these services authorize against auth.PrincipalFromContext but never install "+
			"auth.RequirePrincipal, so on a proxied request they get nil:\n  %s\n\n"+
			"Nil is not a caller with no grants. lineage's governor served every non-PII dataset to "+
			"it while denying a real steward their own PII lineage, and copilot 401'd every request "+
			"that reached it — for two years, with a green suite, because the tests handed the "+
			"principal in as an argument (#268).\n\n"+
			"Wrap the service's mux where it is BUILT, not in cmd/<svc>/main.go:\n"+
			"  s.handler = auth.RequirePrincipal(s.mux)\n"+
			"A composition root is the one place no unit test in the package runs.\n\n"+
			"If the principal on this service's context comes from somewhere other than the mesh "+
			"headers, add the service to upstreamPrincipalExempt with that reason and the issue "+
			"that retires it.",
			strings.Join(violations, "\n  "))
	}

	// DEAD ENTRIES: an exemption for a service that no longer exists, or no longer
	// reads the principal, is standing permission to authorize against nobody.
	var dead []string
	for svc, reason := range upstreamPrincipalExempt {
		if reason == "" {
			dead = append(dead, svc+" (exemption carries no reason)")
			continue
		}
		if _, err := os.Stat(filepath.Join(servicesDir, svc)); err != nil {
			dead = append(dead, svc+" (no such service)")
			continue
		}
		if len(consumers[svc]) == 0 {
			dead = append(dead, svc+" (no longer reads auth.PrincipalFromContext — exemption outlived the repair)")
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("upstreamPrincipalExempt has %d stale entr(y/ies):\n  %s\n\n"+
			"An exemption from the upstream identity check must not outlive the thing it excused.",
			len(dead), strings.Join(dead, "\n  "))
	}
}
