package arch

import (
	"bytes"
	"net"
	"net/url"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"
)

func tokyoGatewayAllows(policies []networkingv1.NetworkPolicy, namespace, app, protocol string, port int) bool {
	for _, p := range policies {
		s, err := metav1.LabelSelectorAsSelector(&p.Spec.PodSelector)
		if err != nil || !s.Matches(labels.Set{"app": "api-gateway"}) {
			continue
		}
		for _, rule := range p.Spec.Egress {
			portOK := false
			for _, candidate := range rule.Ports {
				if candidate.Port != nil && candidate.Port.IntValue() == port && candidate.Protocol != nil && string(*candidate.Protocol) == protocol {
					portOK = true
				}
			}
			if !portOK {
				continue
			}
			for _, peer := range rule.To {
				if peer.IPBlock != nil || peer.PodSelector == nil {
					continue
				}
				if peer.NamespaceSelector == nil {
					if namespace != p.Namespace {
						continue
					}
				} else {
					ns, err := metav1.LabelSelectorAsSelector(peer.NamespaceSelector)
					if err != nil || !ns.Matches(labels.Set{"kubernetes.io/metadata.name": namespace}) {
						continue
					}
				}
				ps, err := metav1.LabelSelectorAsSelector(peer.PodSelector)
				if err == nil && ps.Matches(labels.Set{"app": app, "k8s-app": app}) {
					return true
				}
			}
		}
	}
	return false
}

