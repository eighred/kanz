// Package nodeops performs node-lifecycle operations on the Kubernetes API for the
// operator control plane: cordon/uncordon (patch spec.unschedulable) and drain
// (cordon + evict ordinary pods via the Eviction API, honoring PodDisruptionBudgets).
// No SSH, no Jobs — plain k8s API calls. Drain NEVER force-deletes a pod.
package nodeops

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/kanz-eng/kanz/services/operator/internal/estate"
)

const (
	drainRetryInterval = 5 * time.Second
	drainDeadline      = 15 * time.Minute
)

// Ops performs node-lifecycle operations.
type Ops struct {
	cs     kubernetes.Interface
	logger *slog.Logger
}

func New(cs kubernetes.Interface, logger *slog.Logger) *Ops {
	return &Ops{cs: cs, logger: logger}
}

func (o *Ops) Cordon(ctx context.Context, name string) error {
	return o.setUnschedulable(ctx, name, true)
}
func (o *Ops) Uncordon(ctx context.Context, name string) error {
	return o.setUnschedulable(ctx, name, false)
}

func (o *Ops) setUnschedulable(ctx context.Context, name string, v bool) error {
	if name == "" {
		return fmt.Errorf("node name is required")
	}
	patch := []byte(fmt.Sprintf(`{"spec":{"unschedulable":%t}}`, v))
	_, err := o.cs.CoreV1().Nodes().Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	return err // apierrors.IsNotFound(err) for an unknown node
}

// Drain cordons the node, then evicts its ordinary pods in the background. It returns
// once the node is cordoned; the operator polls ListNodes for the derived status.
func (o *Ops) Drain(ctx context.Context, name string) error {
	if err := o.Cordon(ctx, name); err != nil {
		return err
	}
	// Background eviction, bound to its own deadline (NOT the request ctx — a drain
	// outlives the RPC). Re-issuing Drain is safe (cordon is idempotent).
	go func() {
		bctx, cancel := context.WithTimeout(context.Background(), drainDeadline)
		defer cancel()
		o.evictNode(bctx, name)
	}()
	return nil
}

// evictNode loops evictOnce until no evictable pods remain or the deadline passes. A
// PDB-blocked pod keeps the node Draining until another replica is ready.
func (o *Ops) evictNode(ctx context.Context, name string) {
	for {
		remaining, err := o.evictOnce(ctx, name)
		if err != nil {
			o.logger.Error("drain pass failed", "node", name, "err", err)
			return
		}
		if remaining == 0 {
			o.logger.Info("drain complete", "node", name)
			return
		}
		select {
		case <-ctx.Done():
			o.logger.Warn("drain deadline reached with pods remaining", "node", name, "remaining", remaining)
			return
		case <-time.After(drainRetryInterval):
		}
	}
}

// evictOnce makes one eviction pass over the node's evictable pods. It returns the
// number that were still evictable this pass (a caller loops until 0). A 429 (PDB) or
// 404 (already gone) is NOT an error — 429 means try again, 404 means done.
func (o *Ops) evictOnce(ctx context.Context, name string) (int, error) {
	pods, err := o.podsOnNode(ctx, name)
	if err != nil {
		return 0, err
	}
	remaining := 0
	for i := range pods {
		p := &pods[i]
		if !estate.IsEvictable(p) {
			continue
		}
		remaining++
		err := o.cs.CoreV1().Pods(p.Namespace).EvictV1(ctx, &policyv1.Eviction{
			ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace},
		})
		switch {
		case err == nil, apierrors.IsNotFound(err), apierrors.IsTooManyRequests(err):
			// evicted / already gone / PDB-blocked (retry next pass) — all fine.
		default:
			o.logger.Error("evict failed", "pod", p.Namespace+"/"+p.Name, "err", err)
		}
	}
	return remaining, nil
}

// podsOnNode lists the pods on a node. It requests a fieldSelector for efficiency on a
// real API server, but ALSO filters by Spec.NodeName in code — the fake clientset
// ignores fieldSelectors, so the in-code filter is what makes tests correct.
func (o *Ops) podsOnNode(ctx context.Context, name string) ([]corev1.Pod, error) {
	list, err := o.cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + name})
	if err != nil {
		return nil, err
	}
	var out []corev1.Pod
	for _, p := range list.Items {
		if p.Spec.NodeName == name {
			out = append(out, p)
		}
	}
	return out, nil
}
