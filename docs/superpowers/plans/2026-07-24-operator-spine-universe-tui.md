# Operator Spine + Universe TUI (read-only F0+S1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stand up an in-cluster `operator` gRPC service that lists the Kubernetes estate read-only, plus a thin `universe` terminal UI that renders it — proving the thin-TUI ↔ operator-service ↔ k8s path end-to-end before any privileged write.

**Architecture:** A new `services/operator` service uses `client-go` (in-cluster ServiceAccount) to list Nodes, maps them to a clean read model, and serves `operator.v1.OperatorService` (`ListNodes`, `ListClusters`) over gRPC. A new `cmd/universe` Bubble Tea TUI dials that service over a kubeconfig-gated `kubectl port-forward` (plaintext gRPC inside the RBAC-gated tunnel — the laptop holds no SVID) and renders two read-only panes.

**Tech Stack:** Go (module `github.com/kanz-eng/kanz-schemas-go` generated protos via `buf`), `k8s.io/client-go`, `google.golang.org/grpc`, `github.com/charmbracelet/bubbletea` + `lipgloss`.

## Global Constraints

Every task's requirements implicitly include this section.

- **Go toolchain:** `go 1.26.1` / `toolchain go1.26.5`. All `go` commands run from `kanz/` with `GOFLAGS=-mod=mod` (the Makefile sets this; set it explicitly for raw `go` commands).
- **Generated protobuf types** are imported from module root **`github.com/kanz-eng/kanz-schemas-go/<pkg>/v1`** (resolved locally by the `replace => ../kanz-schemas/gen/go` in `kanz/go.mod`). Generated code is **not committed** — regenerate with `make generate` (runs `cd ../kanz-schemas && buf generate`).
- **`nodes` are a cluster-scoped resource** → RBAC MUST be a **ClusterRole + ClusterRoleBinding**, never a namespaced Role. A namespaced Role cannot grant access to `nodes`.
- **Namespace:** the operator service deploys into **`kanz-operator`** (the SEC-M3c SPIFFE-enabled namespace), NOT `kanz-services`.
- **No `crypto/ssh`.** This slice introduces zero SSH surface. The `test/arch/ssh_plane_test.go` guard MUST stay green after adding `client-go`. SSH provisioning is subsystem S2, out of scope here.
- **Least privilege:** the operator's ClusterRole grants exactly `get`/`list` on `nodes` and nothing else. Any write verb is a build failure (Task 6 enforces this).
- **Distroless/static images:** `FROM gcr.io/distroless/static:nonroot`, `USER nonroot:nonroot` (uid 65532), `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"`. Docker build context is the **repo root**: `docker build -f kanz/services/operator/Dockerfile .`
- **Read-only:** no proto RPC mutates; no write verb in RBAC; the TUI has no `[A]dd/[E]dit/[D]elete/[S]SH` keys. Those are S2–S5.
- **buf lint** is `STANDARD`: request/response messages MUST be named `<Method>Request` / `<Method>Response`.

---

### Task 1: The `operator.v1` proto + generated SDK

**Files:**
- Create: `kanz-schemas/proto/operator/v1/operator.proto`
- Regenerates (not committed): `kanz-schemas/gen/go/operator/v1/operator.pb.go`, `kanz-schemas/gen/go/operator/v1/operator_grpc.pb.go`

**Interfaces:**
- Produces: `operatorpb.OperatorServiceServer` interface with `ListNodes(context.Context, *operatorpb.ListNodesRequest) (*operatorpb.ListNodesResponse, error)` and `ListClusters(context.Context, *operatorpb.ListClustersRequest) (*operatorpb.ListClustersResponse, error)`; message types `Node`, `Cluster`, enum `NodeStatus`; `RegisterOperatorServiceServer(grpc.ServiceRegistrar, OperatorServiceServer)`.

- [ ] **Step 1: Write the proto**

Create `kanz-schemas/proto/operator/v1/operator.proto`:

```proto
syntax = "proto3";

// Operator service — the READ surface over the Kubernetes estate for the
// root@universe operator plane (F0+S1). Read-only: ListNodes / ListClusters
// ONLY, no provisioning/mutation RPC. Writes (node join, drain, key material)
// arrive in later subsystems as their own RPCs.
//
// # Consumers
//
// The `operator` service implements the server, reading Node objects from the
// Kubernetes API via client-go under a get/list-nodes ClusterRole. The
// `universe` TUI is the only client, dialing over a kubeconfig-gated
// port-forward. No other service dials this.
//
// # Versioning
//
// operator.v1, additive-only within the version — a removed/retyped field or
// RPC breaks every client and requires a sibling operator/v2.
package operator.v1;

import "google/protobuf/timestamp.proto";

// OperatorService is the gRPC read surface over the estate.
service OperatorService {
  // ListNodes returns every Node the operator can see, with the status,
  // roles, region and version the Kubernetes API reports.
  rpc ListNodes(ListNodesRequest) returns (ListNodesResponse);

  // ListClusters groups those nodes by region into the Europe/Asia/USA view,
  // with online/offline counts.
  rpc ListClusters(ListClustersRequest) returns (ListClustersResponse);
}

// NodeStatus is the node's readiness. UNSPECIFIED (the zero value) is the
// deny-by-default state: a node whose Ready condition is absent or Unknown
// maps here and must render as not-ready, never silently as ready.
enum NodeStatus {
  NODE_STATUS_UNSPECIFIED = 0;
  NODE_STATUS_READY = 1;
  NODE_STATUS_NOT_READY = 2;
}

message ListNodesRequest {}

message ListNodesResponse {
  repeated Node nodes = 1;
}

message Node {
  string name = 1;
  NodeStatus status = 2;
  // roles are the node-role.kubernetes.io/<role> label suffixes.
  repeated string roles = 3;
  // region is the topology.kubernetes.io/region label value; empty if unset.
  string region = 4;
  string kubelet_version = 5;
  // created_at is the node's CreationTimestamp. Age is derived client-side —
  // the server does not render a duration string against its own clock.
  google.protobuf.Timestamp created_at = 6;
}

message ListClustersRequest {}

message ListClustersResponse {
  repeated Cluster clusters = 1;
}

message Cluster {
  // region is the grouping key (topology.kubernetes.io/region value); nodes
  // with no region label group under the empty region.
  string region = 1;
  int32 online = 2;
  int32 offline = 3;
}
```

- [ ] **Step 2: Lint the proto**

Run: `cd kanz-schemas && buf lint`
Expected: no output, exit 0. (If it complains about request/response names, they must be `List<Noun>Request`/`Response` — they already are.)