func TestTokyoGatewayRenderedEgressCoversColdStartAndConfiguredUpstreams(t *testing.T) {
	raw, err := exec.Command("kubectl", "kustomize", "--load-restrictor=LoadRestrictionsNone", filepath.Join(moduleRoot(t), "infra/overlays/testnet-tokyo")).Output()
	if err != nil {
		t.Fatal(err)
	}
	var policies []networkingv1.NetworkPolicy
	deployments := map[string]appsv1.Deployment{}
	for _, doc := range bytes.Split(bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n")), []byte("\n---\n")) {
		var meta metav1.TypeMeta
		if err := yaml.Unmarshal(doc, &meta); err != nil {
			t.Fatal(err)
		}
		switch meta.Kind {
		case "NetworkPolicy":
			var p networkingv1.NetworkPolicy
			if err := yaml.Unmarshal(doc, &p); err != nil {
				t.Fatal(err)
			}
			policies = append(policies, p)
		case "Deployment":
			var d appsv1.Deployment
			if err := yaml.Unmarshal(doc, &d); err != nil {
				t.Fatal(err)
			}
			deployments[d.Name] = d
		}
	}
	for _, protocol := range []string{"UDP", "TCP"} {
		if !tokyoGatewayAllows(policies, "kube-system", "kube-dns", protocol, 53) {
			t.Errorf("gateway cannot resolve DNS over %s", protocol)
		}
	}
	for _, p := range policies {
		s, err := metav1.LabelSelectorAsSelector(&p.Spec.PodSelector)
		if err != nil {
			t.Fatal(err)
		}
		if !s.Matches(labels.Set{"app": "api-gateway"}) {
			continue
		}
		for _, rule := range p.Spec.Egress {
			if len(rule.To) == 0 || len(rule.Ports) == 0 {
				t.Errorf("%s has unrestricted egress", p.Name)
			}
			for _, peer := range rule.To {
				if peer.IPBlock != nil || peer.PodSelector == nil || (len(peer.PodSelector.MatchLabels) == 0 && len(peer.PodSelector.MatchExpressions) == 0) {
					t.Errorf("%s must name destination workloads", p.Name)
				}
			}
		}
	}
	if !tokyoGatewayAllows(policies, "kanz-messaging", "nats", "TCP", 4222) {
		t.Error("gateway cannot connect to NATS")
	}
	if !tokyoGatewayAllows(policies, "kanz-messaging", "redis", "TCP", 6379) {
		t.Error("gateway cannot claim cross-pod idempotency keys")
	}
	for _, c := range deployments["api-gateway"].Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if !strings.HasSuffix(e.Name, "_ADDR") && !strings.HasSuffix(e.Name, "_URI") && e.Name != "API_GATEWAY_NATS_URL" {
				continue
			}
			address := e.Value
			if strings.Contains(address, "://") {
				u, err := url.Parse(address)
				if err != nil {
					t.Fatal(err)
				}
				address = u.Host
			}
			host, portText, err := net.SplitHostPort(address)
			if err != nil {
				t.Fatalf("%s: %v", e.Name, err)
			}
			parts := strings.Split(host, ".")
			port, err := strconv.Atoi(portText)
			if err != nil || len(parts) < 3 || parts[2] != "svc" {
				t.Fatalf("unmodeled destination %s", e.Name)
			}
			if !tokyoGatewayAllows(policies, parts[1], parts[0], "TCP", port) {
				t.Errorf("gateway egress blocks %s", e.Name)
			}
		}
	}
	for _, target := range []struct {
		ns, app string
		port    int
	}{{"kanz-data", "postgres", 5432}, {"kube-system", "unrelated", 53}, {"kanz-messaging", "unrelated", 4222}, {"kanz-services", "identity", 8080}} {
		if tokyoGatewayAllows(policies, target.ns, target.app, "TCP", target.port) {
			t.Errorf("gateway admits unintended destination %+v", target)
		}
	}
	// Reproduce the deployed failure: the identity-only egress policy must fail
	// the cold-start coverage check regardless of its resource name.
	var identityOnly []networkingv1.NetworkPolicy
	for _, p := range policies {
		if p.Name == "allow-gateway-identity-egress" {
			identityOnly = append(identityOnly, p)
		}
	}
	if len(identityOnly) != 1 || tokyoGatewayAllows(identityOnly, "kube-system", "kube-dns", "UDP", 53) || tokyoGatewayAllows(identityOnly, "kanz-messaging", "nats", "TCP", 4222) {
		t.Fatal("regression fixture no longer reproduces isolated gateway startup")
	}

	const release = "b4106263ca95cf0ca6ce0a12a30c43890fcaa29f"
	digests := map[string]string{
		"identity":    "sha256:c0de444c170c675c5728522501f3605d7c4b6d4e2a1c2697c4004f1a68315e50",
		"api-gateway": "sha256:a3df6f8e8d93f3542616f6caa508a9d4d358dfda5f96b5a217f53aaad0fcc633",
	}
	for _, name := range []string{"identity", "api-gateway"} {
		d := deployments[name]
		if len(d.Spec.Template.Spec.Containers) != 1 || d.Spec.Template.Spec.Containers[0].Image != "012619468098.dkr.ecr.ap-northeast-1.amazonaws.com/"+name+"@"+digests[name] {
			t.Errorf("%s differs from the verified coordinated release", name)
		}
		if d.Annotations["kanz.io/release-commit"] != release || d.Spec.Template.Annotations["kanz.io/release-commit"] != release || d.Spec.Template.Labels["kanz.io/release"] != release {
			t.Errorf("%s release provenance is overwritten", name)
		}
	}
	identity := deployments["identity"]
	var env []corev1.EnvVar
	for _, c := range identity.Spec.Template.Spec.Containers {
		if c.Name == "identity" {
			env = c.Env
		}
	}
	admin := false
	for _, e := range env {
		if e.Name == "IDENTITY_OPERATOR_ROLE" {
			t.Error("legacy operator authority survives cutover")
		}
		if e.Name == "IDENTITY_ADMIN_ROLE" && e.Value == "kanz-identity-admin" {
			admin = true
		}
	}
	if !admin {
		t.Error("dedicated administrator authority missing")
	}
	if len(identity.Spec.Template.Spec.InitContainers) != 1 || !strings.HasSuffix(identity.Spec.Template.Spec.InitContainers[0].Image, "@sha256:bb917df3b095e8c1ab2cc81f5091c7f779128c72beedfb0f2047f8daaccdb867") {
		t.Error("identity session migration does not match verified release")
	}
}
