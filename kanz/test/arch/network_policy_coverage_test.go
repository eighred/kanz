package arch

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	sigyaml "sigs.k8s.io/yaml"
)

// NETWORK POLICY IS AUTHORIZATION HERE, NOT HYGIENE (#232).
//
// AGENTS.md states the trust model in one sentence: the api-gateway is the sole
// identity authority, it injects X-Kanz-Principal-*, and upstreams trust those
// headers "only because a NetworkPolicy makes the gateway their only reachable
// caller." Every gap in the policy set is therefore a gap in authorization, and
// three of them shipped together:
//
//  1. kanz-services default-denies egress and had NO ipBlock rule at all, so
//     every workload that dials an exchange, an OIDC issuer or a model API was
//     cut off — silently, because the connectors back off and readiness is bus
//     health.
//  2. only kanz-services and kanz-operator had any NetworkPolicy, and neither
//     kanz-operator nor any other namespace this repo creates had a
//     namespace-wide default-deny.
//  3. allow-observability-scrape selected every pod with no ports:, which
//     (NetworkPolicy being additive) silently widened every narrower rule in
//     the same file — including the one whose comment claims the venue adapters
//     accept ":9000 ONLY, and only from the OMS… this process holds the exchange
//     API credentials."
//
// None of it was caught on the rig, and the reason is written in the manifest
// itself: "kindnetd does not enforce NetworkPolicy, so NOTHING in this file has
// ever been enforced." These guards cannot run a cluster either. They pin the
// properties whose absence is an outage or an authorization bypass, so the next
// omission fails a test instead of waiting for the first enforcing CNI.

const netPolPath = "infra/security/runtime/network-policies.yaml"

// wantIPBlockExcept is the RFC1918 + link-local exclusion set every 0.0.0.0/0
// rule in this repo must carry. Without it a "may call the public internet"
// permission points INWARD: in-cluster ClusterIPs and the cloud metadata
// endpoint (169.254.169.254) are both addresses inside 0.0.0.0/0.
// infra/deploy/operator-deploy.yaml states the rule; this is the second place
// that has needed it, so the set lives here once for both.
var wantIPBlockExcept = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"169.254.0.0/16",
}

// ---------------------------------------------------------------------------
// manifest loading
// ---------------------------------------------------------------------------

