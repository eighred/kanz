package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// THE PUBLIC EDGE IS THE ONE THING AN ATTACKER GETS TO CHOOSE.
//
// Everything else on this platform is reachable only from inside a default-deny
// namespace. An Ingress is a decision to publish a pod to the internet, and the
// consequences of a sloppy one are not subtle:
//
//   - tv-sync AUTHENTICATES NOTHING. It reads X-Kanz-Principal-Tenant and serves
//     that tenant's book, because the api-gateway is the sole identity authority and
//     the network is what stops anyone else reaching it. An Ingress for tv-sync — one
//     file, six lines — would let ANY CALLER NAME ANY TENANT AND READ THAT TENANT'S
//     BOOK. Nothing in the service would refuse, and nothing would look wrong.
//
//   - Every service serves /healthz, /readyz and /metrics on the SAME PORT as its
//     real surface. An Ingress with a `/` Prefix rule publishes the platform's
//     Prometheus metrics — order counts, breach counters, venue latencies — to the
//     internet, and nobody would notice, because the route that mattered would work.
//
// These are properties of FILES, so a file can check them. This test does not prove
// the cluster is safe; it proves nobody added a route that is obviously unsafe.
//
// NOTE ON WHAT IS *NOT* PROVEN HERE: the NetworkPolicy assertions below check that
// the rules EXIST, not that they are ENFORCED. kind runs kindnetd, which ignores
// NetworkPolicy entirely, so every in-cluster proof this repository has ever run
// would have passed with no policies at all. The policies are unexecuted until they
// run on a CNI that enforces them (Cilium/Calico).

const (
	deployDir = "../../infra/deploy"
	netpolFil = "../../infra/security/runtime/network-policies.yaml"
)

// publishable is the complete list of services allowed to have an Ingress. It is a
// list, not a rule, because "may this pod be on the internet" is a judgement no
// heuristic makes for you — and adding a name to it should be as deliberate as it
// looks.
//
//	api-gateway    — authenticates every request under /v1 before it reaches an
//	                 upstream. It is the reason the upstreams may trust a header.
//	webhook-ingest — authenticates every signal by HMAC over the raw body. It has to
//	                 be reachable: TradingView dials it, and nothing else can.
//
// WHAT "AUTHENTICATES" MEANS FOR THE SECOND ONE, stated because the difference
// was a P0. The HMAC proves the sender holds a STRATEGY's secret — not a fund,
// and not a tenant. Everything else about the request, `fund_id` included, is
// caller-supplied text that happens to be covered by that signature. Being on
// this list is therefore not a claim that a signed request may do whatever it
// asks: the fund it names is checked against a configured strategy→fund→tenant
// binding before any order is published (#632, translate.FundAuthority). Until
// that binding existed the tenant WAS `fund_id`, and one leaked strategy secret
// reached every tenant this deployment served — from an Ingress this file
// approved.
//
// tv-sync is NOT here, and that is the point of this test.
var publishable = map[string]bool{
	"api-gateway":    true,
	"webhook-ingest": true,
}

// TestNoIngressPublishesAnUnauthenticatedService fails the build if a manifest
// exposes a service that cannot defend itself.
func TestNoIngressPublishesAnUnauthenticatedService(t *testing.T) {
	for _, f := range manifests(t) {
		body := read(t, f)
		if !strings.Contains(body, "kind: Ingress") {
			continue
		}
		for _, svc := range ingressBackends(body) {
			if !publishable[svc] {
				t.Errorf("%s publishes %q to the internet, and %q is not on the publishable list.\n\n"+
					"If this is tv-sync: it authenticates NOTHING. It serves whatever tenant the "+
					"X-Kanz-Principal-Tenant header names, because the api-gateway is the only thing "+
					"that can reach it and the only thing that sets that header. On the internet, any "+
					"caller could name any tenant and read that tenant's book.\n\n"+
					"If it is something else: put it behind the gateway, or add it to `publishable` and "+
					"say in the manifest what authenticates a request before it reaches a pod.",
					filepath.Base(f), svc, svc)
			}
		}
	}
}

