package arch

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// netPolIPBlock captures the fields of a NetworkPolicy ipBlock peer this guard
// inspects.
type netPolIPBlock struct {
	CIDR   string   `yaml:"cidr"`
	Except []string `yaml:"except"`
}

// netPolSelector captures a namespaceSelector or podSelector peer's
// matchLabels. A present-but-empty selector (`{}`, matching everything) still
// decodes to a non-nil *netPolSelector with a nil/empty MatchLabels map, which
// is distinguishable from the selector being absent altogether (nil pointer).
type netPolSelector struct {
	MatchLabels map[string]string `yaml:"matchLabels"`
}

// netPolPeer is one entry of an egress rule's `to:` list. All three peer
// kinds NetworkPolicy supports are decoded so the guard can tell exactly what
// a peer is, not just that a `to:` list is non-empty.
type netPolPeer struct {
	IPBlock           *netPolIPBlock  `yaml:"ipBlock"`
	NamespaceSelector *netPolSelector `yaml:"namespaceSelector"`
	PodSelector       *netPolSelector `yaml:"podSelector"`
}

// describe renders a peer for failure messages, naming exactly what was
// found so an unexpected peer fails loudly instead of being silently
// skipped.
func (p netPolPeer) describe() string {
	switch {
	case p.IPBlock != nil:
		return fmt.Sprintf("ipBlock{cidr:%s except:%v}", p.IPBlock.CIDR, p.IPBlock.Except)
	case p.NamespaceSelector != nil:
		return fmt.Sprintf("namespaceSelector{matchLabels:%v}", p.NamespaceSelector.MatchLabels)
	case p.PodSelector != nil:
		return fmt.Sprintf("podSelector{matchLabels:%v}", p.PodSelector.MatchLabels)
	default:
		return "peer{no known selector decoded}"
	}
}

type netPolPort struct {
	Protocol string `yaml:"protocol"`
	Port     int    `yaml:"port"`
}

type netPolRule struct {
	To    []netPolPeer `yaml:"to"`
	Ports []netPolPort `yaml:"ports"`
}

// rulePortKey renders a rule's port set as a stable, order-independent key
// (e.g. "TCP:53,UDP:53") used to identify which documented rule a decoded
// rule corresponds to.
func rulePortKey(rule netPolRule) string {
	keys := make([]string, 0, len(rule.Ports))
	for _, p := range rule.Ports {
		keys = append(keys, strings.ToUpper(p.Protocol)+":"+strconv.Itoa(p.Port))
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// networkPolicyDoc captures only the fields this guard inspects from a
// multi-document manifest.
type networkPolicyDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		PodSelector struct {
			MatchLabels map[string]string `yaml:"matchLabels"`
		} `yaml:"podSelector"`
		PolicyTypes []string     `yaml:"policyTypes"`
		Egress      []netPolRule `yaml:"egress"`
	} `yaml:"spec"`
}

// decodeOperatorNetworkPolicies reads infra/deploy/operator-deploy.yaml into
// typed NetworkPolicy docs. This mirrors decodeOperatorManifest's multi-doc
// yaml.v3 decode pattern (operator_rbac_test.go) but uses its own doc type,
// since roleDoc has no `spec` field for podSelector/policyTypes/egress.
func decodeOperatorNetworkPolicies(t *testing.T) []networkPolicyDoc {
	t.Helper()
	path := filepath.Join(moduleRoot(t), "infra", "deploy", "operator-deploy.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return decodeNetworkPolicyBytes(t, body, path)
}

func decodeNetworkPolicyBytes(t *testing.T, body []byte, source string) []networkPolicyDoc {
	t.Helper()
	var docs []networkPolicyDoc
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	for {
		var d networkPolicyDoc
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("parse %s: %v", source, err)
		}
		docs = append(docs, d)
	}
	if len(docs) == 0 {
		t.Fatalf("no docs decoded from %s", source)
	}
	return docs
}

// findOperatorEgressPolicy returns the operator-egress NetworkPolicy in
// kanz-operator, if present.
func findOperatorEgressPolicy(docs []networkPolicyDoc) (networkPolicyDoc, bool) {
	for _, d := range docs {
		if d.Kind == "NetworkPolicy" && d.Metadata.Name == "operator-egress" && d.Metadata.Namespace == "kanz-operator" {
			return d, true
		}
	}
	return networkPolicyDoc{}, false
}

