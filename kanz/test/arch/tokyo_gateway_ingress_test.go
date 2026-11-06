package arch

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

// A repository-wide policy inventory cannot prove an overlay imports a control.
func tokyoRenderedIngress(t *testing.T) ([]networkingv1.NetworkPolicy, map[string]appsv1.Deployment) {
	t.Helper()
	raw, err := exec.Command("kubectl", "kustomize", "--load-restrictor=LoadRestrictionsNone", filepath.Join(moduleRoot(t), "infra/overlays/testnet-tokyo")).CombinedOutput()
	if err != nil {
		t.Fatalf("render Tokyo: %v: %s", err, raw)
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
	return policies, deployments
}

func TestTokyoRenderedGatewayOnlyIngress(t *testing.T) {
	policies, deployments := tokyoRenderedIngress(t)
	for _, target := range []struct {
		app      string
		api, ops int32
	}{
		{"accounting", 8101, 8080}, {"audit", 8102, 8083}, {"compliance", 8095, 8091},
	} {
		t.Run(target.app, func(t *testing.T) {
			d, ok := deployments[target.app]
			if !ok || d.Namespace != "kanz-services" || d.Spec.Template.Labels["app"] != target.app {
				t.Fatal("missing deployed target identity")
			}
			for _, problem := range splitAPIIngressProblems(policies, d.Spec.Template.Labels, target.app, target.api, target.ops) {
				t.Error(problem)
			}
			// Mutations retain every other rendered policy to test the additive union.
			for name, mutate := range map[string]func(*networkingv1.NetworkPolicy){
				"missing target":      func(p *networkingv1.NetworkPolicy) { p.Spec.PodSelector.MatchLabels["app"] = "absent" },
				"wrong namespace":     func(p *networkingv1.NetworkPolicy) { p.Namespace = "other" },
				"portless monitoring": func(p *networkingv1.NetworkPolicy) { p.Spec.Ingress[1].Ports = nil },
				"fromless API":        func(p *networkingv1.NetworkPolicy) { p.Spec.Ingress[0].From = nil },
				"cross namespace gateway": func(p *networkingv1.NetworkPolicy) {
					p.Spec.Ingress[0].From[0].NamespaceSelector = &metav1.LabelSelector{}
				},
				"metrics reaches API": func(p *networkingv1.NetworkPolicy) {
					p.Spec.Ingress[1].Ports[0].Port = &intstr.IntOrString{IntVal: target.api}
				},
			} {
				t.Run(name, func(t *testing.T) {
					mutated := make([]networkingv1.NetworkPolicy, len(policies))
					found := false
					for i, p := range policies {
						mutated[i] = *p.DeepCopy()
						if p.Spec.PodSelector.MatchLabels["app"] == target.app && len(p.Spec.Ingress) == 2 {
							mutate(&mutated[i])
							found = true
						}
					}
					if !found {
						t.Fatal("target policy missing: mutation did not run")
					}
					if got := splitAPIIngressProblems(mutated, d.Spec.Template.Labels, target.app, target.api, target.ops); len(got) == 0 {
						t.Fatal("mutation escaped rendered-union guard")
					}
				})
			}
			t.Run("additive wildcard", func(t *testing.T) {
				wide := networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "wildcard", Namespace: "kanz-services"}, Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}, Ingress: []networkingv1.NetworkPolicyIngressRule{{}}}}
				if got := splitAPIIngressProblems(append(policies, wide), d.Spec.Template.Labels, target.app, target.api, target.ops); len(got) == 0 {
					t.Fatal("additive wildcard escaped")
				}
			})
		})
	}
}

// Tokyo's gateway port stays reachable exclusively through the existing BFF.
// Importing the global ingress-nginx policy would bypass that edge boundary.
func TestTokyoRenderedGatewayIngressRemainsBFFOnly(t *testing.T) {
	policies, deployments := tokyoRenderedIngress(t)
	d, ok := deployments["api-gateway"]
	if !ok || d.Namespace != "kanz-services" {
		t.Fatal("missing gateway deployment")
	}
	for _, problem := range restrictedIngressProblems(policies, d.Spec.Template.Labels, "api-gateway", "web-bff", 8080, 0) {
		t.Error(problem)
	}
	wide := networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "global-ingress", Namespace: "kanz-services"}, Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "api-gateway"}}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}, Ingress: []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "ingress-nginx"}}}}, Ports: []networkingv1.NetworkPolicyPort{{Port: &intstr.IntOrString{IntVal: 8080}}}}}}}
	if got := restrictedIngressProblems(append(policies, wide), d.Spec.Template.Labels, "api-gateway", "web-bff", 8080, 0); len(got) == 0 {
		t.Fatal("global ingress bypass escaped")
	}
}
