package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// AN ADAPTER THAT CAN TRADE MUST NAME WHO MAY ASK IT TO.
//
// Both venue adapters served venue.v1 under transport.AuthorizeMesh(), which
// accepts any peer presenting a valid SVID in kanz.internal. Issuing every
// workload an SVID is precisely what the trust domain does, so that authorizer
// admits copilot, tv-sync, market-data — anything with a pod. Execute submits
// an order to a live exchange.
//
// The property "only the OMS may trade" therefore rested entirely on a
// NetworkPolicy: one layer down, in a different directory, with nothing in the
// process itself refusing anyone. And that layer has never actually run —
// network-policies.yaml records that kindnetd does not enforce NetworkPolicy,
// and the only estate is a kind rig. So in practice the control existed on
// paper in both places and in force in neither.
//
// The narrower helper already existed and was already used twice, by the
// operator's control plane and the api-gateway's listener. This guard is what
// stops the adapters drifting back.
//
// # WHY THIS IDENTIFIES ADAPTERS BY WHAT THEY REGISTER
//
// Not by directory name. `services/venue-*` is a convention, and a third
// adapter that did not follow it would be exactly the one nobody remembered to
// add here. Registering venue.v1's server is the thing an order-placing adapter
// cannot avoid doing, so it is what the scan keys on — the same technique
// venue_live_endpoint_test.go uses for the same reason.
const venueServerRegistrar = "RegisterVenueAdapterServiceServer"

// allowedClientsEnvRe matches an actual env DECLARATION, so a mention of the key
// in a comment is not evidence that it is set.
var allowedClientsEnvRe = regexp.MustCompile(`(?m)^\s*-\s*name:\s*\S*_ALLOWED_CLIENTS\s*$`)

func TestEveryVenueAdapterNamesWhoMayTrade(t *testing.T) {
	root := moduleRoot(t)

	adapters := map[string]string{} // package dir -> module-relative path
	for _, f := range goFilesUnder(t, root) {
		if strings.HasSuffix(f.rel, "_test.go") || !strings.Contains(f.body, venueServerRegistrar) {
			continue
		}
		// The registrar's own declaration lives in the generated SDK; skip it and
		// anything under test/arch (this file names it in prose).
		if strings.HasPrefix(f.rel, "test/arch/") {
			continue
		}
		adapters[filepath.Dir(f.rel)] = f.rel
	}

	// NON-VACUITY. Two adapters are known to exist; a walk that finds none would
	// pass every assertion below while checking nothing.
	if len(adapters) < 2 {
		t.Fatalf("found %d package(s) registering %s — the estate has two venue adapters, so this "+
			"guard is asserting nothing", len(adapters), venueServerRegistrar)
	}

	for dir, rel := range adapters {
		mesh, services := authorizerCalls(t, filepath.Join(root, filepath.FromSlash(dir)))
		switch {
		case mesh:
			t.Errorf("%s serves venue.v1 under transport.AuthorizeMesh().\n\n"+
				"That admits ANY workload holding a valid SVID in kanz.internal — which is every "+
				"workload, because issuing them SVIDs is what the trust domain does. Execute "+
				"submits an order to a live exchange, so this makes 'only the OMS may trade' a "+
				"NetworkPolicy-only property, and kindnetd does not enforce NetworkPolicy. Use "+
				"transport.AuthorizeServices with an allow-list parsed by "+
				"transport.ParseServiceIDs, as the operator's control plane does.", rel)
		case !services:
			t.Errorf("%s registers %s but calls neither AuthorizeMesh nor AuthorizeServices.\n\n"+
				"An order-placing surface with no authorizer in sight is either unauthenticated "+
				"or authorized somewhere this guard cannot see; both need saying out loud.",
				rel, venueServerRegistrar)
		}
	}
}

// TestEveryVenueAdapterManifestNamesItsCallers is the deployment half.
//
// IT EXISTS BECAUSE THE SIBLING GUARD CANNOT SEE IT. spiffe_env_wiring_test.go
// matches env names containing the substring "SPIFFE", so VENUE_OKX_ALLOWED_-
// CLIENTS is invisible to it. Without this arm, a manifest that mounts the CSI
// socket and omits the allow-list stays green here and refuses to start on
// deploy — a trading outage introduced by a guard's blind spot rather than by
// the change it was guarding.
//
// Failing loudly is the intended runtime behaviour; failing loudly in CI first
// is better.
func TestEveryVenueAdapterManifestNamesItsCallers(t *testing.T) {
	root := moduleRoot(t)

	deployDir := filepath.Join(root, "infra", "deploy")
	entries, err := os.ReadDir(deployDir)
	if err != nil {
		t.Fatalf("read %s: %v", deployDir, err)
	}

	checked := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "venue-") || !strings.HasSuffix(e.Name(), "-deploy.yaml") {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(deployDir, e.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		body := string(raw)
		// Only a manifest that turns mTLS on needs an allow-list; a plaintext dev
		// posture is warned about at runtime and is not this guard's subject.
		if !strings.Contains(body, "SPIFFE_ENDPOINT_SOCKET") {
			continue
		}
		checked++
		// THE DECLARATION, NOT THE WORD. An earlier draft used a substring match
		// and stayed green when the env var was deleted, because this guard's own
		// prose names OPERATOR_ALLOWED_CLIENTS and the manifest comment beside the
		// variable names it too. Found by mutation, which is the only way that
		// class of defect is ever found.
		if !allowedClientsEnvRe.MatchString(body) {
			t.Errorf("infra/deploy/%s mounts the SPIFFE socket but names no *_ALLOWED_CLIENTS.\n\n"+
				"The adapter refuses to start without one, so this is a deploy-time outage on the "+
				"order path. Name the OMS: spiffe://kanz.internal/ns/kanz-services/sa/oms", e.Name())
		}
	}
	if checked < 2 {
		t.Fatalf("checked %d venue manifest(s) with a SPIFFE socket, want at least 2 — the walk "+
			"or the naming convention changed and this guard stopped seeing them", checked)
	}
}

// authorizerCalls reports which transport authorizers a package's non-test files
// call. Resolved from the AST, so the words in this file's own doc comment are
// not evidence about anything.
func authorizerCalls(t *testing.T, dir string) (mesh, services bool) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		parsed, perr := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if perr != nil {
			continue
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "transport" {
				return true
			}
			switch sel.Sel.Name {
			case "AuthorizeMesh":
				mesh = true
			case "AuthorizeServices":
				services = true
			}
			return true
		})
	}
	return mesh, services
}
