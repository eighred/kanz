package arch

import (
	"errors"
	"io"
	"os"
	"path/filepath"
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

// netPolPeer is one entry of an egress rule's `to:` list. Only ipBlock is
// decoded into a typed field — namespaceSelector/podSelector peers are still
// counted (the slice length is what matters for the empty-`to:` check) but
// their contents are not asserted on by this guard.
type netPolPeer struct {
	IPBlock *netPolIPBlock `yaml:"ipBlock"`
}

type netPolPort struct {
	Protocol string `yaml:"protocol"`
	Port     int    `yaml:"port"`
}

type netPolRule struct {
	To    []netPolPeer `yaml:"to"`
	Ports []netPolPort `yaml:"ports"`
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

// TestOperatorEgressIsBounded asserts the S4b operator-egress NetworkPolicy grants
// exactly DNS (53/UDP+TCP) and exchange HTTPS (443/TCP), nothing else, and that the
// 443 rule is scoped to the public internet only. This is the operator's first-ever
// outbound permission (venueproof.Prover's pre-write proof, see venueproof.go); a
// drift toward a wider port set, an unscoped `to:`, or a shrunken except: list would
// let this permission reach further than the one exchange call it exists for.
func TestOperatorEgressIsBounded(t *testing.T) {
	docs := decodeOperatorNetworkPolicies(t)
	pol, ok := findOperatorEgressPolicy(docs)
	if !ok {
		t.Fatal("no NetworkPolicy named operator-egress found in kanz-operator")
	}

	if len(pol.Spec.PolicyTypes) != 1 || pol.Spec.PolicyTypes[0] != "Egress" {
		t.Errorf("operator-egress policyTypes = %v, want exactly [Egress]", pol.Spec.PolicyTypes)
	}

	wantPorts := map[string]bool{"UDP:53": true, "TCP:53": true, "TCP:443": true}
	gotPorts := map[string]bool{}
	var except443 []string
	var found443Block bool

	for _, rule := range pol.Spec.Egress {
		// A rule with no `to:` peers is unrestricted egress on its ports — the
		// one thing this policy exists to forbid.
		if len(rule.To) == 0 {
			t.Errorf("operator-egress has an egress rule with an empty `to:` (ports %v) — unrestricted egress is forbidden", rule.Ports)
		}
		for _, p := range rule.Ports {
			gotPorts[strings.ToUpper(p.Protocol)+":"+strconv.Itoa(p.Port)] = true
		}
		for _, peer := range rule.To {
			if peer.IPBlock == nil || peer.IPBlock.CIDR != "0.0.0.0/0" {
				continue
			}
			found443Block = true
			except443 = peer.IPBlock.Except
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

	if !found443Block {
		t.Fatal("operator-egress has no ipBlock: {cidr: 0.0.0.0/0} rule — the exchange-egress rule is missing or mis-shaped")
	}
	wantExcept := map[string]bool{
		"10.0.0.0/8":     true,
		"172.16.0.0/12":  true,
		"192.168.0.0/16": true,
		"169.254.0.0/16": true,
	}
	gotExcept := map[string]bool{}
	for _, e := range except443 {
		gotExcept[e] = true
	}
	for cidr := range wantExcept {
		if !gotExcept[cidr] {
			t.Errorf("operator-egress 443 rule's ipBlock.except is missing %s — this permission could reach in-cluster services or the cloud metadata endpoint", cidr)
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
