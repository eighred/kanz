package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
	depth := 0
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if !inSystem {
			if strings.HasPrefix(trimmed, "__system__") && strings.Contains(trimmed, "{") {
				inSystem = true
				depth = strings.Count(line, "{") - strings.Count(line, "}")
			}
			continue
		}
		if m := userLine.FindStringSubmatch(line); m != nil {
			users[m[1]] = true
		}
		// SEC-M3d: user entries now carry a nested `permissions: { publish: {
		// ... }, subscribe: { ... } }` block, so a lone `}` no longer
		// reliably marks the END of the __system__ account — it just as often
		// closes a permissions sub-block one line above a user entry's own
		// closing brace. Track brace depth instead: __system__'s block closes
		// only when depth returns to 0, however many nested braces it took to
		// get there.
		depth += strings.Count(line, "{") - strings.Count(line, "}")
		if depth <= 0 {
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
// SEC-M3d: these four USED to present ONE shared SVID (kanz-halt's), recorded
// as a known least-privilege gap — "the broker cannot tell a NAV mark from a
// platform halt". That framing undersold the real exposure: __system__ had NO
// permissions blocks at all, on ANY of its 17 users, so splitting the identity
// alone would have changed nothing — a permission-less user is exactly as
// privileged as every other permission-less user in the account, whether
// there are one, four, or seventeen of them.
//
// tenancy.yaml now gives each of these four its own SVID AND a restrictive
// `permissions` block scoped to the one subject space that tool exists to
// publish (platform.mode.changed / compliance.mandate.changed.> /
// alternatives.> / wealth.>), proven against a real broker — see
// .superpowers/sdd/operator-svid-split-report.md. That pairing, identity +
// permissions, is what makes the split mean something: kanz-mandate can no
// longer halt the platform, and kanz-halt can no longer forge a mandate.
//
// None of the three non-kanz-halt tools has its own Job manifest / ServiceAccount
// yet (see infra/operator/halt-job.yaml and infra/security/spire/registration.yaml)
// — SPIRE issues an SVID per (namespace, ServiceAccountName) a pod actually runs
// as, so today these three still authenticate as kanz-halt's SVID in practice
// until a manifest exists for each. The tenancy.yaml entries below are the
// broker-side half of the identity, ready for when that manifest half ships;
// this map is the code-side half of the same contract.
var operatorSVIDs = map[string]string{
	"kanz-halt":      "spiffe://kanz.internal/ns/kanz-operator/sa/kanz-halt",
	"kanz-mandate":   "spiffe://kanz.internal/ns/kanz-operator/sa/kanz-mandate",
	"kanz-altevent":  "spiffe://kanz.internal/ns/kanz-operator/sa/kanz-altevent",
	"kanz-household": "spiffe://kanz.internal/ns/kanz-operator/sa/kanz-household",
	// kanz-redrive is the DLQ drain (#220). It is an OPERATOR, not a read-only
	// observer, and the distinction is the whole point of the two maps: it
	// republishes parked commands onto live subjects, so it moves capital by
	// construction. See tenancy.yaml for why its publish allow-list is the widest
	// in this plane and why platform.> is excluded from it.
	"kanz-redrive": "spiffe://kanz.internal/ns/kanz-operator/sa/kanz-redrive",
}

// readOnlyObserverSVIDs is a DISTINCT category from operatorSVIDs, not an entry
// in it. A read-only observer dials the spine (so it trips the same cmd/-dialer
// guard operators do — TestOperatorCLIsHaveBrokerAccounts below) and binds an
// EPHEMERAL JetStream consumer to stream subjects to a terminal, but its
// tenancy.yaml grant DENIES all business publish: the publish allow-list holds
// ONLY the JetStream consumer machinery ($JS.API.>/$JS.ACK.>), so with an
// allow-list present every domain subject (order.*, risk.*, everything) is
// denied by default. It is admitted to the broker like an operator yet is
// PROVABLY incapable of moving capital — which is exactly why it must not be an
// operatorSVIDs entry: operators publish, and operatorSVIDs carries the publish
// authority the operator plane needs. TestReadOnlyObserversCannotPublish below
// makes "watches the loop, cannot move it" a guarded fact, not a comment.
//
// kanz-monitor is the read-only Bubble Tea TUI (cmd/kanz-monitor): it
// SubscribeBroadcasts order.> and risk.position.> and publishes NO business
// subject. Its manifest half (a kanz-operator ServiceAccount) is not yet
// shipped — like the operator tools, the broker-side half is declared in
// tenancy.yaml now, ready for when that manifest lands.
var readOnlyObserverSVIDs = map[string]string{
	"kanz-monitor": "spiffe://kanz.internal/ns/kanz-operator/sa/kanz-monitor",
}

// operatorServiceAccountFiles are every manifest that may declare a
// ServiceAccount in the kanz-operator namespace. kanz-halt's lives in
// halt-job.yaml (it owns that tool's whole manifest, Job included); the
// other three live in cli-identities.yaml (SEC-M3d — see that file's header
// for why they are a sibling, not an extension of halt-job.yaml). A new
// operator identity manifest must be added to this list or the guard below
// cannot see it and will falsely report the SVID as un-issuable.
var operatorServiceAccountFiles = []string{"halt-job.yaml", "cli-identities.yaml"}

var (
	k8sKindLine      = regexp.MustCompile(`^kind:\s*(\S+)\s*$`)
	k8sNameLine      = regexp.MustCompile(`^\s*name:\s*(\S+)\s*$`)
	k8sNamespaceLine = regexp.MustCompile(`^\s*namespace:\s*(\S+)\s*$`)
)

// operatorServiceAccounts parses every YAML document in
// infra/operator/{operatorServiceAccountFiles} and returns the
// "namespace/name" of each ServiceAccount object declared. It is a plain
// line scanner, not a YAML library, matching the style systemAccountUsers
// above already uses on tenancy.yaml — these manifests are simple and
// hand-written, and a real parser is not needed to see "this Kind exists
// with this metadata".
func operatorServiceAccounts(t *testing.T, operatorDir string) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	for _, fname := range operatorServiceAccountFiles {
		path := filepath.Join(operatorDir, fname)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, doc := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n---\n") {
			isServiceAccount := false
			name, namespace := "", ""
			for _, line := range strings.Split(doc, "\n") {
				if m := k8sKindLine.FindStringSubmatch(line); m != nil {
					isServiceAccount = m[1] == "ServiceAccount"
				}
				if name == "" {
					if m := k8sNameLine.FindStringSubmatch(line); m != nil {
						name = m[1]
					}
				}
				if namespace == "" {
					if m := k8sNamespaceLine.FindStringSubmatch(line); m != nil {
						namespace = m[1]
					}
				}
			}
			if isServiceAccount && name != "" && namespace != "" {
				found[namespace+"/"+name] = true
			}
		}
	}
	return found
}