// podWorkloadDoc captures the parts of a pod-bearing manifest these guards need:
// which namespace it lands in, which `app` label its pods carry, its scrape
// annotations, and the environment its containers are configured from.
type podWorkloadDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		Template struct {
			Metadata struct {
				Labels      map[string]string `yaml:"labels"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"metadata"`
			Spec struct {
				Containers     []podWorkloadContainer `yaml:"containers"`
				InitContainers []podWorkloadContainer `yaml:"initContainers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

type podWorkloadContainer struct {
	Name string `yaml:"name"`
	Env  []struct {
		Name  string `yaml:"name"`
		Value string `yaml:"value"`
	} `yaml:"env"`
}

func (w podWorkloadDoc) app() string { return w.Spec.Template.Metadata.Labels["app"] }

// decodeYAMLStream decodes a multi-document manifest into T. A parse failure is
// returned rather than fatal: these guards walk directories that also hold
// templated GitOps manifests, and a walker that dies on the first one would be
// deleted rather than fixed.
func decodeYAMLStream[T any](body []byte) ([]T, error) {
	var out []T
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	for {
		var d T
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, d)
	}
}

// infraYAMLFiles lists every *.yaml under infra/, sorted.
func infraYAMLFiles(t *testing.T) []string {
	t.Helper()
	root := filepath.Join(moduleRoot(t), "infra")
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".yaml") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk infra/: %v", err)
	}
	if len(files) < 40 {
		t.Fatalf("found only %d yaml files under infra/ — the walk is broken and every guard "+
			"built on it would pass by finding nothing", len(files))
	}
	sort.Strings(files)
	return files
}

// allNetworkPolicies decodes every NetworkPolicy in infra/, from every file.
// The two guards that came before this one each read a single hard-coded file,
// which is how a namespace with no policy at all stayed invisible: a check that
// only ever opens network-policies.yaml cannot notice kanz-messaging.
func allNetworkPolicies(t *testing.T) []networkPolicyDoc {
	t.Helper()
	var out []networkPolicyDoc
	for _, f := range infraYAMLFiles(t) {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		docs, derr := decodeYAMLStream[networkPolicyDoc](body)
		if derr != nil {
			// A file that does not parse cannot be searched for policies. Say
			// which one, and keep going — but only for files that hold no
			// NetworkPolicy text at all, since a policy hiding in an unparseable
			// file is exactly the blind spot this guard exists to remove.
			if strings.Contains(string(body), "kind: NetworkPolicy") {
				t.Errorf("%s contains a NetworkPolicy but does not parse as YAML (%v) — it cannot be checked", f, derr)
			}
			continue
		}
		for _, d := range docs {
			if d.Kind == "NetworkPolicy" {
				out = append(out, d)
			}
		}
	}
	if len(out) < 20 {
		t.Fatalf("decoded only %d NetworkPolicy docs from infra/ — expected at least 20; the "+
			"decode is broken and every guard built on it is vacuous", len(out))
	}
	return out
}

// allWorkloads decodes every pod-bearing manifest in infra/ that carries an
// `app` label on its pod template.
func allWorkloads(t *testing.T) []podWorkloadDoc {
	t.Helper()
	var out []podWorkloadDoc
	for _, f := range infraYAMLFiles(t) {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		docs, derr := decodeYAMLStream[podWorkloadDoc](body)
		if derr != nil {
			continue
		}
		for _, d := range docs {
			if d.app() != "" {
				out = append(out, d)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// selector helpers
// ---------------------------------------------------------------------------

// selectsApp reports whether a policy's podSelector selects pods labelled
// app=<name>. Both spellings this repo uses are accepted: matchLabels for a
// single workload, and a matchExpressions `app In [...]` set for a family.
func selectsApp(d networkPolicyDoc, app string) bool {
	if d.Spec.PodSelector.MatchLabels["app"] == app {
		return true
	}
	for _, e := range d.Spec.PodSelector.MatchExpressions {
		if e.Key != "app" || e.Operator != "In" {
			continue
		}
		for _, v := range e.Values {
			if v == app {
				return true
			}
		}
	}
	return false
}

// selectorIsWildcard reports whether a podSelector is `{}` — every pod in the
// policy's namespace.
func selectorIsWildcard(d networkPolicyDoc) bool {
	return len(d.Spec.PodSelector.MatchLabels) == 0 && len(d.Spec.PodSelector.MatchExpressions) == 0
}

func hasPolicyType(d networkPolicyDoc, want string) bool {
	for _, pt := range d.Spec.PolicyTypes {
		if strings.EqualFold(pt, want) {
			return true
		}
	}
	return false
}

// isDefaultDeny reports whether a policy is the namespace-wide deny-everything
// baseline: every pod, both directions, no rules.
func isDefaultDeny(d networkPolicyDoc) bool {
	return selectorIsWildcard(d) &&
		hasPolicyType(d, "Ingress") && hasPolicyType(d, "Egress") &&
		len(d.Spec.Ingress) == 0 && len(d.Spec.Egress) == 0
}

// publicInternetPeer reports whether a peer is an ipBlock covering the public
// internet, and whether its except: list is the complete one. A 0.0.0.0/0 peer
// missing any exclusion is reported as public==true, complete==false so the
// caller fails loudly rather than counting it as coverage.
func publicInternetPeer(p netPolPeer) (public, complete bool, missing []string) {
	if p.IPBlock == nil || p.IPBlock.CIDR != "0.0.0.0/0" {
		return false, false, nil
	}
	got := map[string]bool{}
	for _, e := range p.IPBlock.Except {
		got[e] = true
	}
	for _, want := range wantIPBlockExcept {
		if !got[want] {
			missing = append(missing, want)
		}
	}
	return true, len(missing) == 0, missing
}

// ---------------------------------------------------------------------------
// ARM 1 — every internet-facing workload has an egress rule that reaches it
// ---------------------------------------------------------------------------

// externalEgressExempt names apps with an external destination that deliberately
// get NO public-internet egress rule, each with the reason and the issue that
// retires it.
//
// IT IS EMPTY, AND THAT IS THE CURRENT TRUTH rather than an oversight: all five
// apps this guard finds in scope (api-gateway, copilot, market-ingest,
// venue-binance, venue-okx) genuinely dial a public host in the deployed
// configuration, and all five are covered by allow-external-https-egress /
// allow-exchange-stream-egress. The map and its dead-entry arm below exist so the
// next exemption has to be written down with a reason instead of being expressed
// as a service quietly missing from the policy.
var externalEgressExempt = map[string]string{
	"identity": "IDENTITY_TOKEN_ISSUER is this service's OWN identity, not a destination it dials " +
		"(#364). OIDC requires the issuer claim to equal the URL a verifier discovered the document " +
		"from, so it is published in /.well-known/openid-configuration and stamped into every token " +
		"— and read by the API GATEWAY, which fetches the JWKS. The identity service never makes an " +
		"outbound request to it. The scanner cannot tell an issuer from an endpoint, because at the " +
		"level of an env var holding a URL they are the same thing; grep the service for an HTTP " +
		"client and there is none. Egress for this app is Postgres and SPIRE only, both covered by " +
		"the namespace rules.",
}

// externalDest is one required public-internet destination for one app.
type externalDest struct {
	app    string
	port   int
	source string // file:line-ish provenance, quoted back in failures
	raw    string
}

// httpsLiteral matches a quoted https:// or wss:// URL. Applied only to Go
// string literals recovered from the AST, never to raw file text — several
// config files discuss URLs they no longer default to, in comments.
func urlPort(raw string) (int, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return 0, err
	}
	if p := u.Port(); p != "" {
		return strconv.Atoi(p)
	}
	switch u.Scheme {
	case "https", "wss":
		return 443, nil
	}
	return 0, fmt.Errorf("scheme %q is not TLS", u.Scheme)
}

// externalDestinations derives, from TWO independent sources, every public host
// a kanz-services workload dials.
//
// BOTH SOURCES ARE REQUIRED, and each one alone misses a live service:
//
//   - the deploy manifests catch venue-okx, whose OKX_BASE_URL / OKX_WS_BASE have
//     NO code default at all — #147 removed them so that "nobody configured this"
//     could not look like a working config. A code-only scan sees nothing.
//   - the config defaults catch copilot, whose OpenRouter endpoint is a const in
//     internal/config and appears in no manifest. A manifest-only scan sees
//     nothing.
//
// A guard keyed on either source alone would have reported full coverage while
// an order path or a model call stayed severed.
func externalDestinations(t *testing.T) []externalDest {
	t.Helper()
	var out []externalDest

	// Source A: env values in the deploy manifests, which is the deployed truth.
	deployedApps := map[string]bool{}
	for _, w := range allWorkloads(t) {
		if w.Metadata.Namespace != "kanz-services" {
			continue
		}
		deployedApps[w.app()] = true
		containers := append(append([]podWorkloadContainer{}, w.Spec.Template.Spec.Containers...),
			w.Spec.Template.Spec.InitContainers...)
		for _, c := range containers {
			for _, e := range c.Env {
				if !strings.HasPrefix(e.Value, "https://") && !strings.HasPrefix(e.Value, "wss://") {
					continue
				}
				port, err := urlPort(e.Value)
				if err != nil {
					t.Errorf("%s env %s = %q: cannot derive a port (%v)", w.Metadata.Name, e.Name, e.Value, err)
					continue
				}
				out = append(out, externalDest{
					app: w.app(), port: port, raw: e.Value,
					source: fmt.Sprintf("infra/deploy manifest for %s, env %s", w.Metadata.Name, e.Name),
				})
			}
		}
	}

	// Source B: https:// / wss:// defaults in each service's internal/config.
	// Recovered from the AST so a URL mentioned in a comment — venue-okx's config
	// discusses the defaults #147 deleted — cannot be mistaken for a live one.
	servicesRoot := filepath.Join(moduleRoot(t), "services")
	entries, err := os.ReadDir(servicesRoot)
	if err != nil {
		t.Fatalf("read services/: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		svc := e.Name()
		cfgDir := filepath.Join(servicesRoot, svc, "internal", "config")
		files, gerr := filepath.Glob(filepath.Join(cfgDir, "*.go"))
		if gerr != nil || len(files) == 0 {
			continue
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			fset := token.NewFileSet()
			file, perr := parser.ParseFile(fset, f, nil, 0)
			if perr != nil {
				t.Fatalf("parse %s: %v", f, perr)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				raw, uerr := strconv.Unquote(lit.Value)
				if uerr != nil {
					return true
				}
				if !strings.HasPrefix(raw, "https://") && !strings.HasPrefix(raw, "wss://") {
					return true
				}
				port, perr := urlPort(raw)
				if perr != nil {
					return true
				}
				// A service with no deployed workload has nothing to police.
				if !deployedApps[svc] {
					return true
				}
				out = append(out, externalDest{
					app: svc, port: port, raw: raw,
					source: fmt.Sprintf("%s:%d", filepath.ToSlash(strings.TrimPrefix(f, moduleRoot(t)+string(os.PathSeparator))), fset.Position(lit.Pos()).Line),
				})
				return true
			})
		}
	}
	return out
}

// TestInternetFacingWorkloadsHaveEgress asserts that every kanz-services workload
// with a public https:// / wss:// destination has an egress rule reaching it.
//
// The namespace default-denies egress and had no ipBlock rule of any kind, so on
// the first CNI that enforces policy the market feed, both venue adapters, the
// copilot and — worst — the gateway's OIDC issuer are all unreachable. The venue
// connectors turn that into an unbounded "connect failed; backing off" loop and
// readiness is bus health, so the pods report Ready throughout. A platform that
// says it is healthy while no order can leave it is the failure this pins.
func TestInternetFacingWorkloadsHaveEgress(t *testing.T) {
	dests := externalDestinations(t)
	policies := allNetworkPolicies(t)

	apps := map[string]bool{}
	for _, d := range dests {
		apps[d.app] = true
	}
	// NON-VACUITY. The estate demonstrably has several internet-facing workloads.
	// A derivation that finds fewer has stopped finding them, and would pass no
	// matter what the policy set says.
	if len(apps) < 4 {
		t.Fatalf("derived only %d internet-facing app(s) %v from the deploy manifests and "+
			"internal/config defaults — expected at least 4 (api-gateway, copilot, market-ingest, "+
			"venue-binance, venue-okx). The derivation is broken, so this guard asserts nothing",
			len(apps), sortedKeys(apps))
	}
	if len(dests) < 6 {
		t.Fatalf("derived only %d external destination(s) — expected at least 6", len(dests))
	}

	// granted[app][port] — the app may reach the public internet on that port.
	granted := map[string]map[int]bool{}
	for _, p := range policies {
		if p.Metadata.Namespace != "kanz-services" || !hasPolicyType(p, "Egress") {
			continue
		}
		for _, rule := range p.Spec.Egress {
			for _, peer := range rule.To {
				public, complete, missing := publicInternetPeer(peer)
				if !public {
					continue
				}
				if !complete {
					t.Errorf("%s has a 0.0.0.0/0 egress peer whose except: list is missing %v. "+
						"An unexcluded 0.0.0.0/0 does not mean \"the public internet\" — it includes "+
						"every in-cluster ClusterIP and the cloud metadata endpoint 169.254.169.254, "+
						"so it turns an exchange permission INWARD on the pods that hold the exchange "+
						"credentials. operator-deploy.yaml states the same rule for the operator.",
						p.Metadata.Name, missing)
					continue
				}
				for _, port := range rule.Ports {
					for app := range apps {
						if selectsApp(p, app) {
							if granted[app] == nil {
								granted[app] = map[int]bool{}
							}
							granted[app][port.Port] = true
						}
					}
				}
			}
		}
	}

	var missing []string
	for _, d := range dests {
		if _, ok := externalEgressExempt[d.app]; ok {
			continue
		}
		if granted[d.app][d.port] {
			continue
		}
		missing = append(missing, fmt.Sprintf("%s → TCP:%d (%s, from %s)", d.app, d.port, d.raw, d.source))
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("these workloads dial a public host that no NetworkPolicy lets them reach:\n  %s\n\n"+
			"kanz-services default-denies egress (default-deny-all) and the only allows are DNS, "+
			"kanz-messaging, postgres, OTLP and named pod-to-pod pairs. Under an enforcing CNI each "+
			"line above is a severed connection that presents as a backoff loop on a pod that still "+
			"reports Ready.\n\n"+
			"Add the app to allow-external-https-egress (443) or allow-exchange-stream-egress "+
			"(8443/9443) in %s. The ipBlock MUST carry the full except: list %v.",
			strings.Join(missing, "\n  "), netPolPath, wantIPBlockExcept)
	}

	// DEAD ENTRIES: an exemption for an app that no longer has an external
	// destination, or that now has a rule, is stale permission.
	var dead []string
	for app, reason := range externalEgressExempt {
		if reason == "" {
			dead = append(dead, app+" (no reason recorded)")
		}
		if !apps[app] {
			dead = append(dead, app+" (no external destination found — exemption is dead)")
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("externalEgressExempt has %d stale entr(y/ies):\n  %s", len(dead), strings.Join(dead, "\n  "))
	}
}

// ---------------------------------------------------------------------------
// ARM 2 — every namespace this repo creates default-denies
// ---------------------------------------------------------------------------

// defaultDenyExempt names namespaces this repo creates that have no
// namespace-wide default-deny, each with the reason and what retires it.
//
// EVERY ENTRY IS A REAL, OPEN GAP, not a design decision. A namespace with no
// default-deny accepts ingress from ANY pod in the cluster, which for
// kanz-messaging means the NATS/Kafka/Redis spine is reachable without going
// through the gateway, the RLS-scoped DSN, or any part of the header-trust chain
// AGENTS.md's model rests on.
//
// They are exemptions rather than policies because writing a default-deny for a
// clustered StatefulSet blind is how this file's existing gaps were created. The
// manifest's own comment records it: "kindnetd does not enforce NetworkPolicy, so
// NOTHING in this file has ever been enforced on the rig… A policy set that has
// never met an enforcing CNI is a hypothesis." A guessed allow-set for NATS
// cluster routes, the Kafka KRaft controller quorum and kubelet probes is a new
// hypothesis with an estate-down failure mode, and it cannot be verified here.
// The gap is recorded in code, with the issue, so it is retired by evidence.
var defaultDenyExempt = map[string]string{
	"kanz-messaging": "NATS (cluster routes :6222), Kafka (KRaft controller quorum :9093), Redis and " +
		"five Jobs (bootstrap, topics, tenancy, DR rebuild, mirrormaker2) share this namespace. A " +
		"default-deny needs an allow-set covering intra-cluster gossip AND the node-sourced kubelet " +
		"probes that no pod/namespace selector matches — get either wrong and the spine goes " +
		"unready, which is the whole estate. Needs a cluster with an enforcing CNI to verify. #232",
	"kanz-observability": "Prometheus discovers pods cluster-wide (role: pod) and must reach every " +
		"annotated metrics port in every namespace; Grafana reaches Prometheus; node-exporter is a " +
		"host-network DaemonSet. An egress default-deny here silently blanks the dashboards and the " +
		"SLO alerting built on them, and a wrong allow-set looks identical to services being down. #232",
	"kanz-lakehouse": "Trino coordinator/worker split with its own intra-cluster protocol, plus the " +
		"object-store egress the Iceberg/Hive catalogs need. Not on the capital path and not part of " +
		"the header-trust chain, so it is last in this queue. #232",
	"kanz-tenancy": "Holds one Job (the tenant onboarding runner). It is a client, not a server, and " +
		"the namespace exists only while a tenant is being provisioned — but it does reach Postgres " +
		"and NATS with provisioning credentials, so it earns a policy rather than a permanent pass. #232",
	"vault": "Third-party chart namespace. This repo declares the Namespace so `kubectl apply -f` is " +
		"self-sufficient, but does not own what runs in it; a default-deny authored here would fight " +
		"the chart's own policies. #232",
	"spire-system": "Third-party (SPIRE server/agent). The agent is host-network and must reach the " +
		"kubelet and the node's workload attestor — the one workload in the estate for which a " +
		"namespace default-deny is actively wrong. Owned by the SPIRE chart, not by this repo. #232",
}

// TestEveryCreatedNamespaceDefaultDenies asserts every namespace this repo's
// manifests CREATE carries a namespace-wide default-deny, or an exemption saying
// why not.
//
// A NetworkPolicy governs only its own namespace's pods. kanz-services has had a
// default-deny since SEC-02a and every other namespace had none, so the careful
// per-flow rules in network-policies.yaml bounded exactly one side of the estate
// while the spine, the monitoring plane and the operator's namespace accepted
// connections from anywhere in the cluster.
//
// KEYED ON `kind: Namespace`, deliberately. kanz-data is referenced fifteen times
// (the CNPG clusters, DR failover, tenant provisioning) and declared NOWHERE in
// this repo — so it is out of this guard's scope by construction, and it is still
// an unprotected namespace holding every tenant's database. Adding a Namespace
// doc for it would pull it in here; that is the right fix, and it belongs with
// whoever owns the CNPG install.
func TestEveryCreatedNamespaceDefaultDenies(t *testing.T) {
	// Namespaces created by a `kind: Namespace` document anywhere in infra/.
	created := map[string]string{} // name -> file that creates it
	for _, f := range infraYAMLFiles(t) {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		docs, derr := decodeYAMLStream[networkPolicyDoc](body)
		if derr != nil {
			continue
		}
		for _, d := range docs {
			if d.Kind == "Namespace" && d.Metadata.Name != "" {
				if _, seen := created[d.Metadata.Name]; !seen {
					created[d.Metadata.Name] = filepath.ToSlash(strings.TrimPrefix(f, moduleRoot(t)+string(os.PathSeparator)))
				}
			}
		}
	}

	// NON-VACUITY. This repo demonstrably creates several namespaces.
	if len(created) < 6 {
		t.Fatalf("found only %d `kind: Namespace` doc(s) in infra/ %v — expected at least 6. The "+
			"scan is broken and this guard would pass by finding nothing to check",
			len(created), sortedKeys(created))
	}

	denied := map[string]bool{}
	for _, p := range allNetworkPolicies(t) {
		if isDefaultDeny(p) {
			denied[p.Metadata.Namespace] = true
		}
	}
	// NON-VACUITY, second half: at least the two namespaces this repo fully owns
	// must actually be enforced, or the exemption list has swallowed everything
	// and the guard has become a list of excuses.
	for _, ns := range []string{"kanz-services", "kanz-operator"} {
		if !denied[ns] {
			t.Errorf("%s has no namespace-wide default-deny (podSelector: {}, policyTypes "+
				"[Ingress, Egress], no rules). This is a namespace the platform fully owns; every "+
				"per-flow rule in it is meaningless without the baseline that makes the rest deny.", ns)
		}
	}

	var missing []string
	for ns, src := range created {
		if denied[ns] {
			continue
		}
		if _, ok := defaultDenyExempt[ns]; ok {
			continue
		}
		missing = append(missing, fmt.Sprintf("%s (created by %s)", ns, src))
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("these namespaces are created by this repo and have no default-deny NetworkPolicy:\n  %s\n\n"+
			"A NetworkPolicy governs only its own namespace, so pods here accept ingress from ANY pod "+
			"in the cluster — including the upstreams that trust X-Kanz-Principal-* headers because "+
			"\"a NetworkPolicy makes the gateway their only reachable caller\" (AGENTS.md). Add a "+
			"default-deny-all beside the namespace, or add an entry to defaultDenyExempt with the "+
			"reason and the issue that retires it.", strings.Join(missing, "\n  "))
	}

	// DEAD ENTRIES: an exemption for a namespace that is no longer created here,
	// or that now HAS a default-deny, is stale permission.
	var dead []string
	for ns, reason := range defaultDenyExempt {
		switch {
		case reason == "":
			dead = append(dead, ns+" (no reason recorded)")
		case !strings.Contains(reason, "#"):
			dead = append(dead, ns+" (reason names no issue — an exemption must carry what retires it)")
		}
		if _, ok := created[ns]; !ok {
			dead = append(dead, ns+" (no longer created by any manifest in infra/)")
			continue
		}
		if denied[ns] {
			dead = append(dead, ns+" (now has a default-deny — the exemption outlived the repair)")
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("defaultDenyExempt has %d stale entr(y/ies):\n  %s", len(dead), strings.Join(dead, "\n  "))
	}
}

// ---------------------------------------------------------------------------
// ARM 3 — no wildcard ingress rule without a port list
// ---------------------------------------------------------------------------

// wildcardIngressFinding is one policy that opens every port of every pod in its
// namespace to some peer.
type wildcardIngressFinding struct {
	Namespace string
	Name      string
	Peers     []string
}

// findWildcardIngressWithoutPorts returns every policy whose podSelector is `{}`
// and which carries an ingress rule with no ports:.
//
// Factored out so it can be run against a synthetic manifest — the mutation
// proof lives in TestWildcardIngressCheckIsNonVacuous, which feeds it the exact
// shape allow-observability-scrape had before #232 and asserts it is flagged.
func findWildcardIngressWithoutPorts(docs []networkPolicyDoc, namespace string) []wildcardIngressFinding {
	var out []wildcardIngressFinding
	for _, d := range docs {
		if d.Kind != "NetworkPolicy" || d.Metadata.Namespace != namespace {
			continue
		}
		if !selectorIsWildcard(d) {
			continue
		}
		for _, r := range d.Spec.Ingress {
			if len(r.Ports) > 0 {
				continue
			}
			peers := make([]string, 0, len(r.From))
			for _, p := range r.From {
				peers = append(peers, p.describe())
			}
			if len(peers) == 0 {
				peers = []string{"<empty from: — every pod in the cluster>"}
			}
			out = append(out, wildcardIngressFinding{Namespace: d.Metadata.Namespace, Name: d.Metadata.Name, Peers: peers})
		}
	}
	return out
}

// TestNoWildcardIngressWithoutPorts forbids the shape that made
// allow-observability-scrape a cluster-wide port opener.
//
// NETWORKPOLICY IS ADDITIVE. `podSelector: {}` plus an ingress rule with no
// `ports:` does not say "this peer may scrape /metrics" — it says "this peer may
// open ANY port on ANY pod in this namespace", and it overrides every narrower
// rule in the same file. The one that shipped meant a pod in kanz-observability
// could reach postgres:5432, the venue adapters' order-submission gRPC on :9000
// (the port whose own rule's comment says ":9000 ONLY, and only from the OMS…
// this process holds the exchange API credentials"), risk-engine's query surface
// on :9090 and inference on :50051.
//
// The isolation a rule like this claims to provide is WHO may connect. The
// isolation it actually provides is none, because "who" is a namespace and a
// namespace is a set of pods that can each hold a different intent.
func TestNoWildcardIngressWithoutPorts(t *testing.T) {
	docs := allNetworkPolicies(t)

	// NON-VACUITY: there must BE wildcard-podSelector policies in kanz-services
	// for this check to be about anything. There are several (the default-deny,
	// the DNS/messaging/postgres/OTLP egress rules, the scrape rule).
	wildcards := 0
	for _, d := range docs {
		if d.Metadata.Namespace == "kanz-services" && selectorIsWildcard(d) {
			wildcards++
		}
	}
	if wildcards < 4 {
		t.Fatalf("only %d policy(ies) in kanz-services use podSelector: {} — expected at least 4. "+
			"The selector check has stopped matching and this guard is asserting nothing", wildcards)
	}

	findings := findWildcardIngressWithoutPorts(docs, "kanz-services")
	for _, f := range findings {
		t.Errorf("NetworkPolicy %s/%s selects EVERY pod (podSelector: {}) and has an ingress rule "+
			"with no ports:, from %v. NetworkPolicy is additive, so this does not grant a metrics "+
			"scrape — it grants that peer every TCP port on every pod in the namespace, and it "+
			"silently widens every narrower ingress rule in the file (postgres:5432, the venue "+
			"adapters' :9000 order path and the exchange credentials behind it, risk-engine:9090, "+
			"inference:50051). Name the ports.", f.Namespace, f.Name, f.Peers)
	}
}

// TestWildcardIngressCheckIsNonVacuous proves the check above can fail, by
// running it against the exact manifest shape allow-observability-scrape had
// before #232. A guard that has only ever been observed passing is not evidence
// that the property holds — it is evidence that nothing was measured.
func TestWildcardIngressCheckIsNonVacuous(t *testing.T) {
	const preFix = `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-observability-scrape
  namespace: kanz-services
spec:
  podSelector: {}
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: kanz-observability
`
	docs, err := decodeYAMLStream[networkPolicyDoc]([]byte(preFix))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	findings := findWildcardIngressWithoutPorts(docs, "kanz-services")
	if len(findings) != 1 {
		t.Fatalf("findWildcardIngressWithoutPorts flagged %d finding(s) on the known-bad pre-#232 "+
			"shape, want exactly 1 — the check cannot detect the very defect it was written for, so "+
			"TestNoWildcardIngressWithoutPorts passing means nothing", len(findings))
	}

	// And it must NOT flag the fixed shape, or it would be an unconditional
	// failure that says nothing either.
	fixed := strings.TrimSuffix(preFix, "\n") + "\n      ports:\n        - { protocol: TCP, port: 8080 }\n"
	docs, err = decodeYAMLStream[networkPolicyDoc]([]byte(fixed))
	if err != nil {
		t.Fatalf("parse fixed fixture: %v", err)
	}
	if got := findWildcardIngressWithoutPorts(docs, "kanz-services"); len(got) != 0 {
		t.Fatalf("findWildcardIngressWithoutPorts flagged %d finding(s) on a policy WITH a ports "+
			"list, want 0 — the check fails unconditionally and proves nothing", len(got))
	}
}

// ---------------------------------------------------------------------------
// ARM 3b — the scrape rule's ports are exactly the annotated metrics ports
// ---------------------------------------------------------------------------

// TestObservabilityScrapePortsMatchTheAnnotations pins allow-observability-scrape's
// port list to the prometheus.io/port annotations in infra/deploy/.
//
// THIS IS THE ANSWER TO THE OBJECTION THE OLD COMMENT RAISED. It argued against a
// port list because "the next service with a new metrics port is silently
// unmonitored" — a real risk, and the reason the rule was left wide open. The fix
// for a list that can go stale is a guard that fails when it does, not a
// permission wide enough that staleness cannot show. Both directions are checked:
// an annotated port missing from the policy is an unscrapable service, and a port
// in the policy that no workload annotates is a permission with no user.
func TestObservabilityScrapePortsMatchTheAnnotations(t *testing.T) {
	wantPorts := map[int][]string{} // port -> apps annotating it
	for _, w := range allWorkloads(t) {
		if w.Metadata.Namespace != "kanz-services" {
			continue
		}
		raw, ok := w.Spec.Template.Metadata.Annotations["prometheus.io/port"]
		if !ok {
			continue
		}
		p, err := strconv.Atoi(raw)
		if err != nil {
			t.Errorf("%s has prometheus.io/port = %q, which is not a number", w.Metadata.Name, raw)
			continue
		}
		wantPorts[p] = append(wantPorts[p], w.app())
	}
	// NON-VACUITY: the estate annotates well over a dozen workloads across at
	// least half a dozen distinct ports.
	if len(wantPorts) < 6 {
		t.Fatalf("found only %d distinct prometheus.io/port value(s) in kanz-services — expected at "+
			"least 6. The annotation scan is broken and this guard compares two empty sets", len(wantPorts))
	}

	var scrape *networkPolicyDoc
	for i, d := range allNetworkPolicies(t) {
		if d.Metadata.Name == "allow-observability-scrape" && d.Metadata.Namespace == "kanz-services" {
			docs := allNetworkPolicies(t)
			scrape = &docs[i]
			break
		}
	}
	if scrape == nil {
		t.Fatal("no NetworkPolicy named allow-observability-scrape in kanz-services. Prometheus " +
			"discovers pods and scrapes /metrics; without this rule the SLO recording rules and " +
			"every alert built on them go blind under an enforcing CNI (OBS-01).")
	}

	gotPorts := map[int]bool{}
	for _, r := range scrape.Spec.Ingress {
		for _, p := range r.Ports {
			gotPorts[p.Port] = true
		}
	}

	var unscrapable []string
	for port, apps := range wantPorts {
		if !gotPorts[port] {
			sort.Strings(apps)
			unscrapable = append(unscrapable, fmt.Sprintf("TCP:%d (%s)", port, strings.Join(apps, ", ")))
		}
	}
	sort.Strings(unscrapable)
	if len(unscrapable) > 0 {
		t.Errorf("allow-observability-scrape does not permit these annotated metrics ports:\n  %s\n\n"+
			"kanz-services is default-deny for ingress, so an unlisted port is a service Prometheus "+
			"cannot reach — and an unreachable target is a DOWN row on a dashboard nobody watches, "+
			"not an error. Add the port to the rule in %s.", strings.Join(unscrapable, "\n  "), netPolPath)
	}

	var unused []int
	for port := range gotPorts {
		if _, ok := wantPorts[port]; !ok {
			unused = append(unused, port)
		}
	}
	sort.Ints(unused)
	if len(unused) > 0 {
		t.Errorf("allow-observability-scrape permits ports %v that no kanz-services workload "+
			"annotates as prometheus.io/port. A permission with no user is not free here: the peer "+
			"is a whole namespace and the rule selects every pod, so each extra port is a path from "+
			"the monitoring plane to whatever else happens to listen on it.", unused)
	}
}

// ---------------------------------------------------------------------------
// ARM 3c — the residual the port list cannot close
// ---------------------------------------------------------------------------

// tenantHeaderTrustingServices are the services that READ X-Kanz-Principal-Tenant
// off an inbound request and serve the named tenant's data, mapped to the port
// they serve it on.
//
// datamaster, tv-sync and wealth still deploy their tenant API on the metrics port.
// Their listener separation and live verification remain under #232. Services
// that split move to the map below; neither an annotation nor a policy alone
// proves that the running process separates its routes.
var tenantHeaderTrustingServices = map[string]int{
	"datamaster": 8080,
	"tv-sync":    8091,
	"wealth":     8080,
}

// tenantHeaderTrustingSplitListeners is the SAME trust, taken on by a service
// that serves its API and its /metrics on DIFFERENT ports (#409).
//
// It is a separate set because the assertion is the opposite of the one above.
// For the services in tenantHeaderTrustingServices the guard checks that
// the recorded port IS the metrics port, because their tenant-scoped routes
// share it — which is precisely the exposure #232 named and could not close:
// allow-observability-scrape must admit that port, so a pod in
// kanz-observability can choose a principal and use those routes.
//
// #232 said the fix was a second listener in each service, a code change rather
// than a manifest edit. These are the services that made it. The value here is
// the API port, and the guard asserts it is NOT the scraped port and NOT in
// allow-observability-scrape's list — so the separation cannot quietly collapse
// back into one port while the entry keeps claiming otherwise.
var tenantHeaderTrustingSplitListeners = map[string]int{
	// The API materializes a rebalance proposal into order commands and takes the
	// issuer — the field the audit trail records as the person who moved the
	// capital — from the injected principal. A trading surface is the one place
	// this platform cannot afford to inherit #232's gap, so it does not.
	"optimization": 8100,
	// mcp (#743): the agent-facing MCP read plane, SPLIT FROM ITS FIRST COMMIT
	// rather than retrofitted. Its JSON-RPC route decides which TENANT's risk
	// state an external agent may read, from the injected principal — so it is
	// on :8110, which allow-observability-scrape does not admit, while /metrics
	// sits on the :8080 that rule already permits. The two are separate because
	// the alternative is handing the monitoring namespace a self-chosen tenant
	// and a read of that tenant's book.
	"mcp": 8110,
	// accounting SPLIT (#447), and it is the first DEPLOYED service to do so.
	//
	// Its /v1 routes WRITE: a subscription or redemption posted to the IBOR, a
	// NAV, a break list. It sat in tenantHeaderTrustingServices on :8080 — the
	// scraped port — so a pod in kanz-observability could send a self-chosen
	// tenant header to a cash-movement write. The API moved to :8101, which
	// allow-observability-scrape does not admit, and :8080 now serves /metrics
	// and nothing else.
	//
	// THE PORTS SWAPPED RATHER THAN THE METRICS MOVING, because 8080 is shared by
	// six services that legitimately scrape there — admitting a seventh port for
	// accounting's metrics would have widened the rule for no gain. It is the API
	// that had to leave.
	//
	// Moving this entry between the two maps FLIPS the assertion: the map above
	// checks the recorded port IS scraped, this one checks it is NOT. Leaving it
	// above would have kept passing — 8080 is still scraped — while claiming
	// accounting's tenant-scoped routes share it, which stopped being true.
	"accounting": 8101,
	// compliance SPLIT FOR ACT TWO OF #410 (#562), and its argument is the
	// strongest of the three.
	//
	// Its /v1 routes change the MANDATE — the constraint the OMS pre-trade gate
	// and this service's own post-trade monitor check every order against — and
	// they take the proposer AND the approver from X-Kanz-Principal-Subject. On
	// the scraped port a pod in kanz-observability could send two self-chosen
	// principals and hold BOTH signatures, which is not a disclosure gap like the
	// four entries above: it is the dual-control failure #562 exists to prevent,
	// arriving through the network layer instead of through the CLI.
	//
	// :8091 keeps /metrics and the probes, which allow-observability-scrape
	// admits; the API is on :8095, which it does not. The assertions below check
	// both halves of that claim on every run.
	"compliance": 8095,
	// audit SPLIT (#627), and its case is different from the three above: they
	// each traded a shared port for a surface the gateway genuinely proxied, so
	// the exposure was the PRICE of something. audit's was the price of nothing.
	//
	// Nothing in this repository called its /v1 routes, no NetworkPolicy named
	// app: audit, and the gateway did not proxy it — so the ONLY peer that could
	// reach the tenant's compliance record was kanz-observability, through
	// allow-observability-scrape's :8083. The surface was simultaneously dark to
	// every legitimate reader and open to the one namespace that authenticates
	// nothing and can choose its own principal.
	//
	// What it served that peer: /v1/audit/events and /v1/soc2/evidence take the
	// tenant from X-Kanz-Principal-Tenant against audit_log, which is deliberately
	// NOT RLS'd because it IS the cross-tenant record — so the database cannot
	// catch what the handler lets through. /v1/audit/verify adds the estate-wide
	// count #118 exists to withhold, gated by roles read from the same forgeable
	// header.
	//
	// THE PORTS DID NOT SWAP, unlike accounting's (#447): :8083 is shared with
	// regulatory in allow-observability-scrape's list, so moving audit's /metrics
	// off it would have widened the rule for no gain. The API left instead.
	"audit": 8102,
}

// tenantHeaderRead matches a READ of the tenant principal header — the act that
// makes a service depend on the gateway being its only caller. It deliberately
// does not match the WRITE side (auth.SetPrincipalHeaders, formerly Header.Set):
// injecting or forwarding the header is what the gateway and copilot do, and
// neither takes on the network obligation.
//
// PrincipalFromHeaders IS A THIRD WAY IN, and it was invisible here until #409.
// It reads the subject AND the tenant and returns the whole Principal, so a
// service calling it takes on exactly the same network obligation as one calling
// RequireCallerTenant — but it names no tenant constant, so this guard did not
// see it. The optimization service joining the trust boundary is what surfaced
// that: a green guard was checking a weaker property than its name claims.
//
// THE FIRST ALTERNATIVE IS THE ONE THAT MATTERS TODAY. #258 moved the constant
// and both enforcement policies into pkg/auth, so no service performs the
// Header.Get itself any more — and the moment it landed, the non-vacuity floor
// below fired with "only 0 service(s)", which is the guard doing exactly its job:
// the read moved and it had stopped watching. The Header.Get arm is kept because
// it is what a NEW service would write before it discovers pkg/auth, and that is
// precisely the case this guard exists to catch.
var tenantHeaderRead = regexp.MustCompile(
	`auth\.(?:RequireCallerTenantIs|RequireCallerTenant|CallerTenant|PrincipalFromHeaders)\(` +
		`|Header\.Get\(\s*(?:[A-Za-z0-9_.]*[Hh]eaderPrincipalTenant|"X-Kanz-Principal-Tenant")\s*\)`)

// TestTenantHeaderTrustingServicesAreEnumerated fails when a service reads
// X-Kanz-Principal-Tenant and is not in the map above.
//
// The header is the entire authorization story for these routes — AGENTS.md:
// upstreams trust it "only because a NetworkPolicy makes the gateway their only
// reachable caller". A new service adopting that trust is adopting a network
// obligation with it, and the shared-listener map records the remaining gaps. Nothing should join them by accident.
func TestTenantHeaderTrustingServicesAreEnumerated(t *testing.T) {
	root := filepath.Join(moduleRoot(t), "services")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read services/: %v", err)
	}

	found := map[string]string{} // service -> file:line of the read
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		svc := e.Name()
		_ = filepath.WalkDir(filepath.Join(root, svc), func(path string, d fs.DirEntry, werr error) error {
			if werr != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Fatalf("read %s: %v", path, rerr)
			}
			for i, line := range strings.Split(string(b), "\n") {
				if tenantHeaderRead.MatchString(line) {
					if _, seen := found[svc]; !seen {
						rel := filepath.ToSlash(strings.TrimPrefix(path, moduleRoot(t)+string(os.PathSeparator)))
						found[svc] = fmt.Sprintf("%s:%d", rel, i+1)
					}
				}
			}
			return nil
		})
	}

	// NON-VACUITY: the header is demonstrably read by several services. A scan
	// finding fewer means the constant or the accessor has been renamed and this
	// guard has silently stopped watching the trust boundary.
	if len(found) < 4 {
		t.Fatalf("only %d service(s) READ X-Kanz-Principal-Tenant %v — expected at least 4 "+
			"(audit, datamaster, tv-sync, wealth). The scan is broken and this guard is watching "+
			"nothing", len(found), sortedKeys(found))
	}

	var unlisted []string
	for svc, where := range found {
		_, deployed := tenantHeaderTrustingServices[svc]
		_, split := tenantHeaderTrustingSplitListeners[svc]
		if !deployed && !split {
			unlisted = append(unlisted, svc+" ("+where+")")
		}
	}
	sort.Strings(unlisted)
	if len(unlisted) > 0 {
		t.Errorf("these services read X-Kanz-Principal-Tenant and are not listed in "+
			"tenantHeaderTrustingServices:\n  %s\n\n"+
			"That header is the whole authorization decision for the routes behind it, and it is "+
			"sound ONLY while the api-gateway is the service's only reachable caller. Adding the "+
			"trust adds a network obligation: the service must serve those routes on a port no "+
			"other namespace may open. The remaining shared-listener services serve theirs on the same port as "+
			"/metrics, which allow-observability-scrape must permit — so kanz-observability can "+
			"choose a tenant and read its book (#232). Add the service here with its port, and say "+
			"which listener serves the tenant-scoped routes.", strings.Join(unlisted, "\n  "))
	}

	// THE PORT VALUES MUST BE LIVE, not documentation. Each is claimed to be the
	// port the tenant-scoped routes share with /metrics; check it against the
	// workload's own prometheus.io/port annotation. The day a service splits its
	// listeners, this stops matching and the entry must be revisited — which is
	// the only way an exemption of this kind gets retired on evidence rather than
	// on someone remembering.
	annotated := map[string]int{}
	for _, w := range allWorkloads(t) {
		if w.Metadata.Namespace != "kanz-services" {
			continue
		}
		if raw, ok := w.Spec.Template.Metadata.Annotations["prometheus.io/port"]; ok {
			if p, err := strconv.Atoi(raw); err == nil {
				annotated[w.app()] = p
			}
		}
	}
	// THE SPLIT MUST STILL BE A SPLIT. These entries claim the service's API port
	// is not the port the monitoring plane may open; if the listeners ever merge
	// back the claim becomes false silently, and the service rejoins #232's
	// exposure while this file still says it does not.
	scrapePermitted := scrapePolicyPorts(t)
	for svc, apiPort := range tenantHeaderTrustingSplitListeners {
		metricsPort, ok := annotated[svc]
		if !ok {
			t.Errorf("tenantHeaderTrustingSplitListeners lists %s, but no kanz-services workload "+
				"labelled app=%s annotates a prometheus.io/port. The claim that its listeners are "+
				"split is no longer checkable.", svc, svc)
			continue
		}
		if metricsPort == apiPort {
			t.Errorf("%s claims SPLIT listeners with its API on :%d, but its prometheus.io/port is "+
				"the SAME port. The split has collapsed: allow-observability-scrape must admit the "+
				"metrics port, so the API is now reachable from kanz-observability and a pod there "+
				"can choose its own principal (#232). Either restore the second listener or move "+
				"this entry into tenantHeaderTrustingServices and accept the exposure explicitly.",
				svc, apiPort)
		}
		if scrapePermitted[apiPort] {
			t.Errorf("%s serves its API on :%d and allow-observability-scrape PERMITS that port. "+
				"Splitting the listeners bought nothing: the whole kanz-observability namespace can "+
				"reach the API, and these routes decide what a caller may see — and for the trading "+
				"surfaces whose name goes on an order — from a header the api-gateway is supposed to "+
				"be the only source of.", svc, apiPort)
		}
		if _, alsoListed := tenantHeaderTrustingServices[svc]; alsoListed {
			t.Errorf("%s is in BOTH tenantHeaderTrustingServices and "+
				"tenantHeaderTrustingSplitListeners — one is stale, and a reader cannot tell which "+
				"claim is current", svc)
		}
	}

	for svc, port := range tenantHeaderTrustingServices {
		got, ok := annotated[svc]
		if !ok {
			t.Errorf("tenantHeaderTrustingServices lists %s on :%d, but no kanz-services workload "+
				"labelled app=%s annotates a prometheus.io/port. Either the service is no longer "+
				"deployed or the label changed; the claim that its API shares the metrics port is "+
				"no longer checkable.", svc, port, svc)
			continue
		}
		if got != port {
			t.Errorf("tenantHeaderTrustingServices says %s serves its tenant-scoped routes on :%d, "+
				"but its prometheus.io/port annotation is :%d. If the listeners have been SPLIT "+
				"this entry should be removed — that is the fix #232 could not make. If they have "+
				"not, the entry is wrong and the exposure is on a port nobody is tracking.",
				svc, port, got)
		}
	}

	// DEAD ENTRIES: a listed service that no longer reads the header.
	var dead []string
	for svc := range tenantHeaderTrustingServices {
		if _, ok := found[svc]; !ok {
			dead = append(dead, svc+" (no longer reads X-Kanz-Principal-Tenant)")
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("tenantHeaderTrustingServices has %d stale entr(y/ies):\n  %s", len(dead), strings.Join(dead, "\n  "))
	}
}

// scrapePolicyPorts returns the ports allow-observability-scrape admits — the
// set the whole kanz-observability namespace may open in kanz-services.
//
// It is read from the POLICY rather than from the annotations, because the two
// are different claims: the annotation says where a service serves telemetry,
// the policy says what monitoring may reach. TestObservabilityScrapePortsMatch
// TheAnnotations pins them together for metrics ports; this reads the policy so
// a caller can ask the opposite question — "is this API port reachable by
// monitoring?" — which is the one that matters for a surface that trades.
func scrapePolicyPorts(t *testing.T) map[int]bool {
	t.Helper()
	out := map[int]bool{}
	for _, d := range allNetworkPolicies(t) {
		if d.Metadata.Name != "allow-observability-scrape" || d.Metadata.Namespace != "kanz-services" {
			continue
		}
		for _, r := range d.Spec.Ingress {
			for _, p := range r.Ports {
				out[p.Port] = true
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("allow-observability-scrape declares no ports — this helper is returning an empty " +
			"set and every assertion built on it passes by checking nothing")
	}
	return out
}

// TestTVSyncDeploymentListenerMode keeps the exception aligned with the deployed
// configuration while split-capable source is staged before image promotion.
// Enabling the API listener must retire the exception and satisfy the union of
// ingress policies; one narrow policy cannot cancel another policy's grant.
// Neither mode is evidence of enforcement by a deployed CNI.
func TestTVSyncDeploymentListenerMode(t *testing.T) {
	var policies []networkingv1.NetworkPolicy
	for _, path := range infraYAMLFiles(t) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		docs, err := decodeYAMLStream[yaml.Node](raw)
		if err != nil {
			if strings.Contains(string(raw), "kind: NetworkPolicy") {
				t.Fatalf("decode %s: %v", path, err)
			}
			continue
		}
		for _, doc := range docs {
			body, err := yaml.Marshal(&doc)
			if err != nil {
				t.Fatal(err)
			}
			var meta metav1.TypeMeta
			if err := sigyaml.Unmarshal(body, &meta); err != nil {
				t.Fatal(err)
			}
			if meta.Kind != "NetworkPolicy" {
				continue
			}
			var policy networkingv1.NetworkPolicy
			if err := sigyaml.Unmarshal(body, &policy); err != nil {
				t.Fatalf("decode policy %s: %v", path, err)
			}
			policies = append(policies, policy)
		}
	}
	found := false
	for _, w := range allWorkloads(t) {
		if w.Metadata.Namespace != "kanz-services" || w.app() != "tv-sync" {
			continue
		}
		found = true
		if got := w.Spec.Template.Metadata.Annotations["prometheus.io/port"]; got != "8091" {
			t.Errorf("tv-sync operations scrape port = %q, want 8091", got)
		}
		foundContainer := false
		split := false
		for _, c := range w.Spec.Template.Spec.Containers {
			if c.Name != "tv-sync" {
				continue
			}
			foundContainer = true
			env := map[string]string{}
			for _, e := range c.Env {
				env[e.Name] = e.Value
			}
			if env["TV_SYNC_LISTEN"] != ":8091" {
				t.Errorf("tv-sync operations listener must remain explicitly bound to :8091")
			}
			api, configured := env["TV_SYNC_API_LISTEN"]
			var err error
			split, err = tvSyncListenerMode(api, configured, tenantHeaderTrustingServices["tv-sync"], tenantHeaderTrustingSplitListeners["tv-sync"])
			if err != nil {
				t.Error(err)
			}
		}
		if !foundContainer {
			t.Error("tv-sync workload has no tv-sync container: listener configuration was not checked")
		}
		if split {
			for _, problem := range tvSyncIngressProblems(policies, w.Spec.Template.Metadata.Labels) {
				t.Error(problem)
			}
		}
	}
	if !found {
		t.Fatal("no deployed tv-sync workload: listener isolation was not checked")
	}
}

// Missing API_LISTEN deliberately retains the deployed legacy surface until a
// reviewed image and manifest promotion. An explicit value is never a fallback.
func tvSyncListenerMode(api string, configured bool, sharedPort, splitPort int) (bool, error) {
	if !configured {
		if sharedPort != 8091 || splitPort != 0 {
			return false, fmt.Errorf("tv-sync legacy deployment must retain only the shared-port :8091 exception under #232")
		}
		return false, nil
	}
	if api != ":8113" {
		return false, fmt.Errorf("tv-sync explicit API listener must be :8113, got %q", api)
	}
	if splitPort != 8113 || sharedPort != 0 {
		return true, fmt.Errorf("tv-sync split deployment must retire its shared-port exception and record only API :8113")
	}
	return true, nil
}

func TestTVSyncDeploymentListenerModeTransitions(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		api                  string
		configured           bool
		shared, split        int
		wantSplit, wantError bool
	}{
		{name: "staged legacy", shared: 8091},
		{name: "promoted split", api: ":8113", configured: true, split: 8113, wantSplit: true},
		{name: "blank explicit listener", configured: true, shared: 8091, wantError: true},
		{name: "collapsed listener", api: ":8091", configured: true, shared: 8091, wantError: true},
		{name: "legacy exemption retired early", split: 8113, wantError: true},
		{name: "split with stale legacy exemption", api: ":8113", configured: true, shared: 8091, wantSplit: true, wantError: true},
		{name: "both classifications", api: ":8113", configured: true, shared: 8091, split: 8113, wantSplit: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			split, err := tvSyncListenerMode(tc.api, tc.configured, tc.shared, tc.split)
			if split != tc.wantSplit || (err != nil) != tc.wantError {
				t.Fatalf("split=%v err=%v, want split=%v error=%v", split, err, tc.wantSplit, tc.wantError)
			}
		})
	}
}

func tvSyncIngressProblems(policies []networkingv1.NetworkPolicy, podLabels map[string]string) []string {
	return splitAPIIngressProblems(policies, podLabels, "tv-sync", 8113, 8091)
}

// Check the additive ingress union, including policies imported by an overlay.
// A narrow named rule cannot compensate for a second policy widening its target.
func splitAPIIngressProblems(policies []networkingv1.NetworkPolicy, podLabels map[string]string, app string, apiPort, opsPort int32) []string {
	return restrictedIngressProblems(policies, podLabels, app, "api-gateway", apiPort, opsPort)
}

// opsPort zero means this target has no separately admitted monitoring listener.
func restrictedIngressProblems(policies []networkingv1.NetworkPolicy, podLabels map[string]string, app, caller string, apiPort, opsPort int32) []string {
	var problems []string
	var gateway, monitoring, isolated bool
	for _, p := range policies {
		if p.Namespace != "kanz-services" {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(&p.Spec.PodSelector)
		if err != nil {
			problems = append(problems, p.Name+": invalid pod selector")
			continue
		}
		if !selector.Matches(labels.Set(podLabels)) {
			continue
		}
		for _, typ := range p.Spec.PolicyTypes {
			if typ == networkingv1.PolicyTypeIngress {
				isolated = true
			}
		}
		// Kubernetes defaults an omitted policyTypes to include Ingress.
		if len(p.Spec.PolicyTypes) == 0 {
			isolated = true
		}
		for _, rule := range p.Spec.Ingress {
			api, ops := len(rule.Ports) == 0, opsPort > 0 && len(rule.Ports) == 0
			for _, port := range rule.Ports {
				if port.Protocol != nil && *port.Protocol != corev1.ProtocolTCP {
					continue
				}
				if port.Port == nil {
					api, ops = true, opsPort > 0
					continue
				}
				if port.Port.Type == intstr.String {
					// Named container ports may be renamed independently; refuse an
					// unresolved name instead of silently excluding it from the union.
					problems = append(problems, p.Name+": "+app+" ingress uses unresolved named port "+port.Port.StrVal)
					continue
				}
				first, last := port.Port.IntVal, port.Port.IntVal
				if port.EndPort != nil {
					last = *port.EndPort
				}
				api = api || first <= apiPort && apiPort <= last
				ops = ops || opsPort > 0 && first <= opsPort && opsPort <= last
			}
			if ops && len(rule.From) == 0 {
				problems = append(problems, p.Name+": "+app+" operations admit every source")
			}
			if api && len(rule.From) == 0 {
				problems = append(problems, p.Name+": "+app+" API admits every source")
			}
			for _, peer := range rule.From {
				if api {
					local := peer.NamespaceSelector == nil || (peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == "kanz-services")
					onlyGateway := peer.IPBlock == nil && local && peer.PodSelector != nil && peer.PodSelector.MatchLabels["app"] == caller
					if !onlyGateway {
						problems = append(problems, p.Name+": "+app+" API admits an unauthorized peer")
						continue
					}
					// A restrictive expression must not accidentally remove the sole
					// authorized caller while the syntax still names it.
					ps, err := metav1.LabelSelectorAsSelector(peer.PodSelector)
					if err == nil && ps.Matches(labels.Set{"app": caller}) {
						if peer.NamespaceSelector == nil {
							gateway = true
						} else {
							ns, err := metav1.LabelSelectorAsSelector(peer.NamespaceSelector)
							gateway = gateway || err == nil && ns.Matches(labels.Set{"kubernetes.io/metadata.name": "kanz-services"})
						}
					}
				}
				if ops {
					if peer.IPBlock != nil || peer.NamespaceSelector == nil || peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kanz-observability" || peer.PodSelector != nil {
						problems = append(problems, p.Name+": "+app+" operations admit a non-monitoring peer")
						continue
					}
					ns, err := metav1.LabelSelectorAsSelector(peer.NamespaceSelector)
					monitoring = monitoring || err == nil && ns.Matches(labels.Set{"kubernetes.io/metadata.name": "kanz-observability"})
				}
			}
		}
	}
	if !isolated {
		problems = append(problems, app+" has no ingress isolation")
	}
	if !gateway {
		problems = append(problems, fmt.Sprintf("%s API :%d is unreachable by %s", app, apiPort, caller))
	}
	if opsPort > 0 && !monitoring {
		problems = append(problems, fmt.Sprintf("%s operations :%d are unreachable by monitoring", app, opsPort))
	}
	return problems
}

func TestTVSyncSplitListenerPolicyMutations(t *testing.T) {
	var baseline networkingv1.NetworkPolicy
	if err := sigyaml.Unmarshal([]byte(`
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: tv-sync, namespace: kanz-services}
spec:
  podSelector: {matchLabels: {app: tv-sync}}
  policyTypes: [Ingress]
  ingress:
    - from: [{podSelector: {matchLabels: {app: api-gateway}}}]
      ports: [{port: 8113}]
    - from: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: kanz-observability}}}]
      ports: [{port: 8091}]
