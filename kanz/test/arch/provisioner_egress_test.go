package arch

import (
	"strconv"
	"strings"
	"testing"
)

// findNodeProvisionerEgressPolicy returns the node-provisioner-egress
// NetworkPolicy in kanz-operator, if present.
func findNodeProvisionerEgressPolicy(docs []networkPolicyDoc) (networkPolicyDoc, bool) {
	for _, d := range docs {
		if d.Kind == "NetworkPolicy" && d.Metadata.Name == "node-provisioner-egress" && d.Metadata.Namespace == "kanz-operator" {
			return d, true
		}
	}
	return networkPolicyDoc{}, false
}

// validateProvisionerDNSRulePeers asserts the DNS rule's `to:` list is
// exactly one peer: a namespaceSelector matching kube-system, nothing else —
// the same shape operator-egress's DNS rule requires. An empty `to:` (the
// pre-fix shape) or a wildcard/second peer must fail here.
func validateProvisionerDNSRulePeers(t *testing.T, rule netPolRule) {
	t.Helper()
	if len(rule.To) != 1 {
		descs := make([]string, len(rule.To))
		for i, p := range rule.To {
			descs[i] = p.describe()
		}
		t.Errorf("node-provisioner-egress DNS rule (ports %v) has %d peers %v, want exactly 1 (kube-system namespaceSelector) — an empty or extra peer widens DNS egress beyond kube-system", rule.Ports, len(rule.To), descs)
		return
	}
	peer := rule.To[0]
	wantLabels := map[string]string{"kubernetes.io/metadata.name": "kube-system"}
	if peer.NamespaceSelector == nil {
		t.Errorf("node-provisioner-egress DNS rule's peer is %s, want namespaceSelector{matchLabels:%v} scoped to kube-system", peer.describe(), wantLabels)
		return
	}
	if !stringMapEqual(peer.NamespaceSelector.MatchLabels, wantLabels) {
		t.Errorf("node-provisioner-egress DNS rule's namespaceSelector.matchLabels = %v, want exactly %v — a wildcard or mismatched selector reaches more than kube-system", peer.NamespaceSelector.MatchLabels, wantLabels)
	}
	if peer.IPBlock != nil || peer.PodSelector != nil {
		t.Errorf("node-provisioner-egress DNS rule's peer unexpectedly sets more than one selector kind: %s", peer.describe())
	}
}