// TestNoIngressExposesTheOperationalSurface fails on a `/` Prefix rule, which would
// publish /healthz, /readyz and /metrics along with the route somebody meant to add.
func TestNoIngressExposesTheOperationalSurface(t *testing.T) {
	pathRe := regexp.MustCompile(`(?m)^\s*-?\s*path:\s*"?([^"\s#]+)"?`)
	for _, f := range manifests(t) {
		body := read(t, f)
		if !strings.Contains(body, "kind: Ingress") {
			continue
		}
		for _, m := range pathRe.FindAllStringSubmatch(body, -1) {
			if p := m[1]; p == "/" || p == "/*" {
				t.Errorf("%s exposes %q — every service serves /healthz, /readyz and /metrics on the "+
					"same port as its real surface, so this publishes the platform's metrics to the "+
					"internet. Name the paths you mean.", filepath.Base(f), p)
			}
		}
	}
}

// TestTheWebhookIngressCarriesTheClientIPPerimeter.
//
// webhook-ingest has its own IP allowlist, and it is checked against the CONNECTION
// PEER (r.RemoteAddr — X-Forwarded-For is not trusted, because a client can forge
// it). Behind an L7 Ingress the peer is ALWAYS the ingress controller and NEVER
// TradingView, so that allowlist cannot see a client IP and the perimeter has to live
// at the only layer that can. If this annotation goes missing, the outer wall is gone
// and the route is open to anyone who finds the hostname — with only the HMAC left.
func TestTheWebhookIngressCarriesTheClientIPPerimeter(t *testing.T) {
	body := read(t, filepath.Join(deployDir, "webhook-ingest-ingress.yaml"))
	if !strings.Contains(body, "whitelist-source-range") {
		t.Fatal("the webhook Ingress has no whitelist-source-range. That annotation is the ONLY " +
			"client-IP perimeter on the trading loop's entry point: the pod's own allowlist checks " +
			"the connection peer, which behind this Ingress is nginx, not TradingView.")
	}
}

// TestEveryProxiedUpstreamIsReachable.
//
// The kanz-services namespace is DEFAULT-DENY in both directions. A route added to
// the gateway's proxy is not reachable because it compiles — it is reachable because
// a NetworkPolicy says the gateway may talk to that pod. Miss the policy and the
// route 502s in production while every test passes, because kind does not enforce
// NetworkPolicy.
//
// The proxy.Service constants are the pod `app` labels, so this is exact.
func TestEveryProxiedUpstreamIsReachable(t *testing.T) {
	src := read(t, "../../services/api-gateway/internal/proxy/proxy.go")
	constRe := regexp.MustCompile(`Service\s*=\s*"([a-z0-9-]+)"`)
	upstreams := constRe.FindAllStringSubmatch(src, -1)
	if len(upstreams) == 0 {
		t.Fatal("found no proxy.Service constants — this test has lost its subject")
	}

	netpol := read(t, netpolFil)
	egress, ok := policy(netpol, "allow-gateway-to-read-upstreams")
	if !ok {
		t.Fatal("no allow-gateway-to-read-upstreams NetworkPolicy: the gateway cannot reach ANY " +
			"upstream in a default-deny namespace")
	}
	selected := podSelectors(egress)

	// THE INGRESS HALF IS CHECKED TOO, and it is the half that matters for safety.
	//
	// This guard used to assert egress only, while its own message promised "and an
	// ingress rule on the target" — so deleting every ingress rule in the file left
	// it green. The two failures are not symmetric: a missing EGRESS rule breaks
	// the route, which someone notices within minutes. A missing INGRESS rule
	// leaves the upstream reachable by every pod in the namespace, and every one of
	// these upstreams decides what a caller may see — or, for optimization, who a
	// capital-moving command is attributed to — from the principal header the
	// gateway injects. Reachable by anyone means that header is self-declared.
	//
	// Both ends must exist for a NetworkPolicy to constrain anything: default-deny
	// covers both directions, so an egress allow with no matching ingress allow is
	// a connection that is still refused (#232's own lesson, one file over).
	admitted := ingressAdmitted(t, netpol)

	for _, m := range upstreams {
		svc := m[1]
		if !selected[svc] {
			t.Errorf("the gateway proxies to %q but no egress rule permits it. In a default-deny "+
				"namespace that route 502s in production — and passes every test here, because kind's "+
				"CNI ignores NetworkPolicy. Add %q to allow-gateway-to-read-upstreams (and an ingress "+
				"rule on the target).", svc, svc)
		}
		if !admitted[svc] {
			t.Errorf("the gateway proxies to %q but NO ingress policy selects it. Egress alone "+
				"permits nothing: default-deny covers both directions, so the call is still refused — "+
				"and if the namespace's deny is ever relaxed, this upstream is reachable by every pod "+
				"in it. These services read the gateway-injected principal to decide what a caller may "+
				"see and, for the trading surfaces, whose name goes on an order. Reachable by anyone "+
				"means that principal is self-declared. Add an ingress policy selecting app=%q that "+
				"admits api-gateway.", svc, svc)
		}
	}
}