- [ ] **Step 3: Generate the SDK**

Run: `cd kanz && make generate`
Expected: completes; `ls ../kanz-schemas/gen/go/operator/v1/` shows `operator.pb.go` and `operator_grpc.pb.go`.

- [ ] **Step 4: Verify the generated types compile and are importable**

Run: `cd kanz && GOFLAGS=-mod=mod go build github.com/kanz-eng/kanz-schemas-go/operator/v1`
Expected: exit 0, no output.

- [ ] **Step 5: Commit**

```bash
cd kanz-schemas && git add proto/operator/v1/operator.proto
git commit -m "feat(schemas): operator.v1 read surface (ListNodes/ListClusters)"
```

---

### Task 2: `client-go` dependency + the estate read model

**Files:**
- Modify: `kanz/go.mod`, `kanz/go.sum` (add `k8s.io/client-go`, `k8s.io/api`, `k8s.io/apimachinery`)
- Create: `kanz/services/operator/internal/estate/estate.go`
- Test: `kanz/services/operator/internal/estate/estate_test.go`

**Interfaces:**
- Produces:
  - `type NodeStatus int` with `StatusUnknown NodeStatus = 0`, `StatusReady = 1`, `StatusNotReady = 2`
  - `type Node struct { Name string; Status NodeStatus; Roles []string; Region string; KubeletVersion string; CreatedAt time.Time }`
  - `type Cluster struct { Region string; Online int; Offline int }`
  - `type Reader interface { ListNodes(ctx context.Context) ([]Node, error); ListClusters(ctx context.Context) ([]Cluster, error) }`
  - `func NewK8s(cs kubernetes.Interface) *K8s` where `*K8s` implements `Reader`

- [ ] **Step 1: Add the client-go dependency**

Run:
```bash
cd kanz && GOFLAGS=-mod=mod go get k8s.io/client-go@v0.31.3 k8s.io/api@v0.31.3 k8s.io/apimachinery@v0.31.3
GOFLAGS=-mod=mod go mod tidy
```
Expected: `go.mod` now requires the three `k8s.io/*` modules; exit 0.

- [ ] **Step 2: Confirm the SSH-plane guard is unaffected by the new dependency**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./test/arch/ -run SSH -v`
Expected: PASS (client-go does not import `crypto/ssh` on any reachable path). If it FAILS, stop — that is a real regression to resolve before proceeding, not something to work around.

- [ ] **Step 3: Write the failing test**

Create `kanz/services/operator/internal/estate/estate_test.go`:

```go
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
```

- [ ] **Step 4: Run test to verify it fails**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./services/operator/internal/estate/...`
Expected: FAIL — `estate.go` does not exist (`undefined: NewK8s`).

- [ ] **Step 5: Write the implementation**

Create `kanz/services/operator/internal/estate/estate.go`:

```go
// Package estate reads the Kubernetes node inventory into a clean, proto-free
// read model. It is the operator service's view of the estate: what nodes
// exist, whether they are ready, and how they group into regions. The gRPC
// adapter (internal/grpcsrv) converts these types to operator.v1; nothing here
// imports the generated schema, so the read model is testable against a fake
// clientset with no gRPC in the loop.
package estate

import (
	"context"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// NodeStatus is a node's readiness. The zero value is the deny-by-default
// state — an absent/Unknown Ready condition is StatusUnknown, never ready.
type NodeStatus int

const (
	StatusUnknown NodeStatus = iota
	StatusReady
	StatusNotReady
)

const (
	regionLabel = "topology.kubernetes.io/region"
	rolePrefix  = "node-role.kubernetes.io/"
)

// Node is the operator's projection of a Kubernetes Node.
type Node struct {
	Name           string
	Status         NodeStatus
	Roles          []string
	Region         string
	KubeletVersion string
	CreatedAt      time.Time
}

// Cluster is a region grouping with online/offline counts.
type Cluster struct {
	Region  string
	Online  int
	Offline int
}

// Reader is the read surface the gRPC adapter depends on. A fake clientset in
// tests and the live cluster in production both satisfy it via *K8s.
type Reader interface {
	ListNodes(ctx context.Context) ([]Node, error)
	ListClusters(ctx context.Context) ([]Cluster, error)
}

// K8s reads nodes from the Kubernetes API.
type K8s struct {
	cs kubernetes.Interface
}

// NewK8s returns a Reader over the given clientset.
func NewK8s(cs kubernetes.Interface) *K8s { return &K8s{cs: cs} }

func (k *K8s) ListNodes(ctx context.Context) ([]Node, error) {
	list, err := k.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, mapNode(&list.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (k *K8s) ListClusters(ctx context.Context) ([]Cluster, error) {
	nodes, err := k.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	idx := map[string]*Cluster{}
	var order []string
	for _, n := range nodes {
		c, ok := idx[n.Region]
		if !ok {
			c = &Cluster{Region: n.Region}
			idx[n.Region] = c
			order = append(order, n.Region)
		}
		if n.Status == StatusReady {
			c.Online++
		} else {
			c.Offline++
		}
	}
	sort.Strings(order)
	out := make([]Cluster, 0, len(order))
	for _, r := range order {
		out = append(out, *idx[r])
	}
	return out, nil
}

func mapNode(n *corev1.Node) Node {
	return Node{
		Name:           n.Name,
		Status:         readyStatus(n),
		Roles:          roles(n.Labels),
		Region:         n.Labels[regionLabel],
		KubeletVersion: n.Status.NodeInfo.KubeletVersion,
		CreatedAt:      n.CreationTimestamp.Time,
	}
}

// readyStatus maps the NodeReady condition to a NodeStatus. Absent or Unknown
// ⇒ StatusUnknown (deny-by-default): an offline node must read as offline.
func readyStatus(n *corev1.Node) NodeStatus {
	for _, c := range n.Status.Conditions {
		if c.Type != corev1.NodeReady {
			continue
		}
		switch c.Status {
		case corev1.ConditionTrue:
			return StatusReady
		case corev1.ConditionFalse:
			return StatusNotReady
		default:
			return StatusUnknown
		}
	}
	return StatusUnknown
}

// roles extracts node-role.kubernetes.io/<role> label suffixes, sorted.
func roles(labels map[string]string) []string {
	var out []string
	for k := range labels {
		if strings.HasPrefix(k, rolePrefix) {
			if r := strings.TrimPrefix(k, rolePrefix); r != "" {
				out = append(out, r)
			}
		}
	}
	sort.Strings(out)
	return out
}
```

