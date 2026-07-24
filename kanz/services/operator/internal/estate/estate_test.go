package estate

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func node(name string, ready corev1.ConditionStatus, region string, labels map[string]string, created time.Time) *corev1.Node {
	if labels == nil {
		labels = map[string]string{}
	}
	if region != "" {
		labels["topology.kubernetes.io/region"] = region
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, CreationTimestamp: metav1.NewTime(created)},
		Status: corev1.NodeStatus{
			NodeInfo:   corev1.NodeSystemInfo{KubeletVersion: "v1.31.3"},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: ready}},
		},
	}
}

func TestListNodesMapsStatusRolesRegion(t *testing.T) {
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	cs := fake.NewSimpleClientset(
		node("london", corev1.ConditionTrue, "europe", map[string]string{"node-role.kubernetes.io/control-plane": ""}, created),
		node("tokyo", corev1.ConditionFalse, "asia", nil, created),
	)
	got, err := NewK8s(cs).ListNodes(context.Background())
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 nodes, got %d", len(got))
	}
	byName := map[string]Node{}
	for _, n := range got {
		byName[n.Name] = n
	}
	if byName["london"].Status != StatusReady {
		t.Errorf("london status = %v, want StatusReady", byName["london"].Status)
	}
	if byName["london"].Region != "europe" {
		t.Errorf("london region = %q, want europe", byName["london"].Region)
	}
	if len(byName["london"].Roles) != 1 || byName["london"].Roles[0] != "control-plane" {
		t.Errorf("london roles = %v, want [control-plane]", byName["london"].Roles)
	}
	if byName["london"].KubeletVersion != "v1.31.3" {
		t.Errorf("london version = %q, want v1.31.3", byName["london"].KubeletVersion)
	}
	if byName["tokyo"].Status != StatusNotReady {
		t.Errorf("tokyo status = %v, want StatusNotReady", byName["tokyo"].Status)
	}
}

func TestListNodesUnknownReadyIsDenyByDefault(t *testing.T) {
	// A node with no NodeReady condition must map to StatusUnknown, never ready.
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "ghost"}}
	got, err := NewK8s(fake.NewSimpleClientset(n)).ListNodes(context.Background())
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(got) != 1 || got[0].Status != StatusUnknown {
		t.Fatalf("want 1 node StatusUnknown, got %+v", got)
	}
}

func TestListClustersGroupsByRegionWithCounts(t *testing.T) {
	created := time.Now()
	cs := fake.NewSimpleClientset(
		node("a", corev1.ConditionTrue, "usa", nil, created),
		node("b", corev1.ConditionTrue, "usa", nil, created),
		node("c", corev1.ConditionFalse, "usa", nil, created),
		node("d", corev1.ConditionTrue, "asia", nil, created),
	)
	got, err := NewK8s(cs).ListClusters(context.Background())
	if err != nil {
		t.Fatalf("ListClusters: %v", err)
	}
	byRegion := map[string]Cluster{}
	for _, c := range got {
		byRegion[c.Region] = c
	}
	if byRegion["usa"].Online != 2 || byRegion["usa"].Offline != 1 {
		t.Errorf("usa = %+v, want online 2 offline 1", byRegion["usa"])
	}
	if byRegion["asia"].Online != 1 || byRegion["asia"].Offline != 0 {
		t.Errorf("asia = %+v, want online 1 offline 0", byRegion["asia"])
	}
}

func TestListNodesEmptyEstateIsNotAnError(t *testing.T) {
	got, err := NewK8s(fake.NewSimpleClientset()).ListNodes(context.Background())
	if err != nil {
		t.Fatalf("empty estate should not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 nodes, got %d", len(got))
	}
}

func pod(name, node string, owner string, mirror bool, terminating bool) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       corev1.PodSpec{NodeName: node},
	}
	if owner != "" {
		p.OwnerReferences = []metav1.OwnerReference{{Kind: owner, Name: "x"}}
	}
	if mirror {
		p.Annotations = map[string]string{"kubernetes.io/config.mirror": "abc"}
	}
	if terminating {
		now := metav1.Now()
		p.DeletionTimestamp = &now
	}
	return p
}

func TestListNodesSurfacesSchedulableAndEvictableCount(t *testing.T) {
	created := time.Now()
	london := node("london", corev1.ConditionTrue, "europe", nil, created)
	london.Spec.Unschedulable = true // cordoned
	cs := fake.NewSimpleClientset(
		london,
		pod("app-1", "london", "", false, false), // ordinary → evictable
		pod("ds-1", "london", "DaemonSet", false, false), // DaemonSet → not
		pod("mirror-1", "london", "", true, false),       // mirror → not
		pod("term-1", "london", "", false, true),         // terminating → not
		pod("elsewhere", "tokyo", "", false, false),      // other node
	)
	got, err := NewK8s(cs).ListNodes(context.Background())
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	var ln Node
	for _, n := range got {
		if n.Name == "london" {
			ln = n
		}
	}
	if ln.Schedulable {
		t.Errorf("london is cordoned → Schedulable should be false")
	}
	if ln.EvictablePods != 1 {
		t.Errorf("london evictable pods = %d, want 1 (only app-1)", ln.EvictablePods)
	}
}
