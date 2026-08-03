package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// THE PLATFORM'S IDENTITY HEADER HAS ONE DECLARATION AND ONE ENFORCEMENT (#258).
//
// `X-Kanz-Principal-Tenant` is the whole authorization decision for every route
// behind the api-gateway. The gateway is the sole identity authority: it verifies
// the token and injects the header, and an upstream trusts it ONLY because a
// NetworkPolicy makes the gateway its only reachable caller (#232 tracks where
// that is not yet enforced).
//
// It reached FIVE independent declarations before this guard existed —
// api-gateway/internal/proxy (the injector), audit, tv-sync, wealth, datamaster —
// plus a sixth lowercase copy in copilot's lineage client. Each carried its own
// enforcement, and they had already begun to diverge: wealth and datamaster
// (#222) answered a wrong-tenant caller with a no-oracle 404, while audit and
// tv-sync predated that reasoning. CLAUDE.md names the end state of this exactly:
// "17 services each had their own secret() and 15 were wrong while 2 were right".
//
// The copies were not laziness. The canonical constants lived under
// services/api-gateway/internal/, which Go's internal rule makes unimportable by
// any other service, so there was nowhere shared to put them. pkg/auth is now
// that home, and it carries the CONSTANT AND THE CHECK TOGETHER — a service that
// can reach the name can reach the enforcement, so it cannot adopt one and
// reinvent the other.
//
// WHAT THIS GUARD FORBIDS, outside pkg/auth, in non-test Go:
//
//	ARM A — a string literal beginning "X-Kanz-Principal-". Checked on the AST, so
//	        a comment naming the header (there are several, deliberately) is not a
//	        violation; only code is.
//	ARM B — reaching into http.Header for a HeaderPrincipal* name directly, even
//	        the imported auth.HeaderPrincipalTenant. That is the "adopted the name,
//	        reinvented the check" move, and it is how the two response semantics
//	        drifted apart the first time. Go through auth.RequireCallerTenant,
//	        auth.RequireCallerTenantIs, auth.CallerTenant or auth.SetPrincipalHeaders.
//
// _test.go IS OUT OF SCOPE, and that is a real limit worth naming: a test may
// write the literal (several do, asserting the wire contract), and a
// re-declaration hidden in a _test.go would not be caught here. It also could not
// be imported by production code, which is why the trade is acceptable.

// principalHeaderPrefix is the wire prefix every mesh identity header shares.
// Matching the PREFIX rather than the three exact names is deliberate: a fourth
// header (`X-Kanz-Principal-Portfolios` has been discussed) must be born in
// pkg/auth, not discovered later in a service.
const principalHeaderPrefix = "X-Kanz-Principal-"

// principalHeaderHome is the one package allowed to declare and touch them,
// module-relative with forward slashes.
const principalHeaderHome = "pkg/auth"

// principalHeaderExempt maps a module-relative path (forward slashes) to the
// reason it may re-declare or hand-read the header, and the issue that retires
// the entry.
//
// IT IS EMPTY, AND THAT IS THE POINT. Every one of the six copies was migrated in
// #258 rather than grandfathered, because an exemption on this particular string
// is an exemption on the platform's only identity check. The map and the
// dead-entry arm below exist so that a future exemption has to be written down
// with a reason and an issue, in a diff a reviewer sees — not so that one is
// expected.
var principalHeaderExempt = map[string]string{}

// principalHeaderAccess matches a direct http.Header operation keyed on a
// HeaderPrincipal* constant — including the imported one. Get and Set are BOTH
// forbidden: the injector is one implementation of a concept too, and the day the
// wire contract gains a header or a canonicalisation rule, the gateway and
// copilot must get it for free.
var principalHeaderAccess = regexp.MustCompile(`Header\.(?:Get|Set|Add|Del|Values)\(\s*[A-Za-z0-9_.]*[Hh]eaderPrincipal[A-Za-z]*\s*[,)]`)

// sharedPrincipalHelper matches a call into the shared package — the shape a
// compliant consumer has. Used only by the non-vacuity floor.
var sharedPrincipalHelper = regexp.MustCompile(`auth\.(?:RequireCallerTenantIs|RequireCallerTenant|CallerTenant|SetPrincipalHeaders|HeaderPrincipal)`)