// ingressAdmitted returns the pod `app` values some Ingress policy in the
// document selects — i.e. the upstreams that have an ingress rule at all.
//
// It splits the file on the YAML document separator and keeps only documents
// declaring policyTypes containing Ingress, so an EGRESS policy naming a service
// cannot satisfy the ingress assertion. That distinction is the whole point: the
// two halves are separate policies and only one of them is a security boundary.
func ingressAdmitted(t *testing.T, netpol string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	docs := strings.Split(netpol, "\n---")
	ingressDocs := 0
	for _, doc := range docs {
		clean := uncomment(doc)
		if !strings.Contains(clean, "Ingress") {
			continue
		}
		ingressDocs++
		for svc := range podSelectors(doc) {
			out[svc] = true
		}
	}
	// NON-VACUITY: this file demonstrably contains ingress policies. Zero means
	// the document split or the policyTypes match has broken and every assertion
	// above is passing by finding nothing to check.
	if ingressDocs < 3 {
		t.Fatalf("found %d ingress policy document(s) in %s — the split is broken and the ingress "+
			"half of this guard is checking nothing", ingressDocs, netpolFil)
	}
	return out
}

// podSelectors returns the pod `app` values a policy document selects, from both
// forms: `app: name` and `values: [a, b, c]` under a matchExpressions key.
//
// It reads the SELECTORS, not the document. Matching the raw text would find the
// service name in the policy's own comment prose — which is exactly what the first
// version of this test did, and it meant the assertion could not fail. A guard that
// cannot fail is worse than no guard: it reports safety it never checked.
func podSelectors(doc string) map[string]bool {
	out := map[string]bool{}
	appRe := regexp.MustCompile(`(?m)^\s*app:\s*([a-z0-9-]+)\s*$`)
	valRe := regexp.MustCompile(`(?m)^\s*values:\s*\[([^\]]*)\]`)
	for _, m := range appRe.FindAllStringSubmatch(uncomment(doc), -1) {
		out[m[1]] = true
	}
	for _, m := range valRe.FindAllStringSubmatch(uncomment(doc), -1) {
		for _, v := range strings.Split(m[1], ",") {
			out[strings.TrimSpace(v)] = true
		}
	}
	return out
}

// uncomment drops YAML comment lines so prose cannot satisfy an assertion.
func uncomment(doc string) string {
	var keep []string
	for _, line := range strings.Split(doc, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			keep = append(keep, line)
		}
	}
	return strings.Join(keep, "\n")
}

// policy returns the YAML document declaring the named NetworkPolicy.
func policy(body, name string) (string, bool) {
	for _, doc := range strings.Split(body, "\n---") {
		if strings.Contains(uncomment(doc), "name: "+name+"\n") {
			return doc, true
		}
	}
	return "", false
}

// ingressBackends returns the service names an Ingress routes to.
func ingressBackends(body string) []string {
	re := regexp.MustCompile(`(?s)backend:\s*\n\s*service:\s*\n\s*name:\s*([a-z0-9-]+)`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

func manifests(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(deployDir, "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no manifests under %s: %v", deployDir, err)
	}
	return files
}

// read normalizes line endings. core.autocrlf checks these files out as CRLF on
// Windows, and every assertion below is a line-oriented string match — without this
// the whole file silently matches nothing, and a test that cannot fail is worse than
// no test.
func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}