- [ ] **Step 6: Run test to verify it passes**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./services/operator/internal/estate/...`
Expected: PASS (4 tests).

- [ ] **Step 7: Commit**

```bash
cd kanz && git add go.mod go.sum services/operator/internal/estate/
git commit -m "feat(operator): estate read model over client-go node inventory"
```

---

### Task 3: The `grpcsrv` adapter (operator.v1 over the read model)

**Files:**
- Create: `kanz/services/operator/internal/grpcsrv/server.go`
- Test: `kanz/services/operator/internal/grpcsrv/server_test.go`

**Interfaces:**
- Consumes: `estate.Reader`, `estate.Node`, `estate.Cluster`, `estate.StatusReady/NotReady/Unknown`; `operatorpb` generated types from Task 1.
- Produces: `func New(r estate.Reader) *Server`; `func (s *Server) Register(r grpc.ServiceRegistrar)`; `*Server` implements `operatorpb.OperatorServiceServer`.

- [ ] **Step 1: Write the failing test**

Create `kanz/services/operator/internal/grpcsrv/server_test.go`:

```go
package grpcsrv

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	operatorpb "github.com/kanz-eng/kanz-schemas-go/operator/v1"

	"github.com/kanz-eng/kanz/services/operator/internal/estate"
)

type stubReader struct {
	nodes    []estate.Node
	clusters []estate.Cluster
	err      error
}

func (s stubReader) ListNodes(context.Context) ([]estate.Node, error) {
	return s.nodes, s.err
}
func (s stubReader) ListClusters(context.Context) ([]estate.Cluster, error) {
	return s.clusters, s.err
}

func TestListNodesMapsStatusToEnum(t *testing.T) {
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	srv := New(stubReader{nodes: []estate.Node{
		{Name: "london", Status: estate.StatusReady, Roles: []string{"control-plane"}, Region: "europe", KubeletVersion: "v1.31.3", CreatedAt: created},
		{Name: "tokyo", Status: estate.StatusNotReady, Region: "asia"},
		{Name: "ghost", Status: estate.StatusUnknown},
	}})
	resp, err := srv.ListNodes(context.Background(), &operatorpb.ListNodesRequest{})
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(resp.GetNodes()) != 3 {
		t.Fatalf("want 3 nodes, got %d", len(resp.GetNodes()))
	}
	got := map[string]*operatorpb.Node{}
	for _, n := range resp.GetNodes() {
		got[n.GetName()] = n
	}
	if got["london"].GetStatus() != operatorpb.NodeStatus_NODE_STATUS_READY {
		t.Errorf("london status = %v", got["london"].GetStatus())
	}
	if got["tokyo"].GetStatus() != operatorpb.NodeStatus_NODE_STATUS_NOT_READY {
		t.Errorf("tokyo status = %v", got["tokyo"].GetStatus())
	}
	if got["ghost"].GetStatus() != operatorpb.NodeStatus_NODE_STATUS_UNSPECIFIED {
		t.Errorf("ghost status = %v, want UNSPECIFIED (deny-by-default)", got["ghost"].GetStatus())
	}
	if got["london"].GetCreatedAt().AsTime().UTC() != created {
		t.Errorf("london created_at = %v, want %v", got["london"].GetCreatedAt().AsTime(), created)
	}
}

func TestListClustersMapsCounts(t *testing.T) {
	srv := New(stubReader{clusters: []estate.Cluster{{Region: "usa", Online: 2, Offline: 1}}})
	resp, err := srv.ListClusters(context.Background(), &operatorpb.ListClustersRequest{})
	if err != nil {
		t.Fatalf("ListClusters: %v", err)
	}
	if len(resp.GetClusters()) != 1 {
		t.Fatalf("want 1 cluster, got %d", len(resp.GetClusters()))
	}
	c := resp.GetClusters()[0]
	if c.GetRegion() != "usa" || c.GetOnline() != 2 || c.GetOffline() != 1 {
		t.Errorf("cluster = %+v", c)
	}
}

