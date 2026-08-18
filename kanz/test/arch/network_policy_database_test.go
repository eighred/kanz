package arch

import (
	"os"
	"path/filepath"
	"testing"
)

// infra/security/runtime/network-policies.yaml default-denies kanz-services in BOTH
// directions and then opens each flow the estate needs. It shipped WITHOUT the
// database — the one dependency almost every service in that namespace has — so
// applying it took the estate down: every kanz-migrate init container failed with
//
//	acquire: failed to connect to `user=kanzapp database=kanzapp`: connect: connection refused
//
// which reads as a bad credential or a sick database and is neither.
//
// It survived because nothing ever enforced it. The file's own comment on
// allow-gateway-to-read-upstreams says so: "kindnetd does not enforce NetworkPolicy",
// and the rig runs kind. A policy set that has never met an enforcing CNI is a
// hypothesis about connectivity, not a control over it — the same shape as the NATS
// manifest that had never been applied and the operator's egress rule that made the
// operator unable to reach the Kubernetes API.
//
// This guard cannot run a cluster, so it pins the one property whose absence took
// everything down: the database is reachable, from both ends, on one port. Deleting
// either half is a silent outage of the whole namespace, and it must not be possible
// to do it without a test going red.
func TestNetworkPoliciesReachTheDatabase(t *testing.T) {
	path := filepath.Join(moduleRoot(t), "infra", "security", "runtime", "network-policies.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	docs := decodeNetworkPolicyBytes(t, body, path)

	var sawDefaultDeny, sawEgress, sawIngress bool
	for _, d := range docs {
		if d.Kind != "NetworkPolicy" || d.Metadata.Namespace != "kanz-services" {
			continue
		}
		if d.Metadata.Name == "default-deny-all" {
			sawDefaultDeny = true
		}
		for _, r := range d.Spec.Egress {
			if !rulePermitsPort(r, 5432) {
				continue
			}
			for _, peer := range r.To {
				if peer.PodSelector != nil && peer.PodSelector.MatchLabels["app"] == "postgres" {
					sawEgress = true
				}
			}
		}
		if d.Spec.PodSelector.MatchLabels["app"] == "postgres" {
			for _, r := range d.Spec.Ingress {
				if rulePermitsPort(r, 5432) && len(r.From) > 0 {
					sawIngress = true
				}
			}
		}
	}

	// Non-vacuity: if the default-deny is gone, the whole namespace is open and this
	// guard is asserting a property that no longer matters. Say so rather than pass.
	if !sawDefaultDeny {
		t.Fatal("network-policies.yaml no longer declares default-deny-all in kanz-services. " +
			"This guard exists to check the database is still reachable UNDER deny-by-default; " +
			"without it the check is meaningless and the namespace is wide open.")
	}
	if !sawEgress {
		t.Error("no egress rule in kanz-services permits TCP:5432 to a pod labelled app=postgres. " +
			"Under default-deny that means NOTHING can reach the database: every service that " +
			"stores state and every kanz-migrate init container fails at startup with a " +
			"connection-refused that looks like a bad password.")
	}
	if !sawIngress {
		t.Error("no ingress rule on app=postgres permits TCP:5432 from anything. Both halves are " +
			"required — default-deny-all selects every pod for BOTH directions, so an egress rule " +
			"alone still leaves the database refusing the connection its client was just permitted " +
			"to open.")
	}
}

// THE ORDER PATH. INFRA-M7a made every venue an out-of-process adapter, so the OMS
// reaches venue-binance / venue-okx over venue.v1 gRPC on :9000 — the last hop of the
// trading loop and the only way an order leaves this platform. The policy set did not
// mention the venue adapters at all: nothing could dial them and they could accept
// nothing.
//
// It fails in the worst available way. A MIC with no reachable adapter is fatal at
// the router — the OMS asks each adapter who it is (venue.v1.Describe) before
// registering it and refuses to start if one will not answer — so the symptom is a
// CrashLoopBackOff on the ORDER MANAGEMENT SERVICE, reporting "cannot ask the adapter
// which exchange account it holds (is it running?)" while the adapter runs perfectly.
//
// The rig never caught it because its OMS uses the in-process SimVenue, which needs
// no network. The gap appears only when a real exchange is wired up — the one
// configuration where being wrong costs money.
func TestNetworkPoliciesReachTheVenueAdapters(t *testing.T) {
	path := filepath.Join(moduleRoot(t), "infra", "security", "runtime", "network-policies.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	docs := decodeNetworkPolicyBytes(t, body, path)

	const venuePort = 9000
	egressTo := map[string]bool{}  // adapter app -> OMS may dial it
	ingressOn := map[string]bool{} // adapter app -> it accepts :9000

	for _, d := range docs {
		if d.Kind != "NetworkPolicy" || d.Metadata.Namespace != "kanz-services" {
			continue
		}
		if d.Spec.PodSelector.MatchLabels["app"] == "oms" {
			for _, r := range d.Spec.Egress {
				if !rulePermitsPort(r, venuePort) {
					continue
				}
				for _, peer := range r.To {
					if peer.PodSelector != nil {
						egressTo[peer.PodSelector.MatchLabels["app"]] = true
					}
				}
			}
		}
		// The ingress half may select the adapters by label or by a matchExpressions
		// set; accept either, and record which apps it covers.
		for _, r := range d.Spec.Ingress {
			if !rulePermitsPort(r, venuePort) {
				continue
			}
			fromOMS := false
			for _, peer := range r.From {
				if peer.PodSelector != nil && peer.PodSelector.MatchLabels["app"] == "oms" {
					fromOMS = true
				}
			}
			if !fromOMS {
				continue
			}
			if app := d.Spec.PodSelector.MatchLabels["app"]; app != "" {
				ingressOn[app] = true
			}
			for _, e := range d.Spec.PodSelector.MatchExpressions {
				if e.Key == "app" && e.Operator == "In" {
					for _, v := range e.Values {
						ingressOn[v] = true
					}
				}
			}
		}
	}

	for _, adapter := range []string{"venue-binance", "venue-okx"} {
		if !egressTo[adapter] {
			t.Errorf("no egress rule lets the OMS dial %s on :%d. The OMS does not merely fail to "+
				"trade without this — it REFUSES TO START, because an adapter it cannot reach is a "+
				"fatal error at the router. The symptom is a crash-looping OMS blaming a healthy "+
				"adapter.", adapter, venuePort)
		}
		if !ingressOn[adapter] {
			t.Errorf("no ingress rule lets %s accept :%d from the OMS. Both halves are required — "+
				"default-deny-all selects every pod for BOTH directions, so an egress rule alone "+
				"still leaves the adapter refusing the connection.", adapter, venuePort)
		}
	}
}

// rulePermitsPort reports whether the rule names port p. A rule with NO ports is
// all-ports, which trivially includes it.
// BOTH HALVES OF THE GATEWAY'S ROUTE TO ITS OWN IdP (#530).
//
// The gateway verifies every token against a key it fetches from identity's
// unauthenticated /jwks.json (services/identity/internal/server/server.go:7).
// The INGRESS half — identity admitting the gateway on :8087 — has existed since
// #364. The EGRESS half did not, and this file's own rule says "Both ends must
// exist": with only one, the gateway holds no key, verifies no token, and since
// #457 does not even reach the Service — it stays out, logs at ERROR and exports
// kanz_api_gateway_oidc_issuer_reachable=0. A total authentication outage.
//
// IT SURVIVED BECAUSE NOTHING COULD SEE IT. kindnetd does not enforce
// NetworkPolicy, so on the dev rig a missing egress rule behaves exactly like a
// present one, and there is no other cluster in this environment. Both halves
// were read from manifests, which is precisely the kind of evidence that needs a
// guard rather than a reviewer.
//
// PAIRED, NOT SINGLE. Asserting only the egress half would let somebody delete
// the ingress half and leave the flow just as broken — the asymmetry that made
// this a two-day gap in the first place.
func TestNetworkPoliciesReachTheIdentityService(t *testing.T) {
	path := filepath.Join(moduleRoot(t), "infra", "security", "runtime", "network-policies.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	docs := decodeNetworkPolicyBytes(t, body, path)

	const jwksPort = 8087
	gatewayEgress, identityIngress := false, false

	for _, d := range docs {
		if d.Kind != "NetworkPolicy" || d.Metadata.Namespace != "kanz-services" {
			continue
		}
		if d.Spec.PodSelector.MatchLabels["app"] == "api-gateway" {
			for _, r := range d.Spec.Egress {
				if !rulePermitsPort(r, jwksPort) {
					continue
				}
				for _, peer := range r.To {
					if peer.PodSelector != nil && peer.PodSelector.MatchLabels["app"] == "identity" {
						gatewayEgress = true
					}
				}
			}
		}
		if d.Spec.PodSelector.MatchLabels["app"] == "identity" {
			for _, r := range d.Spec.Ingress {
				if !rulePermitsPort(r, jwksPort) {
					continue
				}
				for _, peer := range r.From {
					if peer.PodSelector != nil && peer.PodSelector.MatchLabels["app"] == "api-gateway" {
						identityIngress = true
					}
				}
			}
		}
	}

	// NON-VACUITY. A decoder that stopped parsing this file, or a namespace
	// rename, would leave both false and report the defect this guards against
	// while proving nothing — so fail differently when NEITHER half is found.
	if !gatewayEgress && !identityIngress {
		t.Fatalf("found neither half of the gateway↔identity flow in %s — the scan is broken, not "+
			"the estate: allow-ingress-to-identity has been in this file since #364", path)
	}
	if !gatewayEgress {
		t.Error("no kanz-services Egress rule lets app=api-gateway reach app=identity on :8087.\n\n" +
			"The gateway cannot fetch /jwks.json, so it holds no verification key and every " +
			"authenticated request is refused — and since #457 the pod stays out of the Service " +
			"entirely. allow-external-https-egress does not cover it: identity is an RFC1918 " +
			"ClusterIP, which that policy `except:`s, on a port it does not grant (#530).")
	}
	if !identityIngress {
		t.Error("no kanz-services Ingress rule on app=identity admits app=api-gateway on :8087.\n\n" +
			"The egress half alone permits a call nothing accepts. Both ends must exist — this " +
			"file says so, and #530 is what the asymmetry costs.")
	}
}

func rulePermitsPort(r netPolRule, p int) bool {
	if len(r.Ports) == 0 {
		return true
	}
	for _, port := range r.Ports {
		if port.Port == p {
			return true
		}
	}
	return false
}
