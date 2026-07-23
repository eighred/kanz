package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// SEC-M3b: the broker-identity contract, checked statically.
//
// infra/nats/nats.yaml requires a client SVID (`tls { verify: true,
// verify_and_map: true }`) and maps its SPIFFE URI SAN to a NATS user — and a
// user that is not in tenancy.yaml maps to NO ACCOUNT. So a service that dials
// the spine needs BOTH halves: the code must present an SVID (SEC-M3a's wire) and
// the broker must know that SVID. Either half alone is useless — an authenticated
// client that maps to no account cannot publish, and an account entry for a
// service that dials plaintext is never reached.
//
// This is the CONFIG-side twin of the wire. It exists because the two halves live
// in different languages, in different directories, and are changed by different
// people: nothing but a guard keeps a new NATS-dialing service from shipping with
// no account, which is exactly the shape of SEC-M3 (the whole platform dialed
// plaintext against a broker that refuses it, and every test was green).
//
// It is STATIC on purpose — the reason the subject-taxonomy guard is universal
// (KANZ_BRAIN.md): it needs no broker, covers every service at once, and cannot
// be skipped.

// systemAccountSVID is the identity SPIRE issues a platform service, per
// infra/security/spire/registration.yaml's spiffeIDTemplate
// (spiffe://kanz.internal/ns/{namespace}/sa/{serviceAccountName}) and the
// namespace + serviceAccountName every services/*-deploy.yaml declares.
func systemAccountSVID(svc string) string {
	return "spiffe://kanz.internal/ns/kanz-services/sa/" + svc
}

var userLine = regexp.MustCompile(`user:\s*"(spiffe://[^"]+)"`)

// systemAccountUsers returns the SVIDs the __system__ account admits. Platform
// services land here; per-tenant accounts are a separate boundary (MT-01c) and a
// tenant's workloads carry ns/tenant-{name} identities.
func systemAccountUsers(t *testing.T, path string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tenancy.yaml: %v", err)
	}
	users := map[string]bool{}
	inSystem := false
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "__system__") && strings.Contains(trimmed, "{") {
			inSystem = true
			continue
		}
		if !inSystem {
			continue
		}
		if m := userLine.FindStringSubmatch(line); m != nil {
			users[m[1]] = true
			continue
		}
		// The account block closes on a lone `}`; `users: [ ... ]` lines are
		// consumed above, so this cannot end the block early.
		if trimmed == "}" {
			inSystem = false
		}
	}
	return users
}

// operatorSVIDs maps a top-level cmd/ entrypoint that dials the spine to the
// SPIFFE ID its manifest gives it. These are the OPERATOR PLANE (SEC-M3c): they
// are not services, have no Deployment, and are not covered by the deployability
// guard — so they are named here explicitly, with the manifest that issues each.
//
// A cmd/ dialer that is NOT here fails the build. That is the point: kanz-halt
// dialed plaintext for the life of the project, and because the halt gate is
// deny-by-default that meant the production platform could not be RESUMED, let
// alone stopped. Nothing caught it, because nothing was looking at cmd/.
var operatorSVIDs = map[string]string{
	"kanz-halt": "spiffe://kanz.internal/ns/kanz-operator/sa/kanz-halt",
	// kanz-mandate arms the compliance gate — the same plane, the same broker,
	// the same requirement. It has no Job manifest yet, so it runs from an
	// operator's shell against a dev broker or reuses the kanz-halt SA's socket;
	// either way the code must present the SVID, which is what this asserts.
	"kanz-mandate": "spiffe://kanz.internal/ns/kanz-operator/sa/kanz-halt",
	// kanz-altevent publishes the commitment-lifecycle journal (ALT-01b) the
	// alternatives service folds. Same plane, same broker, same requirement, and
	// the same caveat as kanz-mandate above: no Job manifest of its own yet, so it
	// presents the kanz-halt SA's SVID.
	//
	// THE SHARED SA IS A KNOWN LEAST-PRIVILEGE GAP, not a design. Three operator
	// tools now present one identity, so the broker cannot tell a NAV mark from a
	// platform halt — anything trusted to publish a fund valuation is equally
	// trusted to stop trading. Splitting them needs a manifest and a tenancy entry
	// per tool; recorded here rather than silently widened a third time.
	"kanz-altevent": "spiffe://kanz.internal/ns/kanz-operator/sa/kanz-halt",
}