func TestReaderErrorMapsToInternal(t *testing.T) {
	srv := New(stubReader{err: errors.New("boom")})
	_, err := srv.ListNodes(context.Background(), &operatorpb.ListNodesRequest{})
	if status.Code(err) != codes.Internal {
		t.Fatalf("want Internal, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./services/operator/internal/grpcsrv/...`
Expected: FAIL — `undefined: New`.

- [ ] **Step 3: Write the implementation**

Create `kanz/services/operator/internal/grpcsrv/server.go`:

```go
// Package grpcsrv is the operator service's gRPC adapter: a thin translation
// of operator.v1.OperatorService onto the estate.Reader read model. It holds
// no logic — the node listing and grouping live in internal/estate; this layer
// only converts the read model to proto and maps errors to gRPC codes.
package grpcsrv

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	operatorpb "github.com/kanz-eng/kanz-schemas-go/operator/v1"

	"github.com/kanz-eng/kanz/services/operator/internal/estate"
)

// Server adapts an estate.Reader to the generated OperatorService interface.
type Server struct {
	operatorpb.UnimplementedOperatorServiceServer
	reader estate.Reader
}

// New returns a Server over the given Reader.
func New(r estate.Reader) *Server { return &Server{reader: r} }

// Register binds the server onto a grpc.ServiceRegistrar.
func (s *Server) Register(r grpc.ServiceRegistrar) {
	operatorpb.RegisterOperatorServiceServer(r, s)
}

func (s *Server) ListNodes(ctx context.Context, _ *operatorpb.ListNodesRequest) (*operatorpb.ListNodesResponse, error) {
	nodes, err := s.reader.ListNodes(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := make([]*operatorpb.Node, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, &operatorpb.Node{
			Name:           n.Name,
			Status:         protoStatus(n.Status),
			Roles:          n.Roles,
			Region:         n.Region,
			KubeletVersion: n.KubeletVersion,
			CreatedAt:      nonZeroTimestamp(n.CreatedAt),
		})
	}
	return &operatorpb.ListNodesResponse{Nodes: out}, nil
}

func (s *Server) ListClusters(ctx context.Context, _ *operatorpb.ListClustersRequest) (*operatorpb.ListClustersResponse, error) {
	clusters, err := s.reader.ListClusters(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := make([]*operatorpb.Cluster, 0, len(clusters))
	for _, c := range clusters {
		out = append(out, &operatorpb.Cluster{
			Region:  c.Region,
			Online:  int32(c.Online),
			Offline: int32(c.Offline),
		})
	}
	return &operatorpb.ListClustersResponse{Clusters: out}, nil
}

func protoStatus(s estate.NodeStatus) operatorpb.NodeStatus {
	switch s {
	case estate.StatusReady:
		return operatorpb.NodeStatus_NODE_STATUS_READY
	case estate.StatusNotReady:
		return operatorpb.NodeStatus_NODE_STATUS_NOT_READY
	default:
		return operatorpb.NodeStatus_NODE_STATUS_UNSPECIFIED
	}
}

func nonZeroTimestamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// compile-time assertion the adapter satisfies the generated interface.
var _ operatorpb.OperatorServiceServer = (*Server)(nil)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./services/operator/internal/grpcsrv/...`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
cd kanz && git add services/operator/internal/grpcsrv/
git commit -m "feat(operator): operator.v1 gRPC adapter over the estate reader"
```

---

### Task 4: The operator service entrypoint (`cmd/operator`) + config + health

**Files:**
- Create: `kanz/services/operator/internal/config/config.go`
- Test: `kanz/services/operator/internal/config/config_test.go`
- Create: `kanz/services/operator/cmd/operator/main.go`

**Interfaces:**
- Consumes: `estate.NewK8s`, `grpcsrv.New`, `operatorpb`.
- Produces: `config.Config{GRPCListen, HealthListen string}`; `config.Load() (Config, error)`.

- [ ] **Step 1: Write the failing config test**

Create `kanz/services/operator/internal/config/config_test.go`:

```go
package config

import "testing"

func TestLoadDefaults(t *testing.T) {
	t.Setenv("OPERATOR_GRPC_LISTEN", "")
	t.Setenv("OPERATOR_HEALTH_LISTEN", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GRPCListen != ":9090" {
		t.Errorf("GRPCListen = %q, want :9090", cfg.GRPCListen)
	}
	if cfg.HealthListen != ":8091" {
		t.Errorf("HealthListen = %q, want :8091", cfg.HealthListen)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("OPERATOR_GRPC_LISTEN", ":7000")
	t.Setenv("OPERATOR_HEALTH_LISTEN", ":7001")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GRPCListen != ":7000" || cfg.HealthListen != ":7001" {
		t.Errorf("cfg = %+v", cfg)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./services/operator/internal/config/...`
Expected: FAIL — `undefined: Load`.

- [ ] **Step 3: Write the config implementation**

Create `kanz/services/operator/internal/config/config.go`:

```go
// Package config is the operator service's environment configuration. Every
// field is a listen coordinate — there is no write target, no credential, and
// no exchange/venue config in this slice (the operator reads the k8s API via
// its in-cluster ServiceAccount, which needs no config here).
package config

import "os"

// Config is the operator service configuration.
type Config struct {
	// GRPCListen is the address the operator.v1 gRPC server binds. It is not
	// fronted by a Service — the universe TUI reaches it via kubectl
	// port-forward, so the RBAC on pods/portforward is the access gate.
	GRPCListen string
	// HealthListen is the address the /healthz + /readyz HTTP server binds
	// (the target of the Deployment's liveness/readiness probes).
	HealthListen string
}

// Load reads the configuration from the environment, applying defaults.
func Load() (Config, error) {
	return Config{
		GRPCListen:   envOr("OPERATOR_GRPC_LISTEN", ":9090"),
		HealthListen: envOr("OPERATOR_HEALTH_LISTEN", ":8091"),
	}, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./services/operator/internal/config/...`
Expected: PASS (2 tests).

- [ ] **Step 5: Write the entrypoint**

Create `kanz/services/operator/cmd/operator/main.go`:

```go
// operator binary entrypoint. Reads the Kubernetes node inventory via the
// in-cluster ServiceAccount (a get/list-nodes ClusterRole) and serves it over
// operator.v1 gRPC for the universe TUI. Read-only: no write path, no SSH, no
// Vault reach in this slice.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/kanz-eng/kanz/services/operator/internal/config"
	"github.com/kanz-eng/kanz/services/operator/internal/estate"
	"github.com/kanz-eng/kanz/services/operator/internal/grpcsrv"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config load failed", "err", err)
		os.Exit(2)
	}

	// In-cluster REST config: the pod's ServiceAccount token + the API server
	// CA, mounted by Kubernetes. The ClusterRole (Task 5) scopes it to
	// get/list nodes.
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		logger.Error("in-cluster config failed (operator runs as a pod)", "err", err)
		os.Exit(2)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		logger.Error("kubernetes client init failed", "err", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Health server: liveness/readiness targets for the Deployment probes.
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	healthSrv := &http.Server{Addr: cfg.HealthListen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		logger.Info("operator health listening", "addr", cfg.HealthListen)
		if err := healthSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("health server failed", "err", err)
			stop()
		}
	}()

	// gRPC server: plaintext, reached only via kubeconfig-gated port-forward.
	lis, err := net.Listen("tcp", cfg.GRPCListen)
	if err != nil {
		logger.Error("grpc listen failed", "addr", cfg.GRPCListen, "err", err)
		os.Exit(2)
	}
	grpcSrv := grpc.NewServer()
	grpcsrv.New(estate.NewK8s(cs)).Register(grpcSrv)
	go func() {
		logger.Info("operator gRPC listening", "addr", cfg.GRPCListen)
		if err := grpcSrv.Serve(lis); err != nil {
			logger.Error("grpc server failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("operator shutting down")
	grpcSrv.GracefulStop()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = healthSrv.Shutdown(shutCtx)
}
```

- [ ] **Step 6: Verify the service builds**

Run: `cd kanz && GOFLAGS=-mod=mod go build ./services/operator/...`
Expected: exit 0.

- [ ] **Step 7: Commit**

```bash
cd kanz && git add services/operator/internal/config/ services/operator/cmd/
git commit -m "feat(operator): cmd/operator entrypoint — in-cluster nodes over gRPC + health"
```

---

### Task 5: Deployment manifest, ClusterRole, Dockerfile, build.yml entry

**Files:**
- Create: `kanz/infra/deploy/operator-deploy.yaml`
- Create: `kanz/services/operator/Dockerfile`
- Modify: `.github/workflows/build.yml` (add `- service: operator`)

**Interfaces:**
- Produces: a deployable operator service satisfying `test/arch/deployability_test.go`. The ClusterRole is named `operator-node-reader` (Task 6's arch guard asserts against this exact name).

- [ ] **Step 1: Write the deployment manifest**

Create `kanz/infra/deploy/operator-deploy.yaml`:

```yaml
# Operator service (F0+S1): reads the Kubernetes node inventory read-only and
# serves it over operator.v1 gRPC for the universe TUI. Deploys into the
# SEC-M3c kanz-operator namespace. Its gRPC port is deliberately NOT fronted by
# a Service — the TUI reaches it via `kubectl port-forward`, so RBAC on
# pods/portforward is the access gate.
apiVersion: v1
kind: ServiceAccount
metadata:
  name: operator
  namespace: kanz-operator
  labels: { app.kubernetes.io/part-of: kanz }
---
# nodes are cluster-scoped, so node read access MUST be a ClusterRole (a
# namespaced Role cannot grant it). Least privilege: get/list on nodes, nothing
# else. Task 6's arch guard fails the build if a write verb or another resource
# appears here.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: operator-node-reader
  labels: { app.kubernetes.io/part-of: kanz }
rules:
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get", "list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: operator-node-reader
  labels: { app.kubernetes.io/part-of: kanz }
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: operator-node-reader
subjects:
  - kind: ServiceAccount
    name: operator
    namespace: kanz-operator
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: operator
  namespace: kanz-operator
  labels: { app.kubernetes.io/part-of: kanz, app: operator }
spec:
  replicas: 1
  selector:
    matchLabels: { app: operator }
  template:
    metadata:
      labels: { app: operator, app.kubernetes.io/part-of: kanz }
    spec:
      serviceAccountName: operator
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        fsGroup: 65532
        seccompProfile: { type: RuntimeDefault }
      containers:
        - name: operator
          image: operator:latest
          args: []
          ports:
            - { name: grpc, containerPort: 9090 }
            - { name: health, containerPort: 8091 }
          livenessProbe:
            httpGet: { path: /healthz, port: 8091 }
            initialDelaySeconds: 5
            periodSeconds: 10
          readinessProbe:
            httpGet: { path: /readyz, port: 8091 }
            initialDelaySeconds: 5
            periodSeconds: 10
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            runAsNonRoot: true
            capabilities: { drop: ["ALL"] }
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: operator
  namespace: kanz-operator
spec:
  minAvailable: 1
  selector:
    matchLabels: { app: operator }
```

- [ ] **Step 2: Write the Dockerfile**

Create `kanz/services/operator/Dockerfile` (mirrors `kanz/cmd/kanz-halt/Dockerfile`; build context is the repo root):

```dockerfile
# syntax=docker/dockerfile:1
#
# operator service (F0+S1). Distroless + static binary. Build context is the
# repo ROOT, not kanz/ — the module replaces
# github.com/kanz-eng/kanz-schemas-go => ../kanz-schemas/gen/go, generated
# (not committed). Build with:
#   docker build -f kanz/services/operator/Dockerfile .
FROM golang:1.26.5 AS build
WORKDIR /src
ENV GOFLAGS=-mod=mod

COPY kanz-schemas/gen/go/ ./kanz-schemas/gen/go/
COPY kanz/go.mod kanz/go.sum ./kanz/
WORKDIR /src/kanz
RUN go mod download

COPY kanz/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
      -o /out/operator ./services/operator/cmd/operator

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/operator /operator
USER nonroot:nonroot
ENTRYPOINT ["/operator"]
```

- [ ] **Step 3: Add the build.yml service entry**

In `.github/workflows/build.yml`, find the image build matrix `service:` list and add an entry (alphabetical placement, matching the existing style):

```yaml
        - service: operator
```

Verify the surrounding list format first with: `grep -n "service:" .github/workflows/build.yml | head`. Match the exact indentation/format of the neighbouring entries.

- [ ] **Step 4: Run the deployability guard**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./test/arch/ -run Deployab -v`
Expected: PASS — the operator service now has a Dockerfile, a `build.yml` entry, and an `infra/deploy/operator-deploy.yaml` manifest, and its probes (`/healthz`, `/readyz`) point at routes the service serves.

- [ ] **Step 5: Commit**

```bash
cd kanz && git add infra/deploy/operator-deploy.yaml services/operator/Dockerfile
cd .. && git add .github/workflows/build.yml
git commit -m "feat(operator): deploy manifest, node-reader ClusterRole, image, CI build entry"
```

(Adjust the two `git add` paths to your repo layout — the workflow file lives at the repo root, the manifest/Dockerfile under `kanz/`.)

---

### Task 6: The RBAC arch guard

**Files:**
- Create: `kanz/test/arch/operator_rbac_test.go`

**Interfaces:**
- Consumes: `moduleRoot(t)` helper (in `test/arch/risk_boundary_test.go`), `gopkg.in/yaml.v3` (already a dependency, used by `deployability_test.go`).

- [ ] **Step 1: Write the guard test**

Create `kanz/test/arch/operator_rbac_test.go`:

```go
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

// clusterRoleDoc captures only the fields this guard inspects from a
// multi-document manifest.
type clusterRoleDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Rules []struct {
		APIGroups []string `yaml:"apiGroups"`
		Resources []string `yaml:"resources"`
		Verbs     []string `yaml:"verbs"`
	} `yaml:"rules"`
}

// TestOperatorClusterRoleIsNodeReadOnly asserts the operator service's
// ClusterRole grants exactly get/list on nodes and nothing else. The operator
// is the first workload with Kubernetes-API RBAC; this guard keeps it
// least-privilege — a write verb or extra resource fails the build.
func TestOperatorClusterRoleIsNodeReadOnly(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, "infra", "deploy", "operator-deploy.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	allowedVerbs := map[string]bool{"get": true, "list": true}
	writeVerbs := map[string]bool{
		"create": true, "update": true, "patch": true,
		"delete": true, "deletecollection": true, "*": true,
	}

	var found bool
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	for {
		var doc clusterRoleDoc
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("parse %s as YAML: %v", path, err)
		}
		if doc.Kind != "ClusterRole" || doc.Metadata.Name != "operator-node-reader" {
			continue
		}
		found = true
		if len(doc.Rules) == 0 {
			t.Fatalf("operator-node-reader has no rules")
		}
		for _, r := range doc.Rules {
			for _, res := range r.Resources {
				if res != "nodes" {
					t.Errorf("operator-node-reader grants resource %q; only nodes is allowed", res)
				}
			}
			for _, v := range r.Verbs {
				lv := strings.ToLower(v)
				if writeVerbs[lv] {
					t.Errorf("operator-node-reader grants WRITE verb %q — the operator read spine must never mutate", v)
				}
				if !allowedVerbs[lv] {
					t.Errorf("operator-node-reader grants verb %q; only get/list are allowed", v)
				}
			}
		}
	}

	// Non-vacuity: a guard that finds nothing to check is a guard that passes
	// forever after the manifest is renamed or the role deleted.
	if !found {
		t.Fatalf("no ClusterRole named operator-node-reader found in %s", path)
	}
}
```

- [ ] **Step 2: Run the guard to verify it passes against the real manifest**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./test/arch/ -run TestOperatorClusterRoleIsNodeReadOnly -v`
Expected: PASS.

- [ ] **Step 3: Prove the guard is non-vacuous (mutation check)**

Temporarily add `"delete"` to the ClusterRole's `verbs` in `infra/deploy/operator-deploy.yaml`, then run the same test.
Expected: FAIL with "grants WRITE verb". Revert the mutation; re-run: PASS.

- [ ] **Step 4: Commit**

```bash
cd kanz && git add test/arch/operator_rbac_test.go
git commit -m "test(arch): operator ClusterRole is node-read-only (get/list, no writes)"
```

---

### Task 7: Universe TUI — full read-only client (source, model, poller, view)

**Files:**
- Create: `kanz/cmd/universe/config.go`
- Create: `kanz/cmd/universe/model.go`
- Create: `kanz/cmd/universe/source.go`
- Create: `kanz/cmd/universe/poller.go`
- Create: `kanz/cmd/universe/view.go`
- Test: `kanz/cmd/universe/model_test.go`
- Test: `kanz/cmd/universe/view_test.go`

**Interfaces:**
- Consumes: `operatorpb` gRPC client.
- Produces:
  - `type nodeRow struct { Name, Status, Roles, Region, Version, Age string }`
  - `type clusterRow struct { Region string; Online, Offline int }`
  - `type nodeSource interface { fetch(ctx context.Context) (fetchMsg, error) }`
  - `type fetchMsg struct { nodes []nodeRow; clusters []clusterRow; err error }`
  - `type pane int` with `paneNodes pane = 0`, `paneClusters pane = 1`
  - `type model struct { ... }`, `func newModel(cfg Config, src nodeSource) model`
  - `func (m model) render() string` (backing `View`) — needed for the package to compile, since `Update`'s `tea.Model` return type requires `model` to be a complete `tea.Model`.

- [ ] **Step 1: Write the failing model test**

Create `kanz/cmd/universe/model_test.go`:

```go
package main

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

type stubSource struct{ msg fetchMsg }

func (s stubSource) fetch(context.Context) (fetchMsg, error) { return s.msg, nil }

func TestFetchMsgPopulatesModel(t *testing.T) {
	m := newModel(Config{}, stubSource{})
	updated, _ := m.Update(fetchMsg{
		nodes: []nodeRow{{Name: "london", Status: "Ready", Region: "europe"}},
		clusters: []clusterRow{{Region: "europe", Online: 1}},
	})
	got := updated.(model)
	if len(got.nodes) != 1 || got.nodes[0].Name != "london" {
		t.Fatalf("nodes = %+v", got.nodes)
	}
	if len(got.clusters) != 1 || got.clusters[0].Region != "europe" {
		t.Fatalf("clusters = %+v", got.clusters)
	}
}

func TestTabSwitchesPane(t *testing.T) {
	m := newModel(Config{}, stubSource{})
	if m.active != paneNodes {
		t.Fatalf("initial pane = %v, want paneNodes", m.active)
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if updated.(model).active != paneClusters {
		t.Fatalf("after tab, pane = %v, want paneClusters", updated.(model).active)
	}
}

func TestFetchErrorGoesToStatusNotCrash(t *testing.T) {
	m := newModel(Config{}, stubSource{})
	updated, _ := m.Update(fetchMsg{err: context.DeadlineExceeded})
	if updated.(model).err == nil {
		t.Fatalf("expected err recorded on model")
	}
}

func TestQuitKey(t *testing.T) {
	m := newModel(Config{}, stubSource{})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatalf("expected tea.Quit command on q")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./cmd/universe/...`
Expected: FAIL — the package has no `newModel`.

- [ ] **Step 3: Write config.go**

Create `kanz/cmd/universe/config.go`:

```go
package main

import "time"

// Config is the universe TUI's read configuration. Every field is a read
// coordinate — there is no write path, no credential beyond the kubeconfig the
// operator already holds (used out-of-band by `kubectl port-forward`), and no
// mutation anywhere. The TUI dials the operator service over that port-forward.
type Config struct {
	// OperatorAddr is the operator.v1 gRPC endpoint, typically a local
	// port-forward target (see the run instructions in Task 8).
	OperatorAddr string
	// PollInterval is how often the TUI re-fetches the estate; <=0 ⇒ 3s.
	PollInterval time.Duration
}
```

- [ ] **Step 4: Write model.go**

Create `kanz/cmd/universe/model.go`:

```go
package main

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// nodeRow is one row of the Nodes pane — every field is a display string
// (Age is computed in the poller off the clock, keeping render pure).
type nodeRow struct {
	Name, Status, Roles, Region, Version, Age string
}

// clusterRow is one row of the Clusters pane.
type clusterRow struct {
	Region          string
	Online, Offline int
}

// pane selects which read-only view is shown.
type pane int

const (
	paneNodes pane = iota
	paneClusters
)

// model is the whole UI state, mutated ONLY by Update in response to messages —
// never by the poller goroutine directly (Bubble Tea's concurrency contract).
type model struct {
	cfg    Config
	src    nodeSource
	active pane

	nodes    []nodeRow
	clusters []clusterRow

	width, height int
	err           error
}

func newModel(cfg Config, src nodeSource) model {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 3 * time.Second
	}
	return model{cfg: cfg, src: src, active: paneNodes}
}

func (m model) Init() tea.Cmd { return m.pollTick() }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "tab":
			if m.active == paneNodes {
				m.active = paneClusters
			} else {
				m.active = paneNodes
			}
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case fetchMsg:
		// A failed fetch degrades the tick (err shown in the status line),
		// leaving the prior nodes/clusters intact — never a crash, never a
		// blank screen on one bad poll.
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.nodes = msg.nodes
			m.clusters = msg.clusters
			m.err = nil
		}
		return m, m.pollTick()
	}
	return m, nil
}

func (m model) View() string { return m.render() }
```

- [ ] **Step 5: Write source.go**

Create `kanz/cmd/universe/source.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	operatorpb "github.com/kanz-eng/kanz-schemas-go/operator/v1"
)

// nodeSource fetches one snapshot of the estate. The gRPC-backed impl dials the
// operator service; tests use a stub. fetch does the clock read (Age), so
// render stays a pure function of model.
type nodeSource interface {
	fetch(ctx context.Context) (fetchMsg, error)
}

// fetchMsg carries one poll cycle's result into Update.
type fetchMsg struct {
	nodes    []nodeRow
	clusters []clusterRow
	err      error
}

// grpcSource dials the operator.v1 service over a plaintext connection — the
// laptop has no SVID; the security boundary is the kubeconfig-gated
// port-forward the connection runs inside.
type grpcSource struct {
	client operatorpb.OperatorServiceClient
}

func dialOperator(addr string) (*grpcSource, func() error, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, err
	}
	return &grpcSource{client: operatorpb.NewOperatorServiceClient(conn)}, conn.Close, nil
}

func (g *grpcSource) fetch(ctx context.Context) (fetchMsg, error) {
	nodesResp, err := g.client.ListNodes(ctx, &operatorpb.ListNodesRequest{})
	if err != nil {
		return fetchMsg{}, err
	}
	clustersResp, err := g.client.ListClusters(ctx, &operatorpb.ListClustersRequest{})
	if err != nil {
		return fetchMsg{}, err
	}
	return fetchMsg{
		nodes:    toNodeRows(nodesResp.GetNodes()),
		clusters: toClusterRows(clustersResp.GetClusters()),
	}, nil
}

func toNodeRows(nodes []*operatorpb.Node) []nodeRow {
	out := make([]nodeRow, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, nodeRow{
			Name:    n.GetName(),
			Status:  statusLabel(n.GetStatus()),
			Roles:   joinRoles(n.GetRoles()),
			Region:  n.GetRegion(),
			Version: n.GetKubeletVersion(),
			Age:     age(n.GetCreatedAt().AsTime()),
		})
	}
	return out
}

func toClusterRows(clusters []*operatorpb.Cluster) []clusterRow {
	out := make([]clusterRow, 0, len(clusters))
	for _, c := range clusters {
		out = append(out, clusterRow{Region: c.GetRegion(), Online: int(c.GetOnline()), Offline: int(c.GetOffline())})
	}
	return out
}

func statusLabel(s operatorpb.NodeStatus) string {
	switch s {
	case operatorpb.NodeStatus_NODE_STATUS_READY:
		return "Ready"
	case operatorpb.NodeStatus_NODE_STATUS_NOT_READY:
		return "NotReady"
	default:
		return "Unknown"
	}
}

func joinRoles(roles []string) string {
	if len(roles) == 0 {
		return "-"
	}
	out := roles[0]
	for _, r := range roles[1:] {
		out += "," + r
	}
	return out
}

// age renders a coarse duration since t. A zero time (no created_at) ⇒ "-".
func age(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
```

- [ ] **Step 6: Write poller.go**

Create `kanz/cmd/universe/poller.go`:

```go
package main

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// pollTimeout bounds each fetch so an unreachable operator degrades the tick
// (err in the status line) rather than hanging the ticker.
const pollTimeout = 5 * time.Second

// pollTick arms a single tea.Tick that fires after cfg.PollInterval and does
// the gRPC fetch off the UI thread, returning a fetchMsg. Update re-arms it
// after every fetchMsg, so the TUI polls forever at a fixed cadence.
func (m model) pollTick() tea.Cmd {
	return tea.Tick(m.cfg.PollInterval, func(time.Time) tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
		defer cancel()
		msg, err := m.src.fetch(ctx)
		if err != nil {
			return fetchMsg{err: err}
		}
		return msg
	})
}
```

- [ ] **Step 7: Write the failing render test**

Create `kanz/cmd/universe/view_test.go`:

```go
package main

import (
	"strings"
	"testing"
)

func TestRenderNodesPaneShowsRows(t *testing.T) {
	m := model{
		active: paneNodes,
		width:  100, height: 30,
		nodes: []nodeRow{
			{Name: "london", Status: "Ready", Roles: "control-plane", Region: "europe", Version: "v1.31.3", Age: "3d"},
			{Name: "tokyo", Status: "NotReady", Roles: "-", Region: "asia", Version: "v1.31.3", Age: "3d"},
		},
	}
	out := m.render()
	for _, want := range []string{"london", "Ready", "europe", "tokyo", "NotReady"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q\n%s", want, out)
		}
	}
}

func TestRenderClustersPaneShowsCounts(t *testing.T) {
	m := model{
		active: paneClusters,
		width:  100, height: 30,
		clusters: []clusterRow{{Region: "usa", Online: 2, Offline: 1}},
	}
	out := m.render()
	for _, want := range []string{"usa", "2", "1"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q\n%s", want, out)
		}
	}
}

func TestRenderErrorShownInStatus(t *testing.T) {
	m := model{active: paneNodes, width: 80, height: 24, err: errStub{}}
	if !strings.Contains(m.render(), "boom") {
		t.Errorf("render should surface the error text\n%s", m.render())
	}
}

type errStub struct{}

func (errStub) Error() string { return "boom" }
```

- [ ] **Step 8: Run test to verify it fails to compile**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./cmd/universe/...`
Expected: FAIL to build — `m.render undefined` (and `model.View` in model.go references the missing `render`). This is why `render` lives in this task, not a later one: `model` cannot satisfy `tea.Model` (which `Update`'s return type requires) until `View`→`render` exists.

- [ ] **Step 9: Write view.go**

Create `kanz/cmd/universe/view.go`:

```go
package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var (
	styleReady  = lipgloss.NewStyle().Foreground(lipgloss.Color("2")) // green
	styleNotRdy = lipgloss.NewStyle().Foreground(lipgloss.Color("1")) // red
	styleTitle  = lipgloss.NewStyle().Bold(true)
	styleErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// render is the pure function of model that produces the whole frame — no I/O,
// no clock (Age is precomputed in the poller), so view_test.go calls it
// directly against a hand-built model.
func (m model) render() string {
	width := m.width
	if width <= 0 {
		width = 80
	}

	var body string
	switch m.active {
	case paneClusters:
		body = m.renderClusters()
	default:
		body = m.renderNodes()
	}

	return body + "\n" + m.renderStatus(width)
}

func (m model) renderNodes() string {
	var b strings.Builder
	b.WriteString(styleTitle.Render("NODES") + "\n")
	b.WriteString(fmt.Sprintf("%-16s %-9s %-16s %-10s %-10s %-6s\n",
		"NAME", "STATUS", "ROLES", "REGION", "VERSION", "AGE"))
	if len(m.nodes) == 0 {
		b.WriteString(styleDim.Render("(no nodes)") + "\n")
		return b.String()
	}
	for _, n := range m.nodes {
		st := n.Status
		if n.Status == "Ready" {
			st = styleReady.Render(n.Status)
		} else {
			st = styleNotRdy.Render(n.Status)
		}
		// Colour escapes occupy no display cells, so pad the raw value and
		// substitute the coloured span after — keeps the columns aligned.
		row := fmt.Sprintf("%-16s %-9s %-16s %-10s %-10s %-6s",
			n.Name, n.Status, n.Roles, n.Region, n.Version, n.Age)
		row = strings.Replace(row, n.Status, st, 1)
		b.WriteString(row + "\n")
	}
	return b.String()
}

func (m model) renderClusters() string {
	var b strings.Builder
	b.WriteString(styleTitle.Render("CLUSTERS") + "\n")
	b.WriteString(fmt.Sprintf("%-14s %-8s %-8s\n", "REGION", "ONLINE", "OFFLINE"))
	if len(m.clusters) == 0 {
		b.WriteString(styleDim.Render("(no clusters)") + "\n")
		return b.String()
	}
	for _, c := range m.clusters {
		b.WriteString(fmt.Sprintf("%-14s %-8d %-8d\n", c.Region, c.Online, c.Offline))
	}
	return b.String()
}

func (m model) renderStatus(width int) string {
	left := styleDim.Render("[tab] switch pane   [q] quit")
	if m.err != nil {
		return left + "   " + styleErr.Render("error: "+m.err.Error())
	}
	return left
}
```

- [ ] **Step 10: Run all TUI tests**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./cmd/universe/...`
Expected: PASS (model tests + render tests).

- [ ] **Step 11: Commit**

```bash
cd kanz && git add cmd/universe/config.go cmd/universe/model.go cmd/universe/source.go cmd/universe/poller.go cmd/universe/view.go cmd/universe/model_test.go cmd/universe/view_test.go
git commit -m "feat(universe): read-only nodes + clusters panes over operator.v1"
```

---

### Task 8: Universe main wiring + full verification + rig proof

**Files:**
- Create: `kanz/cmd/universe/main.go`

**Interfaces:**
- Consumes: `Config`, `newModel`, `nodeSource`, `dialOperator` from Task 7.
- Produces: `main()` + `run()` wiring; local `envOr` helper.

- [ ] **Step 1: Write main.go**

Create `kanz/cmd/universe/main.go`:

```go
// universe is the read-only operator TUI (F0+S1): it lists the Kubernetes
// estate (nodes, clusters) by dialing the in-cluster operator.v1 service over a
// kubeconfig-gated `kubectl port-forward`. Read-only — no add/edit/delete/ssh.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "universe: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	var cfg Config
	fs := flag.NewFlagSet("universe", flag.ContinueOnError)
	fs.StringVar(&cfg.OperatorAddr, "operator-addr", envOr("KANZ_OPERATOR_ADDR", "localhost:9090"),
		"operator.v1 gRPC address (typically a `kubectl port-forward` target)")
	fs.DurationVar(&cfg.PollInterval, "poll", 3*time.Second, "estate refresh interval")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	src, closeConn, err := dialOperator(cfg.OperatorAddr)
	if err != nil {
		return err
	}
	defer func() { _ = closeConn() }()

	p := tea.NewProgram(newModel(cfg, src), tea.WithAltScreen())
	_, err = p.Run()
	return err
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
```

- [ ] **Step 2: Build and vet the whole slice**

Run:
```bash
cd kanz && GOFLAGS=-mod=mod go build ./... && GOFLAGS=-mod=mod go vet ./cmd/universe/... ./services/operator/... && gofmt -l cmd/universe services/operator
```
Expected: exit 0; `gofmt -l` prints nothing.

- [ ] **Step 3: Run the full arch suite (guards + SSH-plane regression check)**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./test/arch/...`
Expected: PASS — including `ssh_plane_test.go` (no `crypto/ssh` introduced), `deployability_test.go` (operator deployable), and the new `operator_rbac_test.go`.

- [ ] **Step 4: Commit**

```bash
cd kanz && git add cmd/universe/main.go
git commit -m "feat(universe): main wiring — dial operator.v1 and run the TUI"
```

- [ ] **Step 5: Rig proof (manual — the definition of done)**

This proves the end-to-end spine against the dev kind rig. Run by the operator (needs kubeconfig access to the rig):

1. Build + load the image (tagged as the manifest's fully-qualified `ghcr.io/kanz-eng/operator:latest`) and apply the manifest:
   ```bash
   docker build -f kanz/services/operator/Dockerfile -t ghcr.io/kanz-eng/operator:latest .
   # confirm the kind cluster name first: kind get clusters
   kind load docker-image ghcr.io/kanz-eng/operator:latest --name kanz-dryrun
   kubectl apply -f kanz/infra/deploy/operator-deploy.yaml
   # `:latest` defaults to imagePullPolicy: Always, which would try to pull from ghcr and fail in the
   # offline rig. For the LOCAL rig only, use the kind-loaded image instead of pulling:
   kubectl -n kanz-operator patch deploy/operator --type=json \
     -p='[{"op":"add","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]'
   kubectl -n kanz-operator rollout status deploy/operator
   ```
2. Port-forward the gRPC port (this is the kubeconfig-gated tunnel):
   ```bash
   kubectl -n kanz-operator port-forward deploy/operator 9090:9090
   ```
3. In another terminal, run the TUI:
   ```bash
   cd kanz && GOFLAGS=-mod=mod go run ./cmd/universe -operator-addr localhost:9090
   ```
   Expected: the Nodes pane lists the rig's real nodes with Ready/NotReady status; `[tab]` switches to the Clusters pane showing region groupings; `[q]` quits. A stopped port-forward shows `error: ...` in the status line without crashing.

Record the outcome (pods Running, TUI rendered the real estate) on the KANZ_TASKS.md board row for this feature.

---

## Notes for the executor

- **Task order matters:** Tasks 1→6 build the operator service bottom-up (proto → read model → adapter → entrypoint → deploy → guard); Task 7 builds the entire TUI (source, model, poller, view) so the package compiles and its unit tests pass on their own; Task 8 is only `main` wiring plus whole-slice verification and the manual rig proof. `render` lives in Task 7 because `model` cannot satisfy `tea.Model` — required by `Update`'s return type — until `View`→`render` exists.
- **Generated code is not committed.** Anyone building fresh must run `make generate` first (Task 1, Step 3) or the `operatorpb` import will not resolve.
- **Deferred by design (do NOT add here):** CPU/RAM columns (need metrics-server), Profit column (needs funds), any write/provisioning/SSH/Vault path, the setup wizard, and the operator service's SVID/mesh wiring (only needed when it talks to *other* mesh services, in S2+). See the spec's out-of-scope list.
