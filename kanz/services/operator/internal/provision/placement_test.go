package provision

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// WHAT THESE TESTS PROVE AND WHAT THEY CANNOT (#207).
//
// PROVEN: given a node inventory, CheckPlacement reaches the right verdict and says why
// in terms an operator can act on. The inventory comes from client-go's fake clientset,
// so the DECISION is exercised end to end against a double.
//
// NOT PROVEN: that a real cordoned k3s server produces the inventory these tests hand
// it. Nothing in this environment has a cluster. The two facts the decision rests on are
// narrow and stated here so the gap is inspectable rather than implied: `kubectl drain`
// and this operator's own nodeops.Drain both cordon first (Node.Spec.Unschedulable =
// true), and a nodeSelector is an exact match on Node.Labels. Both are Kubernetes API
// semantics, not cluster behaviour.

// cpNode builds a control-plane node that would run the provisioning Job, then applies
// the mutations a test wants. Built from controlPlaneOnly itself so a change to the pin
// cannot leave these tests asserting against a label the Job no longer uses.
func cpNode(name string, muts ...func(*corev1.Node)) *corev1.Node {
	labels := map[string]string{}
	for k, v := range controlPlaneOnly {
		labels[k] = v
	}
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
		}},
	}
	for _, m := range muts {
		m(n)
	}
	return n
}

func cordoned(n *corev1.Node) { n.Spec.Unschedulable = true }

func notReady(n *corev1.Node) {
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
}

func worker(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"kubernetes.io/os": "linux"}},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
		}},
	}
}

func checkPlacement(t *testing.T, objs ...runtime.Object) PlacementVerdict {
	t.Helper()
	v, err := New(fake.NewSimpleClientset(objs...), cfg()).CheckPlacement(context.Background())
	if err != nil {
		t.Fatalf("CheckPlacement: %v", err)
	}
	return v
}

// A HEALTHY ESTATE MUST STILL PROVISION. The refusal is only worth having if it does not
// also refuse the normal case — a check that says no to everything would "fix" #207 by
// breaking Add Node outright.
func TestPlacementAcceptsAReadyControlPlane(t *testing.T) {
	v := checkPlacement(t, cpNode("k3s-server"), worker("worker-1"))
	if !v.Schedulable() {
		t.Fatalf("a Ready, uncordoned control-plane node was judged unschedulable: %+v\n%s", v, v.Reason())
	}
	if v.Drained() {
		t.Errorf("Drained() is true for an uncordoned estate: %+v", v)
	}
	if v.Reason() != "" {
		t.Errorf("a schedulable verdict carries a reason %q; a caller rendering that would refuse "+
			"a request that is fine", v.Reason())
	}
}

// THE ISSUE'S OWN CASE. One control-plane node, drained for maintenance: the provisioning
// Job has nowhere to run, so the request must be refused with the drain named rather than
// accepted into a Job that sits Pending for its whole deadline holding K3S_TOKEN.
func TestPlacementRefusesWhenTheOnlyControlPlaneNodeIsCordoned(t *testing.T) {
	v := checkPlacement(t, cpNode("k3s-server", cordoned), worker("worker-1"))

	if v.Schedulable() {
		t.Fatalf("a cordoned single control plane was judged schedulable: %+v", v)
	}
	if !v.Drained() {
		t.Fatalf("Drained() is false for a cordoned control plane: %+v. The gRPC boundary keys "+
			"the drain's own refusal code off this, so an operator would be told the estate is "+
			"mislabelled instead of that they drained it", v)
	}
	// The WORKER must not rescue the verdict. It is Ready and schedulable, and it is
	// exactly the node the pin exists to keep this Job off.
	if len(v.Eligible) != 0 {
		t.Errorf("eligible = %v; the worker is not a candidate — the Job's nodeSelector admits "+
			"only the control plane, and that pin is what keeps the fleet-admission token off a "+
			"member of the fleet", v.Eligible)
	}

	reason := v.Reason()
	for _, want := range []string{"drained", "k3s-server", "cordoned", "uncordon"} {
		if !strings.Contains(strings.ToLower(reason), strings.ToLower(want)) {
			t.Errorf("the refusal does not contain %q — an operator gets a refusal that does not "+
				"name the drain, the node, or the repair, and goes looking for a provisioning "+
				"bug.\nreason: %s", want, reason)
		}
	}
}

// A DRAIN AND A MISLABELLED ESTATE ARE DIFFERENT MORNINGS. Nothing carries the pin at
// all: uncordoning repairs nothing, and the refusal must not send anyone to do it.
func TestPlacementDistinguishesNoControlPlaneFromADrain(t *testing.T) {
	v := checkPlacement(t, worker("worker-1"), worker("worker-2"))
	if v.Schedulable() {
		t.Fatalf("no control-plane node, yet judged schedulable: %+v", v)
	}
	if v.Drained() {
		t.Fatalf("Drained() is true with nothing cordoned: %+v", v)
	}
	reason := v.Reason()
	if strings.Contains(strings.ToLower(reason), "drain") && !strings.Contains(reason, "not a drain") {
		t.Errorf("the refusal blames a drain that did not happen: %s", reason)
	}
	if !strings.Contains(reason, controlPlaneRoleLabel) {
		t.Errorf("the refusal does not name %s, the label that is missing: %s", controlPlaneRoleLabel, reason)
	}
}

