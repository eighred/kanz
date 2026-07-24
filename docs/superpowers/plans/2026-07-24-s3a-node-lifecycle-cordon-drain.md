# S3a — Node Lifecycle (Maintenance cordon/uncordon + Drain) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the operator take a node out of service safely from the Universe TUI — cordon/uncordon (Maintenance) and Drain (cordon + PDB-respecting eviction) — with drain status derived from live node/pod state.

**Architecture:** Three new `operator.v1` RPCs (`Cordon`/`Uncordon`/`Drain`) that the operator service performs as plain Kubernetes API calls (node patch + the Eviction API) — no SSH, no Jobs. Drain runs async in a background goroutine and is PDB-safe (backs off on the Eviction API's 429). The node read model gains `schedulable` + `evictable_pods`, from which the TUI derives Schedulable/Cordoned/Draining/Drained. A new `operator-node-writer` ClusterRole carries the writes; the read-only reader role stays read-only (it just gains `pods: list`).

**Tech Stack:** Go, `operator.v1` (buf), `k8s.io/client-go` (node Patch, `PolicyV1`/`EvictV1`), `github.com/charmbracelet/bubbletea`.

## Global Constraints

Every task's requirements implicitly include this section.

- **Go toolchain** `go 1.26.1` / `toolchain go1.26.5`; all `go` commands from `kanz/` with `GOFLAGS=-mod=mod`. **No `make`** — use `buf generate`; set **`GOTMPDIR="$(pwd)/.gotmp"`** for every `go test`.
- Generated protobuf imports from `github.com/kanz-eng/kanz-schemas-go/operator/v1`; regenerate with `cd kanz-schemas && buf generate`.
- **No `crypto/ssh` in the operator** — these are k8s API calls. `TestNoSSHPlane` MUST stay green.
- **Drain is PDB-safe: eviction only, never force-delete.** Use the Eviction API (`EvictV1`); on `429 TooManyRequests` (PDB would be breached) back off and retry — never `Delete` a pod to force it.
- **Drain status is DERIVED from live state, never stored.** No drain record anywhere.
- **RBAC:** the read-only `operator-node-reader` ClusterRole gains `pods: list` (still read-only) — its guard is updated to allow `{nodes, pods}` read but still reject any write verb. Node writes live in a NEW `operator-node-writer` ClusterRole (`nodes: patch` + `pods/eviction: create` — NO node create/delete, NO pod delete).
- **`render()` stays pure.** `govulncheck ./...` stays 0-reachable. Namespace `kanz-operator`.
- **Fake-clientset caveats (tests):** the fake clientset IGNORES `FieldSelector` (so also filter pods by `Spec.NodeName` in code), and eviction surfaces as a `create` action on the `pods`/`eviction` subresource (capture it with a reactor).

---

### Task 1: `operator.v1` Cordon/Uncordon/Drain + Node status fields

**Files:**
- Modify: `kanz-schemas/proto/operator/v1/operator.proto`
- Regenerates (not committed): `operator.pb.go`, `operator_grpc.pb.go`

**Interfaces:**
- Produces: `Cordon`/`Uncordon`/`Drain` RPCs; `CordonRequest/Response`, `UncordonRequest/Response`, `DrainRequest/Response` (request = `{name}`, response empty); `Node.schedulable` (bool, field 7), `Node.evictable_pods` (int32, field 8).

- [ ] **Step 1: Add the RPCs, request/response messages, and Node fields**

In the `service OperatorService { ... }` block add:

```proto
  // Cordon marks a node unschedulable (Maintenance Mode). Reversible via Uncordon.
  rpc Cordon(CordonRequest) returns (CordonResponse);
  // Uncordon marks a node schedulable again.
  rpc Uncordon(UncordonRequest) returns (UncordonResponse);
  // Drain cordons a node and evicts its (non-DaemonSet, non-mirror) pods via the
  // Eviction API, honoring PodDisruptionBudgets. Async: returns once cordoned;
  // eviction proceeds in the background. Poll ListNodes for the derived status.
  rpc Drain(DrainRequest) returns (DrainResponse);
```

Add the messages (each request is a node name; responses are empty acks):

```proto
message CordonRequest { string name = 1; }
message CordonResponse {}
message UncordonRequest { string name = 1; }
message UncordonResponse {}
message DrainRequest { string name = 1; }
message DrainResponse {}
```

In the existing `Node` message, add two fields AFTER the current highest field number (F0 uses 1–6):

```proto
  // schedulable is false when the node is cordoned (spec.unschedulable).
  bool schedulable = 7;
  // evictable_pods is the count of ordinary (non-DaemonSet, non-mirror,
  // non-terminating) pods still on the node — the drain-progress signal.
  int32 evictable_pods = 8;
```

- [ ] **Step 2: Lint + generate**

Run: `cd kanz-schemas && buf lint && buf generate`
Expected: lint exit 0; the new RPCs/messages + `Node.GetSchedulable()`/`GetEvictablePods()` appear in the generated code.

- [ ] **Step 3: Verify types compile**

Run: `cd kanz && GOFLAGS=-mod=mod go build github.com/kanz-eng/kanz-schemas-go/operator/v1`
Expected: exit 0.

- [ ] **Step 4: Commit**

```bash
cd kanz-schemas && git add proto/operator/v1/operator.proto
git commit -m "feat(schemas): operator.v1 Cordon/Uncordon/Drain + Node schedulable/evictable_pods

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: Estate read model — schedulable + evictable pods

**Files:**
- Modify: `kanz/services/operator/internal/estate/estate.go`
- Test: `kanz/services/operator/internal/estate/estate_test.go`

**Interfaces:**
- Produces: `Node.Schedulable bool`, `Node.EvictablePods int`; `func IsEvictable(p *corev1.Pod) bool` (exported — reused by nodeops in Task 3).

- [ ] **Step 1: Write the failing test**

Add to `estate_test.go`:

```go
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
		pod("app-1", "london", "", false, false),       // ordinary → evictable
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
```

Add `corev1 "k8s.io/api/core/v1"` to the test imports if not present.

- [ ] **Step 2: Run — verify it fails**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./services/operator/internal/estate/... -run TestListNodesSurfaces`
Expected: FAIL — `Node` has no `Schedulable`/`EvictablePods`.

- [ ] **Step 3: Implement**

In `estate.go`, add the two fields to `Node`:

```go
	Schedulable    bool
	EvictablePods  int
```

Add the exported classifier:

```go
// IsEvictable reports whether a drain would evict this pod: not DaemonSet-owned,
// not a mirror/static pod, and not already terminating. (Shared with nodeops.)
func IsEvictable(p *corev1.Pod) bool {
	if p.DeletionTimestamp != nil {
		return false
	}
	if _, mirror := p.Annotations["kubernetes.io/config.mirror"]; mirror {
		return false
	}
	for _, or := range p.OwnerReferences {
		if or.Kind == "DaemonSet" {
			return false
		}
	}
	return true
}
```

Change `ListNodes` to also list pods (one call) and count evictable per node. Replace the node-listing body:

```go
func (k *K8s) ListNodes(ctx context.Context) ([]Node, error) {
	list, err := k.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	// Count evictable pods per node in one list (fieldSelector is unnecessary — we
	// group in-code, which is also what makes the fake clientset test meaningful).
	pods, err := k.cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	evictable := map[string]int{}
	for i := range pods.Items {
		p := &pods.Items[i]
		if IsEvictable(p) {
			evictable[p.Spec.NodeName]++
		}
	}
	out := make([]Node, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, mapNode(&list.Items[i], evictable))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
```

Update `mapNode` to take the count map and set the two fields:

```go
func mapNode(n *corev1.Node, evictable map[string]int) Node {
	return Node{
		Name:           n.Name,
		Status:         readyStatus(n),
		Roles:          roles(n.Labels),
		Region:         n.Labels[regionLabel],
		KubeletVersion: n.Status.NodeInfo.KubeletVersion,
		CreatedAt:      n.CreationTimestamp.Time,
		Schedulable:    !n.Spec.Unschedulable,
		EvictablePods:  evictable[n.Name],
	}
}
```

(`ListClusters` calls `ListNodes`, so it is unaffected beyond the extra fields.)

- [ ] **Step 4: Run — verify it passes**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./services/operator/internal/estate/...`
Expected: PASS (the new test + existing estate tests — the existing ones use `fake.NewSimpleClientset()` with nodes only; an empty pod list yields `EvictablePods: 0`, so they still pass).

- [ ] **Step 5: Commit**

```bash
cd kanz && git add services/operator/internal/estate/
git commit -m "feat(operator): estate surfaces schedulable + evictable-pod count

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: nodeops — cordon/uncordon (patch) + drain (eviction loop)

**Files:**
- Create: `kanz/services/operator/internal/nodeops/nodeops.go`
- Test: `kanz/services/operator/internal/nodeops/nodeops_test.go`

**Interfaces:**
- Consumes: `kubernetes.Interface`, `estate.IsEvictable`.
- Produces: `func New(cs kubernetes.Interface, logger *slog.Logger) *Ops`; `(*Ops).Cordon/Uncordon/Drain(ctx, name) error`; internal `evictOnce(ctx, name) (remaining int, err error)`.

- [ ] **Step 1: Write the failing test**

Create `kanz/services/operator/internal/nodeops/nodeops_test.go`:

```go
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
```

- [ ] **Step 2: Run — verify it fails**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./services/operator/internal/nodeops/...`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement**

Create `kanz/services/operator/internal/nodeops/nodeops.go`:

```go
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

func (o *Ops) Cordon(ctx context.Context, name string) error   { return o.setUnschedulable(ctx, name, true) }
func (o *Ops) Uncordon(ctx context.Context, name string) error { return o.setUnschedulable(ctx, name, false) }

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
```

- [ ] **Step 4: Run — verify it passes**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./services/operator/internal/nodeops/...`
Expected: PASS (cordon flip, unknown-node NotFound, evictOnce ordinary-only, PDB-429-no-force-delete).

- [ ] **Step 5: Commit**

```bash
cd kanz && git add services/operator/internal/nodeops/
git commit -m "feat(operator): nodeops — cordon/uncordon + PDB-safe drain eviction

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: Operator gRPC handlers — Cordon/Uncordon/Drain + wiring

**Files:**
- Modify: `kanz/services/operator/internal/grpcsrv/server.go`
- Test: `kanz/services/operator/internal/grpcsrv/server_test.go`
- Modify: `kanz/services/operator/cmd/operator/main.go`

**Interfaces:**
- Consumes: a `NodeOps` interface (`nodeops.Ops` satisfies it).
- Produces: `(*Server).WithNodeOps(NodeOps) *Server` (builder-style setter); `Cordon`/`Uncordon`/`Drain` handlers.

- [ ] **Step 1: Write the failing test**

Add to `server_test.go`:

```go
type stubNodeOps struct {
	cordoned, uncordoned, drained string
	err                           error
}

func (s *stubNodeOps) Cordon(_ context.Context, name string) error   { s.cordoned = name; return s.err }
func (s *stubNodeOps) Uncordon(_ context.Context, name string) error { s.uncordoned = name; return s.err }
func (s *stubNodeOps) Drain(_ context.Context, name string) error    { s.drained = name; return s.err }

func TestCordonUncordonDrainDelegate(t *testing.T) {
	sn := &stubNodeOps{}
	srv := New(stubReader{}).WithNodeOps(sn)

	if _, err := srv.Cordon(context.Background(), &operatorpb.CordonRequest{Name: "london"}); err != nil {
		t.Fatalf("Cordon: %v", err)
	}
	if _, err := srv.Uncordon(context.Background(), &operatorpb.UncordonRequest{Name: "london"}); err != nil {
		t.Fatalf("Uncordon: %v", err)
	}
	if _, err := srv.Drain(context.Background(), &operatorpb.DrainRequest{Name: "london"}); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if sn.cordoned != "london" || sn.uncordoned != "london" || sn.drained != "london" {
		t.Errorf("delegation failed: %+v", sn)
	}
}

func TestCordonRejectsEmptyName(t *testing.T) {
	srv := New(stubReader{}).WithNodeOps(&stubNodeOps{})
	if _, err := srv.Cordon(context.Background(), &operatorpb.CordonRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestCordonUnconfiguredIsUnimplemented(t *testing.T) {
	if _, err := New(stubReader{}).Cordon(context.Background(), &operatorpb.CordonRequest{Name: "x"}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("want Unimplemented when nodeOps is nil, got %v", err)
	}
}
```

- [ ] **Step 2: Run — verify it fails**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./services/operator/internal/grpcsrv/... -run 'Cordon|Drain'`
Expected: FAIL — `WithNodeOps`/handlers undefined.

- [ ] **Step 3: Implement**

In `server.go`, add the interface + field + setter, and the three handlers. Add near the top:

```go
// NodeOps is the node-lifecycle surface the operator gRPC depends on (nodeops.Ops
// satisfies it). Optional — nil in a deployment without the node-writer RBAC.
type NodeOps interface {
	Cordon(ctx context.Context, name string) error
	Uncordon(ctx context.Context, name string) error
	Drain(ctx context.Context, name string) error
}
```

Add a `nodeOps NodeOps` field to `Server`, and a builder-style setter (keeps the existing constructors unchanged):

```go
// WithNodeOps attaches the node-lifecycle surface and returns the server (builder
// style, so it composes with New / NewWithProvisioner without new constructors).
func (s *Server) WithNodeOps(ops NodeOps) *Server { s.nodeOps = ops; return s }
```

Add the handlers (mapping a nil ops → Unimplemented, empty name → InvalidArgument, a k8s NotFound → NotFound, else Internal):

```go
func (s *Server) Cordon(ctx context.Context, req *operatorpb.CordonRequest) (*operatorpb.CordonResponse, error) {
	if err := s.nodeWrite(ctx, req.GetName(), func() error { return s.nodeOps.Cordon(ctx, req.GetName()) }); err != nil {
		return nil, err
	}
	return &operatorpb.CordonResponse{}, nil
}

func (s *Server) Uncordon(ctx context.Context, req *operatorpb.UncordonRequest) (*operatorpb.UncordonResponse, error) {
	if err := s.nodeWrite(ctx, req.GetName(), func() error { return s.nodeOps.Uncordon(ctx, req.GetName()) }); err != nil {
		return nil, err
	}
	return &operatorpb.UncordonResponse{}, nil
}

func (s *Server) Drain(ctx context.Context, req *operatorpb.DrainRequest) (*operatorpb.DrainResponse, error) {
	if err := s.nodeWrite(ctx, req.GetName(), func() error { return s.nodeOps.Drain(ctx, req.GetName()) }); err != nil {
		return nil, err
	}
	return &operatorpb.DrainResponse{}, nil
}

// nodeWrite is the shared guard+error mapping for the three node-write handlers.
func (s *Server) nodeWrite(_ context.Context, name string, do func() error) error {
	if s.nodeOps == nil {
		return status.Error(codes.Unimplemented, "node operations not configured")
	}
	if name == "" {
		return status.Error(codes.InvalidArgument, "node name is required")
	}
	if err := do(); err != nil {
		if apierrors.IsNotFound(err) {
			return status.Error(codes.NotFound, err.Error())
		}
		return status.Error(codes.Internal, err.Error())
	}
	return nil
}
```

Add imports: `apierrors "k8s.io/apimachinery/pkg/api/errors"`.

- [ ] **Step 4: Run — verify it passes**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./services/operator/internal/grpcsrv/...`
Expected: PASS (new + existing handlers).

- [ ] **Step 5: Wire it in main.go**

In `main.go`, attach node ops to the server (always — the clientset is always present). Change the server construction so the final `srv` gets `.WithNodeOps(nodeops.New(cs, logger))`. For example, where `srv` is built:

```go
	srv = srv.WithNodeOps(nodeops.New(cs, logger))
	srv.Register(grpcSrv)
```

(Apply `.WithNodeOps(...)` to both the provisioning and read-only branches — or once, after the if/else, on the resulting `srv`.) Add import `"github.com/kanz-eng/kanz/services/operator/internal/nodeops"`.

- [ ] **Step 6: Build + test the service**

Run: `cd kanz && GOFLAGS=-mod=mod go build ./services/operator/... && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./services/operator/...`
Expected: build exit 0; all operator package tests pass.

- [ ] **Step 7: Commit**

```bash
cd kanz && git add services/operator/internal/grpcsrv/ services/operator/cmd/operator/main.go
git commit -m "feat(operator): Cordon/Uncordon/Drain gRPC handlers + node-ops wiring

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 5: RBAC — node-writer ClusterRole; reader gains pods:list

**Files:**
- Modify: `kanz/infra/deploy/operator-deploy.yaml`

- [ ] **Step 1: Add `pods: list` to the reader ClusterRole**

In `operator-deploy.yaml`, the existing `operator-node-reader` ClusterRole has one rule (`nodes: get,list`). Add a second rule so the read model can count evictable pods (still read-only):

```yaml
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list"]
```

(Place it as a second entry under the existing `rules:` of `ClusterRole operator-node-reader`.)

- [ ] **Step 2: Add the node-writer ClusterRole + binding**

Append to `operator-deploy.yaml`:

```yaml
---
# S3a: node-lifecycle writes. A ClusterRole (nodes are cluster-scoped). Exactly patch
# on nodes (cordon/uncordon) + create on the pods/eviction subresource (drain). NO node
# create/delete, NO pod delete — drain is eviction-only so PodDisruptionBudgets are
# always honored. Separate from the read-only operator-node-reader, which stays read-only.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: operator-node-writer
  labels: { app.kubernetes.io/part-of: kanz }
rules:
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["patch"]
  - apiGroups: [""]
    resources: ["pods/eviction"]
    verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: operator-node-writer
  labels: { app.kubernetes.io/part-of: kanz }
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: operator-node-writer }
subjects:
  - { kind: ServiceAccount, name: operator, namespace: kanz-operator }
```

- [ ] **Step 3: Verify manifests still parse (deployability + volume guards)**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./test/arch/ -run 'Deployab|Volume' -v`
Expected: PASS (valid YAML; operator manifest unchanged in structure the guards check).

- [ ] **Step 4: Commit**

```bash
cd kanz && git add infra/deploy/operator-deploy.yaml
git commit -m "feat(operator): operator-node-writer ClusterRole (patch nodes + evict); reader reads pods

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 6: Arch guards — updated reader + new node-writer

**Files:**
- Modify: `kanz/test/arch/operator_rbac_test.go`

- [ ] **Step 1: Update the reader guard to allow `{nodes, pods}` read**

In `operator_rbac_test.go`, `TestOperatorClusterRoleIsNodeReadOnly` currently asserts the `operator-node-reader` ClusterRole's resources are all `nodes`. Widen the allowed read resources to `{nodes, pods}` (still rejecting any write verb). Change the resource check so a resource is allowed if it is in `{"nodes": true, "pods": true}` (keep the verb allow-list `{get, list}` and the write-verb rejection exactly as they are):

```go
	allowedResources := map[string]bool{"nodes": true, "pods": true}
	// ... inside the rule loop:
			if !allowedResources[res] {
				t.Errorf("operator-node-reader grants resource %q; only nodes/pods (read) are allowed", res)
			}
```

Leave the write-verb rejection and non-vacuity checks unchanged.

- [ ] **Step 2: Add the node-writer guard**

Reuse the existing `decodeOperatorManifest(t) []roleDoc` helper (added by S2a's `TestOperatorProvisionerRoleIsBounded`) — `roleDoc` already captures `Kind`, `Metadata.Name`, and `Rules[]{APIGroups, Resources, Verbs}`, which is exactly what this guard needs. Do NOT add a new reader helper. If, after grepping the file, `decodeOperatorManifest` is somehow absent, add it per S2a's shape; otherwise reuse it.

Add to `operator_rbac_test.go`:

```go
// TestOperatorNodeWriterRoleIsBounded asserts the node-writer ClusterRole grants
// exactly patch-on-nodes + create-on-pods/eviction, and NO node create/delete and NO
// verbs on the bare pods resource — so drain is eviction-only (PDB-honoring) and the
// operator can never remove a node object.
func TestOperatorNodeWriterRoleIsBounded(t *testing.T) {
	docs := decodeOperatorManifest(t)
	forbiddenNodeVerbs := map[string]bool{"create": true, "delete": true, "deletecollection": true, "update": true, "*": true}

	var found bool
	for _, doc := range docs {
		if doc.Kind != "ClusterRole" || doc.Metadata.Name != "operator-node-writer" {
			continue
		}
		found = true
		for _, r := range doc.Rules {
			for _, res := range r.Resources {
				switch res {
				case "nodes":
					for _, v := range r.Verbs {
						lv := strings.ToLower(v)
						if forbiddenNodeVerbs[lv] {
							t.Errorf("operator-node-writer grants %q on nodes — only patch is allowed (no create/delete/update)", v)
						}
						if lv != "patch" {
							t.Errorf("operator-node-writer grants verb %q on nodes; only patch (cordon) is allowed", v)
						}
					}
				case "pods/eviction":
					for _, v := range r.Verbs {
						if strings.ToLower(v) != "create" {
							t.Errorf("operator-node-writer grants %q on pods/eviction; only create is allowed", v)
						}
					}
				case "pods":
					// A bare "pods: delete" would let drain force-delete, bypassing PDBs.
					t.Errorf("operator-node-writer must not grant verbs on the bare pods resource (only pods/eviction) — got verbs %v", r.Verbs)
				default:
					t.Errorf("operator-node-writer grants resource %q; only nodes + pods/eviction are allowed", res)
				}
			}
		}
	}
	if !found {
		t.Fatalf("no ClusterRole operator-node-writer found — S3a RBAC missing")
	}
}
```

- [ ] **Step 3: Run both guards**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./test/arch/ -run 'OperatorClusterRole|OperatorNodeWriter|OperatorProvisioner' -v`
Expected: PASS (reader now allows pods read; the new writer guard passes; the S2a provisioner guard unaffected).

- [ ] **Step 4: Mutation-prove the writer guard**

Temporarily add `delete` to the writer ClusterRole's `nodes` verbs in the manifest, run `TestOperatorNodeWriterRoleIsBounded` → expect FAIL ("only patch is allowed"). Revert; re-run PASS. Confirm `git diff` on the manifest is clean before committing.

- [ ] **Step 5: Commit**

```bash
cd kanz && git add test/arch/operator_rbac_test.go
git commit -m "test(arch): node-writer bounded (patch+evict only); reader reads pods (still read-only)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 7: TUI — node selection + cordon/uncordon/drain + confirm + status

**Files:**
- Modify: `kanz/cmd/universe/source.go` (widen `nodeSource`; add schedulable/evictable to `nodeRow`)
- Modify: `kanz/cmd/universe/model.go` (selection + c/u/d keys + drain confirm + action cmds/msgs)
- Modify: `kanz/cmd/universe/view.go` (highlight selected row; status column; confirm prompt)
- Test: `kanz/cmd/universe/model_test.go` / a nodes-pane test file

**Interfaces:**
- Consumes: `operatorpb` Cordon/Uncordon/Drain + `Node.GetSchedulable/GetEvictablePods`.
- Produces: `nodeSource.cordon/uncordon/drain(ctx, name)`; `model.selected int`; a `confirmingDrain` state.

- [ ] **Step 1: Widen the source (update ALL implementers)**

In `source.go`, add to `nodeSource`:

```go
	cordon(ctx context.Context, name string) error
	uncordon(ctx context.Context, name string) error
	drain(ctx context.Context, name string) error
```

Add the `grpcSource` impls (each a thin RPC call returning the error):

```go
func (g *grpcSource) cordon(ctx context.Context, name string) error {
	_, err := g.client.Cordon(ctx, &operatorpb.CordonRequest{Name: name})
	return err
}
func (g *grpcSource) uncordon(ctx context.Context, name string) error {
	_, err := g.client.Uncordon(ctx, &operatorpb.UncordonRequest{Name: name})
	return err
}
func (g *grpcSource) drain(ctx context.Context, name string) error {
	_, err := g.client.Drain(ctx, &operatorpb.DrainRequest{Name: name})
	return err
}
```

Add `schedulable bool` + `evictablePods int` to `nodeRow`, and set them in `toNodeRows` from `n.GetSchedulable()` / `int(n.GetEvictablePods())`. Update the test `stubSource` (in `model_test.go`) with no-op `cordon`/`uncordon`/`drain` returning nil (grep for every `nodeSource` implementer).

- [ ] **Step 2: Write the failing test**

Add a nodes-pane test (e.g. in `model_test.go`):

```go
func TestNodeSelectionMovesAndActionsFire(t *testing.T) {
	m := newModel(Config{}, stubSource{})
	m.active = paneNodes
	m.nodes = []nodeRow{{Name: "a", schedulable: true}, {Name: "b", schedulable: true}}
	// down moves selection
	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if u.(model).selected != 1 {
		t.Fatalf("down should select row 1, got %d", u.(model).selected)
	}
	// 'c' on the selected node returns a command
	u2, cmd := u.(model).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	if cmd == nil {
		t.Fatalf("c (cordon) should return a command")
	}
	_ = u2
}

func TestDrainAsksForConfirmation(t *testing.T) {
	m := newModel(Config{}, stubSource{})
	m.active = paneNodes
	m.nodes = []nodeRow{{Name: "a", schedulable: true, evictablePods: 3}}
	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	if cmd != nil {
		t.Fatalf("d should NOT fire drain immediately — it opens a confirm")
	}
	if !u.(model).confirmingDrain {
		t.Fatalf("d should enter the confirm state")
	}
	// 'y' confirms and fires
	_, cmd2 := u.(model).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if cmd2 == nil {
		t.Fatalf("y should fire the drain command")
	}
}

func TestNodeStatusLabels(t *testing.T) {
	for _, tc := range []struct {
		row  nodeRow
		want string
	}{
		{nodeRow{schedulable: true}, "Ready"},
		{nodeRow{schedulable: false, evictablePods: 3}, "Draining (3)"},
		{nodeRow{schedulable: false, evictablePods: 0}, "Drained"},
	} {
		if got := nodeStateLabel(tc.row); got != tc.want {
			t.Errorf("nodeStateLabel(%+v) = %q, want %q", tc.row, got, tc.want)
		}
	}
}
```

- [ ] **Step 3: Run — verify it fails**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./cmd/universe/... -run 'NodeSelection|DrainAsks|NodeStatus'`
Expected: FAIL — selection/confirm/`nodeStateLabel` not present.

- [ ] **Step 4: Implement the model**

In `model.go`:
- Add fields: `selected int`, `confirmingDrain bool`, `actionErr error`.
- In the nodes-pane key handling (when `m.active == paneNodes` and not in a form/confirm): `up`/`down` move `selected` (clamped to `[0, len(nodes)-1]`); `c`/`u` return a cmd calling `cordon`/`uncordon` on `m.nodes[m.selected].Name`; `d` sets `confirmingDrain = true` (does NOT fire).
- When `confirmingDrain`: `y` → clear the flag and return the drain cmd for the selected node; `n`/`esc` → clear the flag.
- Guard `selected` against an empty/shrunk `nodes` slice (clamp on each fetch).
- Add the action command + result message:

```go
func (m model) nodeActionCmd(action func(context.Context, string) error, name string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
		defer cancel()
		return nodeActionMsg{err: action(ctx, name)}
	}
}

type nodeActionMsg struct{ err error }
```

Handle `nodeActionMsg` in `Update`: on error set `m.actionErr`; the node's new status arrives on the next poll. Add `nodeStateLabel`:

```go
func nodeStateLabel(n nodeRow) string {
	if n.schedulable {
		return "Ready"
	}
	if n.evictablePods > 0 {
		return fmt.Sprintf("Draining (%d)", n.evictablePods)
	}
	return "Drained"
}
```

- [ ] **Step 5: Render selection + status + confirm**

In `view.go`'s nodes-pane render: highlight the `m.selected` row (e.g. a `▸` marker / reverse style), replace the old Ready/NotReady status cell with `nodeStateLabel(n)` (keep a cordoned node that isn't draining showing `Cordoned` — note: `!schedulable && evictablePods>0 → Draining`, `!schedulable && evictablePods==0 → Drained`; if you want a distinct `Cordoned` for a freshly-cordoned node with pods, that is the `Draining (N)` state — acceptable, or add a `Cordoned` label when `!schedulable`). Add a footer hint `[↑↓] select  [c]ordon [u]ncordon [d]rain`. When `m.confirmingDrain`, render a prompt line: `Drain <selected node>? evicts N pods  [y/n]`. Keep `render` pure.

- [ ] **Step 6: Run — verify it passes**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./cmd/universe/... && GOFLAGS=-mod=mod go build ./cmd/universe/... && gofmt -l cmd/universe`
Expected: tests PASS (new + existing); build exit 0; gofmt clean.

- [ ] **Step 7: Commit**

```bash
cd kanz && git add cmd/universe/
git commit -m "feat(universe): node selection + cordon/uncordon/drain (drain confirmed) + status

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 8: Whole-slice verification + rig-proof handoff

**Files:** none (verification only).

- [ ] **Step 1: Whole-slice build/vet/gofmt**

Run:
```bash
cd kanz && GOFLAGS=-mod=mod go build ./... && \
GOFLAGS=-mod=mod go vet ./cmd/universe/... ./services/operator/... && \
gofmt -l cmd/universe services/operator
```
Expected: exit 0; `gofmt -l` prints nothing.

- [ ] **Step 2: Full arch suite**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./test/arch/...`
Expected: PASS — including `TestNoSSHPlane` (no `crypto/ssh` — these are k8s API calls), the updated reader guard + new `TestOperatorNodeWriterRoleIsBounded`, the S2a provisioner guards, deployability.

- [ ] **Step 3: govulncheck**

Run: `cd kanz && GOFLAGS=-mod=mod go run golang.org/x/vuln/cmd/govulncheck@latest ./...`
Expected: 0 reachable (2 known-unreachable advisories acceptable). If any reachable vuln appears, STOP and report.

- [ ] **Step 4: Rig proof (manual — FULLY provable on the kind rig)**

Unlike S2a's k3s-gated join, cordon/drain are real k8s ops on the kind rig:
```bash
kubectl apply -f kanz/infra/deploy/operator-deploy.yaml   # applies the node-writer ClusterRole/binding + reader pods:list
kubectl -n kanz-operator port-forward deploy/operator 9090:9090
# in universe: Nodes pane → ↑↓ select a rig node
#   press c → next poll shows the node non-Ready (Cordoned/Draining); kubectl get node → SchedulingDisabled
#   press u → back to Ready; kubectl get node → no SchedulingDisabled
#   press d → confirm y → the node shows Draining (N) then Drained as its pods evict
#             (kubectl get pods -o wide shows them rescheduling; PDBs are honored, nothing force-killed)
```

- [ ] **Step 5: Board update (controller)** — add the S3a readiness row (fully rig-provable), note the new writer ClusterRole. Validate with `bash tools/validate-board.sh KANZ_TASKS.md`, commit, push.

---

## Notes for the executor

- **Drain is eviction-only, forever.** No task may add a pod `Delete` on the drain path (it would bypass PDBs). The RBAC guard (Task 6) enforces this — `operator-node-writer` grants no `pods: delete`.
- **Fake clientset gotchas (Tasks 2, 3):** the fake ignores `FieldSelector` (filter by `Spec.NodeName` in code) and surfaces eviction as a `create` on the `pods`/`eviction` subresource (capture with `PrependReactor("create","pods", ...)` checking `action.GetSubresource()=="eviction"`).
- **Interface-widening (Tasks 4, 7):** `NodeOps` and `nodeSource` each gain methods; every implementer (real + test stub) must gain them or the package won't compile.
- **Deferred by design (do NOT add here):** Move Node (region relabel) — S3b; Balance — dropped; node create/delete.
- **Git hygiene:** never `git add -A` — stage explicit paths (untracked `universe.exe`/`kanz-monitor.exe`/`kanz-py/gen/` must not be committed).
