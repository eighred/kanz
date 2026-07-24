package nodeops

import (
	"context"
	"log/slog"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/kanz-eng/kanz/services/operator/internal/estate"
)

func node(name string, unsched bool) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.NodeSpec{Unschedulable: unsched}}
}
func pod(name, nodeName, owner string) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: corev1.PodSpec{NodeName: nodeName}}
	if owner != "" {
		p.OwnerReferences = []metav1.OwnerReference{{Kind: owner, Name: "x"}}
	}
	return p
}
func ops(objs ...runtime.Object) (*fake.Clientset, *Ops) {
	cs := fake.NewSimpleClientset(objs...)
	return cs, New(cs, slog.New(slog.NewTextHandler(&nopWriter{}, nil)))
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestCordonFlipsUnschedulable(t *testing.T) {
	cs, o := ops(node("london", false))
	if err := o.Cordon(context.Background(), "london"); err != nil {
		t.Fatalf("Cordon: %v", err)
	}
	n, _ := cs.CoreV1().Nodes().Get(context.Background(), "london", metav1.GetOptions{})
	if !n.Spec.Unschedulable {
		t.Errorf("node should be unschedulable after Cordon")
	}
	if err := o.Uncordon(context.Background(), "london"); err != nil {
		t.Fatalf("Uncordon: %v", err)
	}
	n, _ = cs.CoreV1().Nodes().Get(context.Background(), "london", metav1.GetOptions{})
	if n.Spec.Unschedulable {
		t.Errorf("node should be schedulable after Uncordon")
	}
}

func TestCordonUnknownNodeNotFound(t *testing.T) {
	_, o := ops()
	if err := o.Cordon(context.Background(), "ghost"); !apierrors.IsNotFound(err) {
		t.Fatalf("want NotFound, got %v", err)
	}
}

func TestEvictOnceEvictsOnlyOrdinaryPods(t *testing.T) {
	cs, o := ops(
		node("london", true),
		pod("app-1", "london", ""),
		pod("ds-1", "london", "DaemonSet"),
		pod("elsewhere", "tokyo", ""),
	)
	var evicted []string
	cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		ca := a.(k8stesting.CreateAction)
		ev := ca.GetObject().(metav1.Object)
		evicted = append(evicted, ev.GetName())
		return true, nil, nil // success
	})

	remaining, err := o.evictOnce(context.Background(), "london")
	if err != nil {
		t.Fatalf("evictOnce: %v", err)
	}
	if len(evicted) != 1 || evicted[0] != "app-1" {
		t.Errorf("evicted = %v, want [app-1] only (DaemonSet + other-node skipped)", evicted)
	}
	if remaining != 1 {
		t.Errorf("remaining = %d, want 1 (app-1 was evictable this pass)", remaining)
	}
}

func TestSetRegionPatchesLabel(t *testing.T) {
	cs, o := ops(node("london", false))
	if err := o.SetRegion(context.Background(), "london", "asia"); err != nil {
		t.Fatalf("SetRegion: %v", err)
	}
	n, _ := cs.CoreV1().Nodes().Get(context.Background(), "london", metav1.GetOptions{})
	if n.Labels[estate.RegionLabel] != "asia" {
		t.Errorf("region label = %q, want asia", n.Labels[estate.RegionLabel])
	}
}

func TestSetRegionRejectsEmpty(t *testing.T) {
	_, o := ops(node("london", false))
	if err := o.SetRegion(context.Background(), "london", ""); err == nil {
		t.Errorf("empty region should error")
	}
	if err := o.SetRegion(context.Background(), "", "asia"); err == nil {
		t.Errorf("empty name should error")
	}
}

func TestSetRegionUnknownNodeNotFound(t *testing.T) {
	_, o := ops()
	if err := o.SetRegion(context.Background(), "ghost", "asia"); !apierrors.IsNotFound(err) {
		t.Fatalf("want NotFound, got %v", err)
	}
}

func TestEvictOncePDBBlockedDoesNotForceDelete(t *testing.T) {
	cs, o := ops(node("london", true), pod("app-1", "london", ""))
	var deleted bool
	cs.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		deleted = true
		return true, nil, nil
	})
	cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		return true, nil, apierrors.NewTooManyRequests("blocked by PDB", 1) // 429
	})
	if _, err := o.evictOnce(context.Background(), "london"); err != nil {
		t.Fatalf("a PDB-429 must not fail evictOnce (it retries next pass): %v", err)
	}
	if deleted {
		t.Errorf("drain must NEVER force-delete a pod — eviction only")
	}
}