// dialsNATSInDir reports whether a directory tree calls bus.DialNATS.
// AST, not grep: a comment naming DialNATS is not a dial, and this codebase's
// prose discusses the calls it makes (retiring internal/integrity turned on
// exactly that distinction — see KANZ_BRAIN.md).
func dialsNATSInDir(t *testing.T, dir string) bool {
	t.Helper()
	found := false
	cmdDir := dir
	err := filepath.WalkDir(cmdDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "DialNATS" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "bus" {
				found = true
			}
			return true
		})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walk %s: %v", cmdDir, err)
	}
	return found
}

// dialsNATS reports whether a service's entrypoint tree calls bus.DialNATS.
func dialsNATS(t *testing.T, root, svc string) bool {
	t.Helper()
	return dialsNATSInDir(t, filepath.Join(root, "services", svc, "cmd"))
}

// TestOperatorCLIsHaveBrokerAccounts is the cmd/ half. The services guard above
// walks services/*/cmd, and every top-level cmd/ dialer fell through that gap —
// which is precisely where the worst instance lived: the KILL-SWITCH, the one
// tool that must work while the system is on fire, could not authenticate to the
// production broker.
//
// An operator CLI has no Deployment, so its identity comes from a Job manifest
// rather than a service manifest; operatorSVIDs names each one. A new cmd/ dialer
// must declare its SVID here and be admitted to __system__, or it does not ship.
func TestOperatorCLIsHaveBrokerAccounts(t *testing.T) {
	root := moduleRoot(t)
	users := systemAccountUsers(t, filepath.Join(root, "infra", "nats", "tenancy.yaml"))

	entries, err := os.ReadDir(filepath.Join(root, "cmd"))
	if err != nil {
		t.Fatalf("read cmd/: %v", err)
	}

	var problems []string
	seen := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() || !dialsNATSInDir(t, filepath.Join(root, "cmd", e.Name())) {
			continue
		}
		seen[e.Name()] = true
		svid, declared := operatorSVIDs[e.Name()]
		if !declared {
			problems = append(problems, e.Name()+": dials bus.DialNATS but declares no SVID in operatorSVIDs — "+
				"the production broker requires one, so this tool cannot reach the spine at all")
			continue
		}
		if !users[svid] {
			problems = append(problems, e.Name()+": declares "+svid+" but tenancy.yaml's __system__ does not admit it — "+
				"it would authenticate into no account")
		}
	}
	// An entry for a cmd that no longer dials is dead: it would keep an operator
	// account alive for nothing, and the next reader would believe it load-bearing.
	for name := range operatorSVIDs {
		if !seen[name] {
			problems = append(problems, name+": is in operatorSVIDs but cmd/"+name+" no longer dials NATS — remove it (dead entry)")
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("operator CLIs without a broker account (SEC-M3c):\n  %s", strings.Join(problems, "\n  "))
	}
}

// TestNATSDialersHaveBrokerAccounts asserts every DEPLOYED service that dials the
// spine has a __system__ user for the SVID SPIRE will issue it. A service without
// one authenticates and then maps to no account: it cannot publish or subscribe,
// and it finds out in production.
//
// Undeployed services are exempt via the SAME notDeployed map the deployability
// guard uses — deliberately not a second list. A service that gains a manifest
// becomes required here automatically, on the day it becomes deployable, with
// nobody having to remember this file exists.
func TestNATSDialersHaveBrokerAccounts(t *testing.T) {
	root := moduleRoot(t)
	users := systemAccountUsers(t, filepath.Join(root, "infra", "nats", "tenancy.yaml"))
	if len(users) == 0 {
		t.Fatal("no __system__ users parsed from tenancy.yaml — has the account block format changed?")
	}

	var problems []string
	for _, svc := range servicesWithEntrypoints(t, root) {
		if !dialsNATS(t, root, svc) {
			continue
		}
		if _, ok := notDeployed[svc]; ok {
			// Not deployed ⇒ no pod ⇒ no SVID to admit. If it IS admitted
			// anyway, that is an account the broker grants to nothing — say so,
			// so the list cannot rot the way an unowned allowlist does.
			if users[systemAccountSVID(svc)] {
				problems = append(problems, svc+": is exempt from deployment (notDeployed) yet holds a __system__ user — "+
					"remove the account entry or deploy it")
			}
			continue
		}
		if !users[systemAccountSVID(svc)] {
			problems = append(problems, svc+": dials bus.DialNATS but tenancy.yaml's __system__ account does not admit "+
				systemAccountSVID(svc)+" — it would authenticate into no account and be unable to publish")
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("NATS dialers without a broker account (SEC-M3b):\n  %s", strings.Join(problems, "\n  "))
	}
}