// TestOperatorCLIsHaveServiceAccounts is the manifest-side half of SEC-M3d.
// SPIRE issues an SVID for the (namespace, ServiceAccountName) a pod (or a
// `go run` process reusing the node's SPIRE agent socket) actually runs as —
// registration.yaml's ClusterSPIFFEID is a namespace-wide template keyed on
// exactly that pair, with no per-tool registration entry to add. So an
// operator SVID named in tenancy.yaml's __system__ account with no matching
// ServiceAccount manifest is not a paperwork gap: it is never issued at all.
// The tool falls back to whatever ServiceAccount it actually runs as — on
// this rig, kanz-halt's — and inherits THAT identity's permissions, which is
// the halt subject alone. The tool authenticates fine and is then denied
// every publish it exists to make. That failure shows up as "this tool is
// broken", not "this manifest is missing", which is what makes it dangerous:
// nothing about it looks like a deploy problem.
func TestOperatorCLIsHaveServiceAccounts(t *testing.T) {
	root := moduleRoot(t)
	operatorDir := filepath.Join(root, "infra", "operator")
	serviceAccounts := operatorServiceAccounts(t, operatorDir)

	var problems []string
	// readOnlyObserverSVIDs is DELIBERATELY not iterated here: a read-only
	// observer (kanz-monitor) has no operator Job manifest and the plan does not
	// ask for one, so requiring a ServiceAccount for it would fail the build for
	// a manifest this task's scope does not ship. The residual — that its
	// broker-side grant is declared ahead of that manifest — is recorded on the
	// tenancy.yaml entry itself, so this omission is explicit, not an oversight.
	for name, svid := range operatorSVIDs {
		// svid is spiffe://kanz.internal/ns/{namespace}/sa/{serviceAccountName};
		// registration.yaml's spiffeIDTemplate constructs it from exactly that
		// pair, so this is the same string reversed, not a guess.
		const prefix = "spiffe://kanz.internal/ns/"
		rest := strings.TrimPrefix(svid, prefix)
		parts := strings.SplitN(rest, "/sa/", 2)
		if len(parts) != 2 {
			t.Fatalf("operatorSVIDs[%q] = %q does not match spiffe://kanz.internal/ns/{namespace}/sa/{name}", name, svid)
		}
		namespace, saName := parts[0], parts[1]
		if !serviceAccounts[namespace+"/"+saName] {
			problems = append(problems, name+": tenancy.yaml's __system__ account admits "+svid+
				" with its own permissions block, but no ServiceAccount named "+saName+" in namespace "+namespace+
				" exists under infra/operator/ — SPIRE issues an SVID per (namespace, ServiceAccountName) a workload "+
				"actually runs as, so without this ServiceAccount the SVID above is never issued. "+name+
				" falls back to whatever identity it actually runs as instead and is denied every publish it exists "+
				"to make — it will look like a broken tool, not a missing manifest")
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("operator SVIDs without a ServiceAccount (SEC-M3d):\n  %s", strings.Join(problems, "\n  "))
	}
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
		// A cmd/ dialer is acceptable if it declares an SVID in EITHER plane:
		// operatorSVIDs (operators, which publish) or readOnlyObserverSVIDs
		// (read-only observers, whose grant denies business publish). Both map
		// to a __system__ user that must be admitted; a name in NEITHER map is
		// the real failure this guard exists to catch.
		svid, declared := operatorSVIDs[e.Name()]
		if !declared {
			svid, declared = readOnlyObserverSVIDs[e.Name()]
		}
		if !declared {
			problems = append(problems, e.Name()+": dials bus.DialNATS but declares no SVID in operatorSVIDs or "+
				"readOnlyObserverSVIDs — the production broker requires one, so this tool cannot reach the spine at all")
			continue
		}
		if !users[svid] {
			problems = append(problems, e.Name()+": declares "+svid+" but tenancy.yaml's __system__ does not admit it — "+
				"it would authenticate into no account")
		}
	}
	// An entry in EITHER plane for a cmd that no longer dials is dead: it would
	// keep a broker account alive for nothing, and the next reader would believe
	// it load-bearing.
	for name := range operatorSVIDs {
		if !seen[name] {
			problems = append(problems, name+": is in operatorSVIDs but cmd/"+name+" no longer dials NATS — remove it (dead entry)")
		}
	}
	for name := range readOnlyObserverSVIDs {
		if !seen[name] {
			problems = append(problems, name+": is in readOnlyObserverSVIDs but cmd/"+name+" no longer dials NATS — remove it (dead entry)")
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

// readOnlyObserverMachinery is the exact, closed set of subjects a read-only
// observer's tenancy.yaml publish allow-list may name — and nothing else. These
// are the JetStream consumer machinery its ephemeral consumer binds ($JS.API.>
// to resolve the stream + create the consumer, $JS.ACK.> to ack) plus the
// reply-inbox space; none of them is a business/domain subject, so an
// allow-list drawn only from this set cannot move capital. _INBOX.> is a
// subscribe-side subject in practice and will not normally appear in a publish
// allow-list, but is admitted here so the assertion stays about "only
// machinery" rather than an over-narrow literal match.
var readOnlyObserverMachinery = map[string]bool{
	"$JS.API.>": true,
	"$JS.ACK.>": true,
	"_INBOX.>":  true,
}

// TestReadOnlyObserversCannotPublish is the whole point of the read-only
// observer category, encoded as a guard: every readOnlyObserverSVIDs member's
// tenancy.yaml publish grant must be PROVABLY incapable of a business publish.
// It asserts, for each observer, that (1) an allow-list is actually present,
// (2) every entry in it is one of the JetStream machinery subjects
// (readOnlyObserverMachinery) — i.e. NO domain/business subject is publishable —
// and (3) there is no publish.deny shortcut at all, which also rules out the
// `deny: [">"]` trap that would break the consumer's own $JS.API.>/$JS.ACK.>
// machinery (see the risk-engine/archiver headers in tenancy.yaml). An
// allow-list restricts by omission: with these entries present, order.*, risk.*
// and every other subject is denied by default, which is what makes "watches the
// loop, cannot move it" a guarded fact.
func TestReadOnlyObserversCannotPublish(t *testing.T) {
	root := moduleRoot(t)
	perms := servicePublishPermissions(t, filepath.Join(root, "infra", "nats", "tenancy.yaml"))

	var problems []string
	observers := 0
	allowEntries := 0
	for name, svid := range readOnlyObserverSVIDs {
		perm, ok := perms[svid]
		if !ok {
			problems = append(problems, name+": tenancy.yaml has no `permissions` block for "+svid+
				" — an absent block is UNRESTRICTED within __system__ (NATS semantics), the exact opposite of a "+
				"read-only observer; it must carry a machinery-only publish allow-list")
			continue
		}
		observers++
		if len(perm.deny) > 0 {
			problems = append(problems, name+": tenancy.yaml's permissions.publish for "+svid+" carries a `deny` list "+
				"("+strings.Join(perm.deny, ", ")+") — a read-only observer must restrict by an allow-list ONLY. In "+
				"particular `deny: [\">\"]` would deny the $JS.API.>/$JS.ACK.> its own ephemeral consumer binds and ack, "+
				"silently breaking the consumer (every ack denied, infinite redelivery); a deny list also risks masking "+
				"a broad allow")
		}
		if len(perm.allow) == 0 {
			problems = append(problems, name+": tenancy.yaml's permissions.publish for "+svid+" has no allow entries — an "+
				"explicitly empty publish allow-list was proven not to restrict anything on this nats-server (see "+
				"tenancy.yaml's trap note), so an observer with nothing but machinery to publish must still list that "+
				"machinery explicitly")
			continue
		}
		for _, subj := range perm.allow {
			allowEntries++
			if !readOnlyObserverMachinery[subj] {
				problems = append(problems, name+": tenancy.yaml's permissions.publish for "+svid+" allows "+
					strconv.Quote(subj)+", which is NOT JetStream consumer machinery — a read-only observer may publish "+
					"only $JS.API.>/$JS.ACK.> (its own consumer transactions) and provably no business subject, so this "+
					"grant would let it move capital")
			}
		}
	}

	// Non-vacuity: the estate has at least one observer (kanz-monitor) with a
	// non-empty machinery allow-list. A scan that parsed zero observers or zero
	// allow entries proves nothing about what an observer can publish, which is
	// the one thing this guard exists to prove.
	if observers == 0 || allowEntries == 0 {
		t.Fatalf("parsed %d observer(s) and %d publish-allow entr(y/ies) from tenancy.yaml — the scanner or the "+
			"readOnlyObserverSVIDs list is broken; this assertion cannot vacuously pass", observers, allowEntries)
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%d read-only observer(s) hold a publish grant that is not machinery-only:\n\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// groupedSubscribeCallsInDir AST-walks dir for grouped-Subscribe call sites.
// Modeled on dialsNATSInDir above (go/parser + ast.Inspect, skip _test.go
// files, skip dirs on walk error) — root is threaded through only so the
// call sites it finds can be reported relative to the module root, the way
// every other scanner in this package reports them; dialsNATSInDir needs no
// such parameter because it returns a bool, not paths.
//
// It matches a *ast.CallExpr whose Fun is a *ast.SelectorExpr with
// Sel.Name == "Subscribe" AND len(call.Args) == 4.
//
// RATIONALE: the bus grouped subscribe signature is
// Subscribe(ctx, subject, group string, h EventHandler) = 4 args (both
// *bus.Consumer.Subscribe and *bus.NATSClient.Subscribe); SubscribeBroadcast
// is 3 args and SubscribeBroadcastReady is 4 args but named differently, so
// name=="Subscribe" && arity==4 catches exactly the grouped subscribe and
// nothing else — this is the same arity-based disambiguation
// natsClientSubscribeMethodArity/directNATSClientSubscribeCalls
// (nats_jetstream_machinery_test.go) already relies on to avoid false
// positives like tv-sync's 2-arg Projection.Subscribe.
//
// Returns "file:line" strings, file relative to the module root and
// forward-slashed, matching the style of the other scanners in this package.
func groupedSubscribeCallsInDir(t *testing.T, root, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		rel := filepath.ToSlash(mustRel(root, path))
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Subscribe" || len(call.Args) != 4 {
				return true
			}
			out = append(out, rel+":"+strconv.Itoa(fset.Position(call.Pos()).Line))
			return true
		})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

// TestReadOnlyObserversNeverJoinAQueueGroup is the source-level guard for the
// property readOnlyObserverSVIDs exists to hold: a read-only observer
// subscribes via consumer.SubscribeBroadcast ONLY — an EPHEMERAL,
// per-connection JetStream consumer that gets its OWN copy of every event —
// and NEVER via a grouped consumer.Subscribe(ctx, subject, group, handler).
// A grouped Subscribe from an observer would JOIN the durable consumer
// group the real OMS/tv-sync replicas share on that subject and STEAL
// deliveries meant for them — a capital-path incident, not a UI bug (see
// busreader.go's own SubscribeBroadcast comments in cmd/kanz-monitor for the
// same property described at the call site).
//
// Before this guard, "SubscribeBroadcast only" was verified by grep alone —
// a plan-level check, not a build-time one. A regression to a grouped
// Subscribe would still compile, still dial, still decode, and pass the rest
// of this test suite, because nothing at the source level distinguished
// SubscribeBroadcast from Subscribe. This is that source-level guard: it
// AST-walks every listed observer's cmd/ tree for a grouped-Subscribe call
// site and fails the build on the first one, naming the file:line.
//
// GENERALIZED, not kanz-monitor-specific: this iterates readOnlyObserverSVIDs
// itself rather than naming kanz-monitor directly, so any future read-only
// observer added to that map is covered automatically, with no second guard
// to remember to write.
func TestReadOnlyObserversNeverJoinAQueueGroup(t *testing.T) {
	root := moduleRoot(t)

	var problems []string
	checked := 0
	for name := range readOnlyObserverSVIDs {
		dir := filepath.Join(root, "cmd", name)

		// A typo'd observer name (or one whose cmd/ tree was removed/renamed)
		// must fail loudly here rather than let the scan below run over a
		// missing or empty directory and vacuously find nothing to complain
		// about.
		info, statErr := os.Stat(dir)
		if statErr != nil || !info.IsDir() {
			t.Fatalf("readOnlyObserverSVIDs[%q]: cmd/%s does not exist — the observer name or directory is wrong; "+
				"this guard cannot scan an observer it cannot find", name, name)
		}
		goFiles := 0
		walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(path, ".go") {
				goFiles++
			}
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk cmd/%s: %v", name, walkErr)
		}
		if goFiles == 0 {
			t.Fatalf("readOnlyObserverSVIDs[%q]: cmd/%s contains no .go files — the observer name or directory is "+
				"wrong; a scanner over an empty tree would pass vacuously", name, name)
		}

		checked++
		// SCAN THE OBSERVER'S WHOLE CODE, NOT ITS DIRECTORY (#67).
		//
		// This used to walk cmd/<name>/ alone, which silently stopped covering
		// the observer the moment any of its code moved out — and #67 is exactly
		// that move: "fold kanz-monitor in". Demonstrated before the fix: a
		// grouped Subscribe planted in internal/tui/monitor/ (where a fold would
		// naturally put the bus reader) PASSED this guard, because cmd/kanz-monitor/
		// still existed, still held .go files, and no longer held the subscribe.
		//
		// All three vacuity checks above kept passing while the property they
		// protect had left the building. The guard #67 calls "the acceptance
		// criteria, not a formality" would have become a formality on the first
		// commit that implemented #67.
		//
		// So the scan follows first-party imports transitively from the binary.
		// Moving the code no longer moves it out of view.
		for _, site := range groupedSubscribeCallsInPackages(t, root, observerPackageDirs(t, root, dir)) {
			problems = append(problems, name+": "+site+" calls a grouped Subscribe(ctx, subject, group, handler) — "+
				"this JOINS the durable consumer group the real OMS/tv-sync replicas share on that subject and "+
				"STEALS deliveries from them. A read-only observer must use SubscribeBroadcast only.")
		}
	}

	// NON-VACUITY: a guard that checked zero observers would pass no matter how
	// wrong an observer's subscribe call was — mirrors the checked==0 pattern
	// nats_jetstream_machinery_test.go's TestServiceHasJetStreamMachineryItsRoleRequires
	// already uses for the same reason. The per-observer os.Stat/goFiles==0
	// checks above cover the OTHER vacuity mode: a listed observer whose
	// directory exists but contains nothing to scan.
	if checked == 0 {
		t.Fatal("checked zero read-only observers for grouped-Subscribe call sites — the scanner or " +
			"readOnlyObserverSVIDs is broken; this guard cannot vacuously pass")
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%d read-only observer(s) join a queue group (steal-nothing violation):\n\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// subscribePrimitivePackages DEFINE Subscribe and SubscribeBroadcast rather than
// choosing between them.
//
// pkg/bus is the library: Consumer.Subscribe's body (consumer.go:148) delegates
// to c.subscriber.Subscribe, which is the grouped primitive existing so that
// SERVICES can join a durable consumer group — that is its job. Flagging it
// would report the bus for providing the call every real consumer needs, and the
// only way to make the guard green again would be to weaken it.
//
// The property this guard protects is the OBSERVER'S CHOICE: kanz-monitor must
// call SubscribeBroadcast and never Subscribe. That choice is made in the
// observer's own packages, which are still walked in full.
var subscribePrimitivePackages = map[string]bool{
	modulePath + "/pkg/bus": true,
}

// observerPackageDirs returns the observer's own package directory plus every
// FIRST-PARTY package it reaches, transitively.
//
// This is what makes the read-only guarantee follow the code instead of a path
// (#67). A read-only observer that subscribes through a helper package is still
// a read-only observer, and a fold that relocates that helper must not relocate
// it out of the guard's sight.
//
// modulePath (risk_boundary_test.go) is this module's import prefix; reusing it
// rather than declaring a second copy, which is how the two would drift apart on
// the next module rename.
//
// Third-party imports are deliberately not followed: pkg/bus's own definition of
// Subscribe is not a call site, and walking the module cache would make this
// guard slow and noisy for no added safety.
func observerPackageDirs(t *testing.T, root, start string) []string {
	t.Helper()

	seen := map[string]bool{}
	var order []string
	queue := []string{start}

	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		if seen[dir] {
			continue
		}
		seen[dir] = true
		order = append(order, dir)

		entries, err := os.ReadDir(dir)
		if err != nil {
			// A package that cannot be read is not silently skipped: the whole
			// point of this guard is that it never scans less than it claims.
			t.Fatalf("read %s while resolving the observer's packages: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if perr != nil {
				t.Fatalf("parse %s: %v", path, perr)
			}
			for _, spec := range f.Imports {
				imp, uerr := strconv.Unquote(spec.Path.Value)
				if uerr != nil || !strings.HasPrefix(imp, modulePath+"/") {
					continue
				}
				if subscribePrimitivePackages[imp] {
					continue
				}
				next := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(imp, modulePath+"/")))
				if info, serr := os.Stat(next); serr == nil && info.IsDir() && !seen[next] {
					queue = append(queue, next)
				}
			}
		}
	}
	return order
}

// groupedSubscribeCallsInPackages runs the existing per-directory scan over each
// package, non-recursively — observerPackageDirs already enumerated them, and
// recursing again would pull in unrelated sibling packages that merely sit
// beneath one the observer imports.
func groupedSubscribeCallsInPackages(t *testing.T, root string, dirs []string) []string {
	t.Helper()
	if len(dirs) == 0 {
		t.Fatal("resolved zero packages for a read-only observer — a scan over nothing passes vacuously")
	}
	var out []string
	for _, dir := range dirs {
		out = append(out, groupedSubscribeCallsInDir(t, root, dir)...)
	}
	return out
}