func TestPrincipalHeadersLiveOnlyInPkgAuth(t *testing.T) {
	root := moduleRoot(t)

	var (
		violations []string
		consumers  []string
		scanned    int
		homeDecls  = map[string]bool{}
	)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "vendor" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, root+string(os.PathSeparator)))
		scanned++

		src, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("read %s: %v", rel, rerr)
		}
		body := string(src)
		inHome := strings.HasPrefix(rel, principalHeaderHome+"/")

		// Parse without comments: ARM A must not fire on prose.
		f, perr := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", rel, perr)
		}
		var literals []string
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, uerr := strconv.Unquote(lit.Value)
			if uerr != nil || !strings.HasPrefix(v, principalHeaderPrefix) {
				return true
			}
			literals = append(literals, v)
			return true
		})

		if inHome {
			for _, v := range literals {
				homeDecls[v] = true
			}
			return nil
		}

		if _, exempt := principalHeaderExempt[rel]; exempt {
			return nil
		}
		if len(literals) > 0 {
			sort.Strings(literals)
			violations = append(violations, rel+" declares "+strings.Join(uniq(literals), ", "))
		}
		if m := principalHeaderAccess.FindString(body); m != "" {
			violations = append(violations, rel+" reads/writes the header itself: "+strings.TrimSpace(m))
		}
		if sharedPrincipalHelper.MatchString(body) {
			consumers = append(consumers, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	// NON-VACUITY 1 — the walk ran. The module has ~560 non-test .go files; a
	// scan that sees a handful is a broken path, and this guard would pass on a
	// repository full of copies.
	if scanned < 400 {
		t.Fatalf("scanned only %d non-test .go files under %s — expected at least 400. "+
			"The walk is broken and this guard is asserting nothing", scanned, root)
	}

	// NON-VACUITY 2 — the shared home actually declares the headers. If pkg/auth
	// stops declaring them, ARM A passes everywhere for the worst reason: the
	// concept was renamed and every service is free again.
	for _, want := range []string{
		principalHeaderPrefix + "Subject",
		principalHeaderPrefix + "Tenant",
		principalHeaderPrefix + "Roles",
	} {
		if !homeDecls[want] {
			t.Fatalf("%s does not declare %q. The shared home is the whole premise of this guard: "+
				"without it there is nothing for a service to import, and forbidding the local "+
				"declaration would just forbid the header. Restore it in "+
				"pkg/auth/meshheader.go, with its accessor.", principalHeaderHome, want)
		}
	}

	// NON-VACUITY 3 — services actually route through it. Six non-test files do
	// today: the api-gateway injector, copilot's lineage client, and the four
	// tenant-header-trusting services (audit, tv-sync, wealth, datamaster). Fewer
	// than five means either the consolidation has been unwound or the surfaces
	// have gone, and ARM A is passing because nobody uses the header at all.
	if len(consumers) < 5 {
		sort.Strings(consumers)
		t.Fatalf("only %d non-test file(s) call into pkg/auth for the principal headers %v — "+
			"expected at least 5. Either the shared helpers were bypassed or the surfaces that "+
			"depend on the gateway's identity injection are gone; either way this guard is no "+
			"longer watching the trust boundary", len(consumers), consumers)
	}

	sort.Strings(violations)
	if len(violations) > 0 {
		t.Errorf("these non-test files declare or hand-read a %s* header outside %s:\n  %s\n\n"+
			"That header IS the authorization decision for every route behind the api-gateway, and "+
			"a local copy is how a fix to it stops spreading — it was five copies with two different "+
			"answers to \"what does a wrong-tenant caller see\" before #258.\n\n"+
			"Use pkg/auth instead:\n"+
			"  auth.RequireCallerTenant(w, r)                       — multi-tenant scoping surface; "+
			"401 when unscoped, and the caller's tenant becomes the scope of the read (audit, tv-sync).\n"+
			"  auth.RequireCallerTenantIs(w, r, s.tenant, notFound) — single-tenant instance (#97); "+
			"404 with the SAME body a genuine miss returns, so id enumeration is not a cross-tenant "+
			"directory (wealth, datamaster).\n"+
			"  auth.SetPrincipalHeaders(h, subject, tenant, roles)  — minting/forwarding identity "+
			"outbound (api-gateway, copilot).\n\n"+
			"If none of those fit, the right move is to add the policy to pkg/auth beside the "+
			"others — not to re-derive it here. If it genuinely cannot live there, add the path to "+
			"principalHeaderExempt with the reason and the issue that retires it.",
			principalHeaderPrefix, principalHeaderHome, strings.Join(violations, "\n  "))
	}

	// DEAD ENTRIES: an exemption for a file that no longer exists, or that no
	// longer touches the header, is stale permission on the platform's identity
	// check.
	var dead []string
	for rel := range principalHeaderExempt {
		path := filepath.Join(root, filepath.FromSlash(rel))
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			dead = append(dead, rel+" (no such file)")
			continue
		}
		body := string(src)
		f, perr := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
		if perr != nil {
			dead = append(dead, rel+" (does not parse)")
			continue
		}
		touches := principalHeaderAccess.MatchString(body)
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if v, uerr := strconv.Unquote(lit.Value); uerr == nil && strings.HasPrefix(v, principalHeaderPrefix) {
				touches = true
			}
			return true
		})
		if !touches {
			dead = append(dead, rel+" (no longer declares or hand-reads the header — exemption outlived the repair)")
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("principalHeaderExempt has %d stale entr(y/ies):\n  %s\n\n"+
			"An exemption on the identity header must not outlive the thing it excused.",
			len(dead), strings.Join(dead, "\n  "))
	}
}

func uniq(ss []string) []string {
	out := ss[:0:0]
	for i, s := range ss {
		if i == 0 || s != ss[i-1] {
			out = append(out, s)
		}
	}
	return out
}