// THE EXACT-MATCH TRAP, REPORTED AS ITSELF. k3s writes control-plane=true and kubeadm/kind
// write an empty value; a nodeSelector is an exact string match, so the same estate on a
// different distribution has a control-plane node that the pin cannot see. Told only "no
// node carries the label", an operator looking straight at a control-plane node would not
// believe the message.
func TestPlacementNamesALabelValueMismatch(t *testing.T) {
	mislabelled := cpNode("kubeadm-cp", func(n *corev1.Node) {
		n.Labels[controlPlaneRoleLabel] = "" // kubeadm's spelling
	})
	v := checkPlacement(t, mislabelled)
	if v.Schedulable() {
		t.Fatalf("a node whose label value does not match the selector was judged eligible: %+v", v)
	}
	if len(v.Mismatched) != 1 || !strings.Contains(v.Mismatched[0], "kubeadm-cp") {
		t.Fatalf("mismatched = %v, want the node named", v.Mismatched)
	}
	if !strings.Contains(v.Reason(), "kubeadm-cp") {
		t.Errorf("the refusal does not name the node that ALMOST matches, so the reader is told "+
			"there is no control-plane node while looking at one: %s", v.Reason())
	}
}

// A KUBELET THAT FELL OVER IS NOT A DRAIN EITHER, and uncordoning is not the repair.
func TestPlacementDistinguishesNotReadyFromADrain(t *testing.T) {
	v := checkPlacement(t, cpNode("k3s-server", notReady))
	if v.Schedulable() {
		t.Fatalf("a NotReady control plane was judged schedulable: %+v", v)
	}
	if v.Drained() {
		t.Fatalf("Drained() is true for a NotReady, uncordoned node: %+v — uncordoning it would "+
			"do nothing and the operator would have been sent to do it", v)
	}
	if !strings.Contains(v.Reason(), "k3s-server") || !strings.Contains(v.Reason(), "Ready") {
		t.Errorf("the refusal does not name the node and its readiness: %s", v.Reason())
	}
}

// AN UNTOLERATED TAINT IS THE LATENT VERSION OF THE SAME OUTAGE. The Job tolerates only
// the control-plane taint; hardening the node with any other NoSchedule taint makes
// provisioning unschedulable, and that must be legible rather than another Pending Job.
func TestPlacementRefusesAnUntoleratedTaint(t *testing.T) {
	v := checkPlacement(t, cpNode("k3s-server", func(n *corev1.Node) {
		n.Spec.Taints = []corev1.Taint{{Key: "kanz.io/reserved", Value: "market-data", Effect: corev1.TaintEffectNoSchedule}}
	}))
	if v.Schedulable() {
		t.Fatalf("a node carrying an untolerated NoSchedule taint was judged eligible: %+v", v)
	}
	if !strings.Contains(v.Reason(), "kanz.io/reserved") {
		t.Errorf("the refusal does not name the taint that blocks it: %s", v.Reason())
	}
}

// THE TOLERATION THE JOB ACTUALLY CARRIES MUST STILL PLACE IT. tolerateControlPlane
// exists so that tainting the control plane NoSchedule — kubeadm's default posture —
// does not silently turn the pin into an unschedulable Job. If this check ignored
// tolerations it would refuse every provision on such a cluster, inventing the outage it
// was written to report.
func TestPlacementHonoursTheJobsOwnToleration(t *testing.T) {
	v := checkPlacement(t, cpNode("k3s-server", func(n *corev1.Node) {
		n.Spec.Taints = []corev1.Taint{{Key: controlPlaneRoleLabel, Effect: corev1.TaintEffectNoSchedule}}
	}))
	if !v.Schedulable() {
		t.Fatalf("the control-plane taint the Job tolerates was treated as blocking: %+v\n%s", v, v.Reason())
	}
}

// CANNOT-TELL IS NOT FINE. A verdict that cannot be computed must reach the caller as an
// error, never as an empty (and therefore unschedulable) verdict with nil error, and
// never as an optimistic yes.
func TestPlacementReportsAnUnreadableInventoryAsAnError(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("nodes is forbidden: RBAC")
	})
	v, err := New(cs, cfg()).CheckPlacement(context.Background())
	if err == nil {
		t.Fatalf("a failed node list returned no error (verdict %+v). The caller cannot tell "+
			"'checked, and nothing can run it' from 'could not check', which is the pair "+
			"CLAUDE.md forbids collapsing", v)
	}
	if !strings.Contains(err.Error(), "RBAC") {
		t.Errorf("the cause was discarded: %v", err)
	}
}

// The zero value is the one a caller gets from a double, a decode, or a struct nobody
// filled in. It must refuse.
func TestZeroPlacementVerdictRefuses(t *testing.T) {
	var v PlacementVerdict
	if v.Schedulable() {
		t.Fatal("the zero PlacementVerdict is schedulable — a verdict nobody computed reads as fine")
	}
	if v.Reason() == "" {
		t.Error("the zero PlacementVerdict gives no reason, so a refusal built from it would be silent")
	}
}
