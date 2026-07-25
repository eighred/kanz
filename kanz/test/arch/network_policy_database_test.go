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

// rulePermitsPort reports whether the rule names port p. A rule with NO ports is
// all-ports, which trivially includes it.
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