// stringMapEqual reports whether two string maps have exactly the same keys
// and values — no extras on either side.
func stringMapEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// validateKubeSystemDNSPeer asserts the DNS rule's `to:` list is exactly one peer:
// a namespaceSelector matching kube-system, nothing else. An empty `to:`, a
// wildcard `namespaceSelector: {}`, or an added second peer must fail here.
func validateKubeSystemDNSPeer(t *testing.T, policyName string, rule netPolRule) {
	t.Helper()
	if len(rule.To) != 1 {
		descs := make([]string, len(rule.To))
		for i, p := range rule.To {
			descs[i] = p.describe()
		}
		t.Errorf("%s DNS rule (ports %v) has %d peers %v, want exactly 1 (kube-system namespaceSelector) — an extra peer widens DNS egress beyond kube-system", policyName, rule.Ports, len(rule.To), descs)
		return
	}
	peer := rule.To[0]
	wantLabels := map[string]string{"kubernetes.io/metadata.name": "kube-system"}
	if peer.NamespaceSelector == nil {
		t.Errorf("%s DNS rule's peer is %s, want namespaceSelector{matchLabels:%v} scoped to kube-system", policyName, peer.describe(), wantLabels)
		return
	}
	if !stringMapEqual(peer.NamespaceSelector.MatchLabels, wantLabels) {
		t.Errorf("%s DNS rule's namespaceSelector.matchLabels = %v, want exactly %v — a wildcard or mismatched selector reaches more than kube-system", policyName, peer.NamespaceSelector.MatchLabels, wantLabels)
	}
	if peer.IPBlock != nil || peer.PodSelector != nil {
		t.Errorf("%s DNS rule's peer unexpectedly sets more than one selector kind: %s", policyName, peer.describe())
	}
}

// validate443RulePeers asserts the exchange-HTTPS rule's `to:` list is
// exactly one peer: an ipBlock of 0.0.0.0/0 scoped by the full except: list,
// nothing else. An added second peer (e.g. namespaceSelector: {}) or a
// missing/incomplete except: list must fail here.
func validate443RulePeers(t *testing.T, rule netPolRule) {
	t.Helper()
	if len(rule.To) != 1 {
		descs := make([]string, len(rule.To))
		for i, p := range rule.To {
			descs[i] = p.describe()
		}
		t.Errorf("operator-egress 443 rule (ports %v) has %d peers %v, want exactly 1 (ipBlock 0.0.0.0/0 with except:) — an extra peer (e.g. a namespaceSelector) reopens exactly the in-cluster/metadata access the except: list exists to forbid", rule.Ports, len(rule.To), descs)
		return
	}
	peer := rule.To[0]
	if peer.IPBlock == nil {
		t.Errorf("operator-egress 443 rule's peer is %s, want ipBlock{cidr:0.0.0.0/0}", peer.describe())
		return
	}
	if peer.NamespaceSelector != nil || peer.PodSelector != nil {
		t.Errorf("operator-egress 443 rule's peer unexpectedly sets more than one selector kind: %s", peer.describe())
	}
	if peer.IPBlock.CIDR != "0.0.0.0/0" {
		t.Errorf("operator-egress 443 rule's ipBlock.cidr = %q, want 0.0.0.0/0", peer.IPBlock.CIDR)
	}

	wantExcept := map[string]bool{
		"10.0.0.0/8":     true,
		"172.16.0.0/12":  true,
		"192.168.0.0/16": true,
		"169.254.0.0/16": true,
	}
	gotExcept := map[string]bool{}
	for _, e := range peer.IPBlock.Except {
		gotExcept[e] = true
	}
	for cidr := range wantExcept {
		if !gotExcept[cidr] {
			t.Errorf("operator-egress 443 rule's ipBlock.except is missing %s — this permission could reach in-cluster services or the cloud metadata endpoint", cidr)
		}
	}
	for _, e := range peer.IPBlock.Except {
		if !wantExcept[e] {
			t.Errorf("operator-egress 443 rule's ipBlock.except has unexpected entry %s — only the RFC1918 ranges and the link-local block should be excluded", e)
		}
	}
}

