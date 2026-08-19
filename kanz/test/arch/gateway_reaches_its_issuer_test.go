package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// THE GATEWAY MUST BE ABLE TO REACH THE ISSUER IT VERIFIES AGAINST (#530).
//
// # What this protects, and why nothing else can
//
// The gateway fetches identity's JWKS before it can verify a single token. That
// flow is the gateway's OWN dependency, made on its own behalf, and it is the
// only gateway egress whose absence is an outage for EVERY route rather than for
// one — nobody signs in, no order is placed by anyone.
//
// The policy that permits it, allow-identity-egress-from-gateway, was added by
// #530 and is asserted by nothing. Deleting it would be caught by:
//
//   - not the rig. network-policies.yaml says so itself: "kindnetd does not
//     enforce NetworkPolicy, so on the rig a missing egress rule behaves exactly
//     like a present one and the gateway's JWKS fetch succeeds either way. The
//     rig cannot show this gap and never will."
//   - not CI, which runs no cluster at all.
//   - not a unit test, because the failure is a manifest that is missing rather
//     than code that is wrong.
//
// So a manifest read is the ONLY available proof, and until this file existed
// nobody was performing it. That is the same shape as every other control this
// estate has lost: correct, deployed, and unprotected against its own deletion.
//
// # Why the port is cross-checked rather than pinned
//
// A hardcoded 8087 here would agree with a policy that had drifted away from the
// service. The port is read from identity's own manifest, so moving identity to
// a new port and forgetting the egress rule fails HERE — which is the realistic
// regression, not somebody deleting the policy outright.
func TestTheGatewayMayReachTheIssuerItVerifiesAgainst(t *testing.T) {
	root := moduleRoot(t)

	port := identityListenPort(t, root)
	policies := allNetworkPolicies(t)

	// NON-VACUITY. allNetworkPolicies already fails below 20 docs, but this guard
	// depends on finding gateway-selecting policies specifically: if the label or
	// the decode changed, every assertion below would pass by matching nothing.
	gatewayPolicies := 0
	for _, p := range policies {
		if p.Spec.PodSelector.MatchLabels["app"] == "api-gateway" {
			gatewayPolicies++
		}
	}
	if gatewayPolicies == 0 {
		t.Fatal("no NetworkPolicy selects app=api-gateway — the label moved or the decode broke, " +
			"and this guard would vouch for an egress path by finding nothing to check")
	}

	for _, p := range policies {
		if p.Spec.PodSelector.MatchLabels["app"] != "api-gateway" {
			continue
		}
		for _, rule := range p.Spec.Egress {
			if !peersInclude(rule.To, "identity") {
				continue
			}
			for _, pr := range rule.Ports {
				if pr.Port == port {
					return // permitted, on the port identity actually serves
				}
			}
			// A rule that names identity on the WRONG port is worse than none: it
			// reads in review as "the gateway may reach identity" while the JWKS
			// fetch is refused in production and nowhere else.
			t.Fatalf("%s permits egress to identity but not on port %d, which is the port "+
				"identity-deploy.yaml serves. The JWKS fetch is refused under an enforcing CNI "+
				"and the gateway never becomes ready — for every route, not one (#530).",
				p.Metadata.Name, port)
		}
	}

	t.Fatalf("NO NetworkPolicy permits app=api-gateway egress to app=identity on port %d.\n\n"+
		"The gateway fetches identity's JWKS before it can verify any token, so under an "+
		"enforcing CNI this is a total authentication outage: the pod probes the issuer, stays "+
		"out of the Service, and exports kanz_api_gateway_oidc_issuer_reachable=0. Nobody signs "+
		"in and no order is placed by anyone.\n\n"+
		"The rig cannot show this — kindnetd does not enforce NetworkPolicy, so a missing rule "+
		"behaves exactly like a present one there. This manifest read is the only proof "+
		"available, which is why it is a guard (#530).", port)
}

var identityListenRe = regexp.MustCompile(`IDENTITY_LISTEN[^\n]*?:\s*"?:(\d+)`)

// identityListenPort reads the port identity is configured to serve on, so the
// egress assertion tracks the service rather than a number copied once.
func identityListenPort(t *testing.T, root string) int {
	t.Helper()
	path := filepath.Join(root, "infra", "deploy", "identity-deploy.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := identityListenRe.FindStringSubmatch(string(body))
	if m == nil {
		t.Fatalf("no IDENTITY_LISTEN in %s — identity's port is configured somewhere else now, and "+
			"this guard is comparing the egress rule against nothing", path)
	}
	port, err := strconv.Atoi(m[1])
	if err != nil || port == 0 {
		t.Fatalf("IDENTITY_LISTEN is %q, which is not a port", m[1])
	}
	return port
}

// peersInclude reports whether any peer selects the given app by label.
func peersInclude(peers []netPolPeer, app string) bool {
	for _, p := range peers {
		if p.PodSelector.MatchLabels["app"] == app {
			return true
		}
	}
	return false
}

var _ = strings.TrimSpace