// TestProvisionerEgressIsBounded asserts the SEC-M6 node-provisioner-egress
// NetworkPolicy grants exactly DNS (53/UDP+TCP, scoped to kube-system) and SSH
// (22/TCP, destination-open by necessity — the provisioner dials an
// operator-supplied node IP that has no fixed CIDR), and nothing else. The
// provisioner Job pod itself never dials :6443 or :443 — the k3s join curl
// runs on the target node over the SSH session, not in the pod (see
// cmd/kanz-provisioner/join.go) — so those ports must not appear here. Unlike
// operator-egress, this guard's "no empty to:" rule is per-rule rather than
// global: the SSH rule's open `to:` is the one intentional exception, but its
// port set is still pinned so it cannot silently widen.
func TestProvisionerEgressIsBounded(t *testing.T) {
	docs := decodeOperatorNetworkPolicies(t)
	pol, ok := findNodeProvisionerEgressPolicy(docs)
	if !ok {
		t.Fatal("no NetworkPolicy named node-provisioner-egress found in kanz-operator")
	}

	wantPodSelector := map[string]string{"app.kubernetes.io/component": "node-provisioner"}
	if !stringMapEqual(pol.Spec.PodSelector.MatchLabels, wantPodSelector) {
		t.Errorf("node-provisioner-egress podSelector.matchLabels = %v, want exactly %v", pol.Spec.PodSelector.MatchLabels, wantPodSelector)
	}

	if len(pol.Spec.PolicyTypes) != 1 || pol.Spec.PolicyTypes[0] != "Egress" {
		t.Errorf("node-provisioner-egress policyTypes = %v, want exactly [Egress]", pol.Spec.PolicyTypes)
	}

	wantPorts := map[string]bool{"UDP:53": true, "TCP:53": true, "TCP:22": true}
	gotPorts := map[string]bool{}

	const wantDNSPortKey = "TCP:53,UDP:53"
	const wantSSHPortKey = "TCP:22"
	var dnsRules, sshRules []netPolRule

	for _, rule := range pol.Spec.Egress {
		for _, p := range rule.Ports {
			gotPorts[strings.ToUpper(p.Protocol)+":"+strconv.Itoa(p.Port)] = true
		}
		switch rulePortKey(rule) {
		case wantDNSPortKey:
			dnsRules = append(dnsRules, rule)
		case wantSSHPortKey:
			sshRules = append(sshRules, rule)
		default:
			// Any other port shape (e.g. a re-added :6443/:443 rule) is
			// unexpected regardless of its `to:` — fail below via the port-set
			// check, but also flag it here so the offending rule is visible.
			t.Errorf("node-provisioner-egress has an egress rule with unexpected port set %v (key %q) — only the DNS (53) and SSH (22) rules are allowed", rule.Ports, rulePortKey(rule))
		}
	}

	for want := range wantPorts {
		if !gotPorts[want] {
			t.Errorf("node-provisioner-egress missing port %s", want)
		}
	}
	for got := range gotPorts {
		if !wantPorts[got] {
			t.Errorf("node-provisioner-egress grants unexpected port %s — the provisioner pod only dials DNS (53) and SSH (22); the k3s join (:6443/:443) happens on the target node, not in this pod", got)
		}
	}

	switch len(dnsRules) {
	case 0:
		t.Errorf("node-provisioner-egress has no rule with ports [TCP:53 UDP:53] — the DNS rule is missing or mis-shaped")
	case 1:
		validateProvisionerDNSRulePeers(t, dnsRules[0])
	default:
		t.Errorf("node-provisioner-egress has %d rules with ports [TCP:53 UDP:53] (want exactly 1)", len(dnsRules))
	}

	switch len(sshRules) {
	case 0:
		t.Fatal("node-provisioner-egress has no rule with ports [TCP:22] — the SSH rule is missing or mis-shaped")
	case 1:
		// The SSH rule's `to:` is intentionally open — the provisioner SSHes
		// to an arbitrary operator-supplied node IP, so there is no fixed
		// CIDR to scope it to. Only the port set is pinned here.
		if got := rulePortKey(sshRules[0]); got != wantSSHPortKey {
			t.Errorf("node-provisioner-egress SSH rule port key = %q, want %q", got, wantSSHPortKey)
		}
	default:
		t.Errorf("node-provisioner-egress has %d rules with ports [TCP:22] (want exactly 1)", len(sshRules))
	}
}

// TestProvisionerEgressGuardIsNonVacuous proves TestProvisionerEgressIsBounded's
// lookup can actually fail: it strips the node-provisioner-egress doc from the
// decoded manifest and asserts findNodeProvisionerEgressPolicy no longer finds
// it. Modelled on TestOperatorEgressGuardIsNonVacuous.
func TestProvisionerEgressGuardIsNonVacuous(t *testing.T) {
	docs := decodeOperatorNetworkPolicies(t)

	var stripped []networkPolicyDoc
	for _, d := range docs {
		if d.Kind == "NetworkPolicy" && d.Metadata.Name == "node-provisioner-egress" {
			continue
		}
		stripped = append(stripped, d)
	}
	if len(stripped) != len(docs)-1 {
		t.Fatalf("stripping node-provisioner-egress removed %d docs (want exactly 1) — decode is broken, this guard proves nothing", len(docs)-len(stripped))
	}

	if _, ok := findNodeProvisionerEgressPolicy(stripped); ok {
		t.Fatal("findNodeProvisionerEgressPolicy still found node-provisioner-egress after it was stripped — the lookup can never fail, so TestProvisionerEgressIsBounded cannot catch a missing policy")
	}
}