`), &baseline); err != nil {
		t.Fatal(err)
	}
	podLabels := map[string]string{"app": "tv-sync"}
	if got := tvSyncIngressProblems([]networkingv1.NetworkPolicy{baseline}, podLabels); len(got) != 0 {
		t.Fatal(got)
	}
	for name, mutate := range map[string]func(*networkingv1.NetworkPolicy){
		"monitoring reaches API": func(p *networkingv1.NetworkPolicy) {
			p.Spec.Ingress[1].Ports[0].Port = &intstr.IntOrString{IntVal: 8113}
		},
		"all ports":              func(p *networkingv1.NetworkPolicy) { p.Spec.Ingress[1].Ports = nil },
		"port range reaches API": func(p *networkingv1.NetworkPolicy) { end := int32(8113); p.Spec.Ingress[1].Ports[0].EndPort = &end },
		"all sources":            func(p *networkingv1.NetworkPolicy) { p.Spec.Ingress[0].From = nil },
		"all namespaces": func(p *networkingv1.NetworkPolicy) {
			p.Spec.Ingress[0].From[0].NamespaceSelector = &metav1.LabelSelector{}
		},
		"gateway selector removed": func(p *networkingv1.NetworkPolicy) { p.Spec.Ingress[0].From[0].PodSelector = &metav1.LabelSelector{} },
		"gateway port stale": func(p *networkingv1.NetworkPolicy) {
			p.Spec.Ingress[0].Ports[0].Port = &intstr.IntOrString{IntVal: 8091}
		},
		"named port unresolved": func(p *networkingv1.NetworkPolicy) {
			value := intstr.FromString("api")
			p.Spec.Ingress[1].Ports[0].Port = &value
		},
	} {
		t.Run(name, func(t *testing.T) {
			mutant := baseline.DeepCopy()
			mutate(mutant)
			if got := tvSyncIngressProblems([]networkingv1.NetworkPolicy{*mutant}, podLabels); len(got) == 0 {
				t.Fatal("mutation escaped")
			}
		})
	}
	t.Run("additive wildcard policy", func(t *testing.T) {
		wide := baseline.DeepCopy()
		wide.Spec.PodSelector = metav1.LabelSelector{}
		wide.Spec.Ingress[0].From = nil
		if got := tvSyncIngressProblems([]networkingv1.NetworkPolicy{baseline, *wide}, podLabels); len(got) == 0 {
			t.Fatal("additive ingress escaped")
		}
	})
	if got := tvSyncIngressProblems(nil, podLabels); len(got) == 0 {
		t.Fatal("empty policy inventory passed")
	}
}
