package nodeops

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

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

// opsLogging is ops() with the log captured, so a drain test can tell the loop's
// terminal outcomes apart — cancelled, node gone, complete and deadline reached are
// four different things and the whole point of the cordon re-check is that an
// operator can see which one happened.
func opsLogging(objs ...runtime.Object) (*fake.Clientset, *Ops, *bytes.Buffer) {
	cs := fake.NewSimpleClientset(objs...)
	var log bytes.Buffer
	return cs, New(cs, slog.New(slog.NewTextHandler(&log, nil))), &log
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// evictionCounter counts eviction attempts and answers them with ret, without
// removing the pod — the pod survives, so a loop that keeps running keeps evicting
// and the count is a direct measure of how many passes ran.
func evictionCounter(cs *fake.Clientset, ret error) *int {
	n := 0
	cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		n++
		return true, nil, ret
	})
	return &n
}

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

// The drain loop's passes are drainRetryInterval apart, so the tests below spend
// real seconds waiting. They run in parallel with each other, and each bounds its
// own context so that a loop which fails to stop still ends the test in seconds
// rather than at the 15-minute deadline.

func TestDrainCancelledWhenNodeIsUncordonedMidLoop(t *testing.T) {
	t.Parallel()
	cs, o, log := opsLogging(node("london", true), pod("app-1", "london", ""))
	// The operator uncordons between the first and second pass: the first re-read
	// still sees a cordoned node, every later one sees it back in service.
	reads := 0
	cs.PrependReactor("get", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		reads++
		return true, node("london", reads == 1), nil
	})
	// 429 on every eviction, so the pod survives and a loop that keeps going keeps
	// evicting it — the eviction count is how many passes actually ran.
	evictions := evictionCounter(cs, apierrors.NewTooManyRequests("blocked by PDB", 1))

	// Two passes' worth of headroom: an uncancelled loop evicts a second time at
	// drainRetryInterval and only then hits this deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	o.evictNode(ctx, "london")

	if *evictions != 1 {
		t.Errorf("evicted %d times, want 1 — the loop kept evicting a node the operator had put back in service", *evictions)
	}
	if !strings.Contains(log.String(), "drain cancelled") {
		t.Errorf("an uncordon must be logged as a cancellation, got log: %s", log.String())
	}
	if strings.Contains(log.String(), "drain complete") {
		t.Errorf("a cancelled drain must not report completion, got log: %s", log.String())
	}
}

func TestDrainCompletesWhileNodeStaysCordoned(t *testing.T) {
	t.Parallel()
	cs, o, log := opsLogging(node("london", true), pod("app-1", "london", ""))
	// A real eviction removes the pod, so the second pass finds nothing to do.
	// Written straight to the tracker: re-entering the clientset from inside a
	// reactor would deadlock on the fake's own lock.
	evictions := 0
	cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		evictions++
		ev := a.(k8stesting.CreateAction).GetObject().(metav1.Object)
		if err := cs.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), ev.GetNamespace(), ev.GetName()); err != nil {
			t.Errorf("tracker delete: %v", err)
		}
		return true, nil, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	o.evictNode(ctx, "london")

	if evictions != 1 {
		t.Errorf("evicted %d times, want 1 (the pod was gone after the first pass)", evictions)
	}
	if !strings.Contains(log.String(), "drain complete") {
		t.Errorf("a cordoned node with no pods left must report completion, got log: %s", log.String())
	}
	if strings.Contains(log.String(), "drain cancelled") {
		t.Errorf("the node never became schedulable, so nothing was cancelled, got log: %s", log.String())
	}
}

func TestTransientNodeReadFailureDoesNotCancelDrain(t *testing.T) {
	t.Parallel()
	cs, o, log := opsLogging(node("london", true), pod("app-1", "london", ""))
	// The node read fails once, the way an API server blip fails it. That is not an
	// uncordon and must not stop the drain.
	reads := 0
	cs.PrependReactor("get", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		reads++
		if reads == 1 {
			return true, nil, apierrors.NewServerTimeout(corev1.Resource("nodes"), "get", 1)
		}
		return false, nil, nil
	})
	evictions := evictionCounter(cs, apierrors.NewTooManyRequests("blocked by PDB", 1))

	// Shorter than drainRetryInterval: exactly one pass runs, and it is the pass
	// whose node read failed.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	o.evictNode(ctx, "london")

	if *evictions != 1 {
		t.Errorf("evicted %d times, want 1 — a failed node read aborted a legitimate drain", *evictions)
	}
	if strings.Contains(log.String(), "drain cancelled") {
		t.Errorf("a read error is not an uncordon, got log: %s", log.String())
	}
}

func TestDrainStopsWhenNodeIsDeleted(t *testing.T) {
	t.Parallel()
	// The node is gone but its pod object lingers: without an explicit stop the loop
	// would evict into the void until the deadline.
	cs, o, log := opsLogging(pod("app-1", "london", ""))
	evictions := evictionCounter(cs, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	o.evictNode(ctx, "london")

	if *evictions != 0 {
		t.Errorf("evicted %d times, want 0 — there is no node to drain", *evictions)
	}
	if !strings.Contains(log.String(), "no longer exists") {
		t.Errorf("a deleted node needs its own outcome in the log, got log: %s", log.String())
	}
	if strings.Contains(log.String(), "drain complete") {
		t.Errorf("nothing was drained, so this is not a completion, got log: %s", log.String())
	}
}
