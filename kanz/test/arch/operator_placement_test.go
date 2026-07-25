package arch

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// controlPlaneRoleLabel is the node label that marks a control-plane node. Only the
// KEY is pinned here, deliberately: k3s sets it to "true" and kubeadm/kind set it to
// "", and this guard is about WHICH NODES the operator may land on, not about which
// distribution the estate runs. The manifest itself carries the value and the note
// explaining that a nodeSelector is an exact string match.
const controlPlaneRoleLabel = "node-role.kubernetes.io/control-plane"

// placementDoc captures only the pod-placement fields this guard inspects from the
// multi-document operator manifest, in the same yaml.v3 multi-doc decode style as
// operator_rbac_test.go and node_exporter_test.go.
type placementDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				NodeSelector map[string]string `yaml:"nodeSelector"`
				Tolerations  []struct {
					Key      string `yaml:"key"`
					Operator string `yaml:"operator"`
					Effect   string `yaml:"effect"`
				} `yaml:"tolerations"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

// TestOperatorDeploymentIsPinnedToControlPlane asserts the operator Deployment can
// only be scheduled onto a control-plane node.
//
// THE FAILURE THIS PREVENTS IS A DEADLOCK, NOT AN AESTHETIC. The operator is the
// component that performs node drains. Unpinned, the scheduler may place it on a
// worker; a drain of that worker must then evict the operator pod to complete, and
// cannot — replicas: 1 plus the `operator` PodDisruptionBudget's minAvailable: 1
// leaves zero allowed disruptions, so the Eviction API refuses for the whole
// 15-minute drainDeadline and the node never drains. Observed on the two-node k3s
// estate with the operator on worker ip-172-26-12-47.
//
// The answer is placement, never a weaker PDB: refusing to evict the only replica of
// a control-plane component is exactly what that budget is for.
func TestOperatorDeploymentIsPinnedToControlPlane(t *testing.T) {
	var found bool
	for _, d := range decodeOperatorPlacement(t) {
		if d.Kind != "Deployment" || d.Metadata.Name != "operator" {
			continue
		}
		found = true
		podSpec := d.Spec.Template.Spec

		// --- the nodeSelector: the pin itself ---
		if len(podSpec.NodeSelector) == 0 {
			t.Fatalf("the operator Deployment has NO nodeSelector, so Kubernetes may schedule "+
				"the drainer onto a worker. A drain of that worker then blocks forever on "+
				"evicting this pod (replicas: 1 + PDB minAvailable: 1 = 0 allowed disruptions). "+
				"Pin it back with nodeSelector: {%s: \"true\"} — do not weaken the PDB instead.",
				controlPlaneRoleLabel)
		}
		if _, ok := podSpec.NodeSelector[controlPlaneRoleLabel]; !ok {
			t.Errorf("the operator Deployment's nodeSelector is %v, which does not name %q. "+
				"Pinning it to some other label does not keep the drainer off the fleet it "+
				"drains; only the control-plane role label does.",
				podSpec.NodeSelector, controlPlaneRoleLabel)
		}
		for k := range podSpec.NodeSelector {
			if k != controlPlaneRoleLabel {
				t.Errorf("the operator Deployment's nodeSelector carries extra key %q. Every "+
					"nodeSelector key must match for the pod to schedule, so an additional "+
					"constraint can only narrow placement further — and narrowing it past the "+
					"one control-plane node leaves the operator Pending.", k)
			}
		}

		// --- the toleration: not needed on this cluster, required for the next one ---
		//
		// The k3s control-plane node carries no taints today, so the selector alone
		// places the pod. On a cluster whose control plane IS tainted NoSchedule
		// (kubeadm's default) the selector says "only here" and the taint says "not
		// here", and the pin becomes an unschedulable pod. Losing the toleration is
		// therefore a latent outage, invisible until someone hardens the cluster.
		var tolerated bool
		for _, tol := range podSpec.Tolerations {
			if tol.Key != controlPlaneRoleLabel {
				continue
			}
			if tol.Effect == "NoSchedule" || tol.Effect == "" {
				tolerated = true
			}
		}
		if !tolerated {
			t.Errorf("the operator Deployment is pinned to %s but does not tolerate "+
				"%s:NoSchedule. It schedules on THIS cluster only because the control-plane "+
				"node happens to be untainted; on a cluster that taints its control plane the "+
				"pin above turns into a permanently Pending pod.",
				controlPlaneRoleLabel, controlPlaneRoleLabel)
		}
	}

	// Non-vacuity: a guard that matches no document passes forever after the
	// Deployment is renamed or the manifest split.
	if !found {
		t.Fatalf("no Deployment named operator found in infra/deploy/operator-deploy.yaml")
	}
}

// decodeOperatorPlacement reads infra/deploy/operator-deploy.yaml into typed docs.
// Separate from operator_rbac_test.go's decodeOperatorManifest because that one
// projects the RBAC fields and this one projects the pod-placement fields; widening
// a shared struct would couple two unrelated guards.
func decodeOperatorPlacement(t *testing.T) []placementDoc {
	t.Helper()
	path := filepath.Join(moduleRoot(t), "infra", "deploy", "operator-deploy.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var docs []placementDoc
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	for {
		var d placementDoc
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		docs = append(docs, d)
	}
	if len(docs) == 0 {
		t.Fatalf("no docs decoded from %s", path)
	}
	return docs
}