// TestOperatorEgressIsBounded asserts the S4b operator-egress NetworkPolicy grants
// exactly DNS (53/UDP+TCP) and exchange HTTPS (443/TCP), nothing else, that the 443
// rule is scoped to the public internet only, and that neither rule carries any
// additional peer beyond the one it is documented to have. This is the operator's
// first-ever outbound permission (venueproof.Prover's pre-write proof, see
// venueproof.go); a drift toward a wider port set, an extra `to:` peer, an unscoped
// `to:`, or a shrunken except: list would let this permission reach further than the
// one exchange call it exists for. Peer content is validated exhaustively — the test
// must fail on anything the policy permits beyond the documented set, not merely
// confirm the documented set is present.
func TestOperatorEgressIsBounded(t *testing.T) {
	docs := decodeOperatorNetworkPolicies(t)
	pol, ok := findOperatorEgressPolicy(docs)
	if !ok {
		t.Fatal("no NetworkPolicy named operator-egress found in kanz-operator")
	}

	wantPodSelector := map[string]string{"app": "operator"}
	if !stringMapEqual(pol.Spec.PodSelector.MatchLabels, wantPodSelector) {
		t.Errorf("operator-egress podSelector.matchLabels = %v, want exactly %v", pol.Spec.PodSelector.MatchLabels, wantPodSelector)
	}

	if len(pol.Spec.PolicyTypes) != 1 || pol.Spec.PolicyTypes[0] != "Egress" {
		t.Errorf("operator-egress policyTypes = %v, want exactly [Egress]", pol.Spec.PolicyTypes)
	}

	wantPorts := map[string]bool{"UDP:53": true, "TCP:53": true, "TCP:443": true}
	gotPorts := map[string]bool{}

	const wantDNSPortKey = "TCP:53,UDP:53"
	const want443PortKey = "TCP:443"
	var dnsRules, tcp443Rules []netPolRule

	for _, rule := range pol.Spec.Egress {
		// A rule with no `to:` peers is unrestricted egress on its ports — the
		// one thing this policy exists to forbid.
		if len(rule.To) == 0 {
			t.Errorf("operator-egress has an egress rule with an empty `to:` (ports %v) — unrestricted egress is forbidden", rule.Ports)
		}
		for _, p := range rule.Ports {
			gotPorts[strings.ToUpper(p.Protocol)+":"+strconv.Itoa(p.Port)] = true
		}
		switch rulePortKey(rule) {
		case wantDNSPortKey:
			dnsRules = append(dnsRules, rule)
		case want443PortKey:
			tcp443Rules = append(tcp443Rules, rule)
		}
	}

	for want := range wantPorts {
		if !gotPorts[want] {
			t.Errorf("operator-egress missing port %s", want)
		}
	}
	for got := range gotPorts {
		if !wantPorts[got] {
			t.Errorf("operator-egress grants unexpected port %s — only DNS (53) and exchange HTTPS (443) are allowed", got)
		}
	}

	// Exactly one rule per port shape — a second rule with the same ports
	// (e.g. an under-scoped duplicate 443 rule ordered before the correct
	// one) must fail here regardless of ordering, since every matching rule
	// is collected rather than only the last one seen.
	switch len(dnsRules) {
	case 0:
		t.Errorf("operator-egress has no rule with ports [TCP:53 UDP:53] — the DNS rule is missing or mis-shaped")
	case 1:
		validateKubeSystemDNSPeer(t, "operator-egress", dnsRules[0])
	default:
		t.Errorf("operator-egress has %d rules with ports [TCP:53 UDP:53] (want exactly 1)", len(dnsRules))
	}

	switch len(tcp443Rules) {
	case 0:
		t.Fatal("operator-egress has no ipBlock: {cidr: 0.0.0.0/0} rule — the exchange-egress rule is missing or mis-shaped")
	case 1:
		validate443RulePeers(t, tcp443Rules[0])
	default:
		t.Errorf("operator-egress has %d rules with ports [TCP:443] (want exactly 1) — a duplicate 443 rule can under-scope the except: list while the correctly-scoped rule masks it", len(tcp443Rules))
		for _, r := range tcp443Rules {
			validate443RulePeers(t, r)
		}
	}
}

// TestOperatorEgressGuardIsNonVacuous proves TestOperatorEgressIsBounded's lookup
// can actually fail: it strips the operator-egress doc from the decoded manifest
// and asserts findOperatorEgressPolicy no longer finds it. A guard whose "found"
// check can never be false is not a guard — it reads as proof of a property it
// never checked (the same failure mode ssh_plane_test.go and operator_rbac_test.go
// guard against with their own non-vacuity checks).
func TestOperatorEgressGuardIsNonVacuous(t *testing.T) {
	docs := decodeOperatorNetworkPolicies(t)

	var stripped []networkPolicyDoc
	for _, d := range docs {
		if d.Kind == "NetworkPolicy" && d.Metadata.Name == "operator-egress" {
			continue
		}
		stripped = append(stripped, d)
	}
	if len(stripped) != len(docs)-1 {
		t.Fatalf("stripping operator-egress removed %d docs (want exactly 1) — decode is broken, this guard proves nothing", len(docs)-len(stripped))
	}

	if _, ok := findOperatorEgressPolicy(stripped); ok {
		t.Fatal("findOperatorEgressPolicy still found operator-egress after it was stripped — the lookup can never fail, so TestOperatorEgressIsBounded cannot catch a missing policy")
	}
}
