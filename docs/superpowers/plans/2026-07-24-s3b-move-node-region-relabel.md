# S3b — Move Node (region relabel) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the operator move a node between region groupings from the Universe TUI by relabeling its `topology.kubernetes.io/region` — a safe, instant, non-destructive `nodes: patch`.

**Architecture:** A new `operator.v1.SetNodeRegion` RPC; the operator patches the node's region label (via `json.Marshal`, so an odd value can't break the patch). It reuses S3a's `operator-node-writer` ClusterRole (`nodes: patch` already covers relabel — no new RBAC, no guard change) and the `nodeWrite` handler guard. The TUI adds an `m` key that opens a free-form region input.

**Tech Stack:** Go, `operator.v1` (buf), `k8s.io/client-go` (node Patch), `github.com/charmbracelet/bubbletea`.

## Global Constraints

Every task's requirements implicitly include this section.

- **Go toolchain** `go 1.26.1` / `toolchain go1.26.5`; all `go` commands from `kanz/` with `GOFLAGS=-mod=mod`. **No `make`** — use `buf generate`; set **`GOTMPDIR="$(pwd)/.gotmp"`** for every `go test`.
- Generated protobuf imports from `github.com/kanz-eng/kanz-schemas-go/operator/v1`; regenerate with `cd kanz-schemas && buf generate`.
- **No new RBAC, no guard change.** A region relabel is a `nodes: patch`, which the existing `operator-node-writer` ClusterRole already grants and its arch guard already permits. Do NOT touch `infra/deploy/operator-deploy.yaml` or the RBAC arch tests.
- **No `crypto/ssh` in the operator** — a k8s patch. `TestNoSSHPlane` stays green.
- **A relabel is non-destructive** — no cordon, no eviction, so **no confirm prompt** (unlike Drain). Empty name/region → `codes.InvalidArgument`.
- **Reuse, don't redefine:** the region label key lives in `estate` (F0) — export and reuse it, do not add a second copy of the magic string. Build the patch with `json.Marshal`, not string formatting.
- **`render()` stays pure.** `govulncheck ./...` stays 0-reachable.

---

### Task 1: `operator.v1` SetNodeRegion proto + SDK gen

**Files:**
- Modify: `kanz-schemas/proto/operator/v1/operator.proto`
- Regenerates (not committed): `operator.pb.go`, `operator_grpc.pb.go`

**Interfaces:**
- Produces: `SetNodeRegion` RPC; `SetNodeRegionRequest{name, region}`, `SetNodeRegionResponse{}`.

- [ ] **Step 1: Add the RPC + messages**

In the `service OperatorService { ... }` block add:

```proto
  // SetNodeRegion relabels a node's topology.kubernetes.io/region — the "Move Node"
  // action: it moves the node between the region groupings the Clusters view shows.
  // Non-destructive (relabel only, no eviction). Poll ListNodes/ListClusters for the
  // new grouping.
  rpc SetNodeRegion(SetNodeRegionRequest) returns (SetNodeRegionResponse);
```

Append the messages:

```proto
message SetNodeRegionRequest {
  string name = 1;
  string region = 2;
}
message SetNodeRegionResponse {}
```

- [ ] **Step 2: Lint + generate**

Run: `cd kanz-schemas && buf lint && buf generate`
Expected: lint exit 0; `SetNodeRegion` + `SetNodeRegionRequest/Response` appear in the generated code.

- [ ] **Step 3: Verify types compile**

Run: `cd kanz && GOFLAGS=-mod=mod go build github.com/kanz-eng/kanz-schemas-go/operator/v1`
Expected: exit 0.

- [ ] **Step 4: Commit**

```bash
cd kanz-schemas && git add proto/operator/v1/operator.proto
git commit -m "feat(schemas): operator.v1 SetNodeRegion (Move Node = region relabel)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: Export the region label + `nodeops.SetRegion`

**Files:**
- Modify: `kanz/services/operator/internal/estate/estate.go` (export `RegionLabel`)
- Modify: `kanz/services/operator/internal/nodeops/nodeops.go` (add `SetRegion`)
- Test: `kanz/services/operator/internal/nodeops/nodeops_test.go`

**Interfaces:**
- Produces: `estate.RegionLabel` (exported const); `func (o *Ops) SetRegion(ctx, name, region string) error`.

- [ ] **Step 1: Export the region-label constant from estate**

In `estate.go`, the unexported `regionLabel = "topology.kubernetes.io/region"` is used by `mapNode`. Rename it to the exported `RegionLabel` and update its use(s) in `estate.go` (a mechanical rename — grep `regionLabel` in the estate package; there are ~1–2 references). This makes it a single source of truth shared by the read model and the write path. No behavior change.

- [ ] **Step 2: Write the failing test**

Add to `nodeops_test.go`:

```go
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
```

Add `"github.com/kanz-eng/kanz/services/operator/internal/estate"` to the test imports if not present. (The existing `node`/`ops` helpers are already in this test file from S3a.)

- [ ] **Step 3: Run — verify it fails**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./services/operator/internal/nodeops/... -run TestSetRegion`
Expected: FAIL — `SetRegion` undefined.

- [ ] **Step 4: Implement `SetRegion`**

In `nodeops.go`, add (imports: `encoding/json`, and `estate` for `RegionLabel`):

```go
// SetRegion relabels a node's topology.kubernetes.io/region (the "Move Node" action).
// A strategic-merge patch on labels — non-destructive (no cordon, no eviction). The
// patch is json.Marshal'd so an odd region value cannot break it; Kubernetes validates
// the label value server-side (an invalid value returns an error).
func (o *Ops) SetRegion(ctx context.Context, name, region string) error {
	if name == "" || region == "" {
		return fmt.Errorf("node name and region are required")
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"labels": map[string]string{estate.RegionLabel: region},
		},
	})
	if err != nil {
		return err
	}
	_, err = o.cs.CoreV1().Nodes().Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	return err // apierrors.IsNotFound(err) for an unknown node
}
```

- [ ] **Step 5: Run — verify it passes**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./services/operator/internal/estate/... ./services/operator/internal/nodeops/...`
Expected: PASS (estate still passes after the rename; the three new SetRegion tests pass; existing nodeops tests pass).

- [ ] **Step 6: Commit**

```bash
cd kanz && git add services/operator/internal/estate/ services/operator/internal/nodeops/
git commit -m "feat(operator): nodeops.SetRegion (relabel) + export estate.RegionLabel

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: Operator gRPC — SetNodeRegion handler

**Files:**
- Modify: `kanz/services/operator/internal/grpcsrv/server.go`
- Test: `kanz/services/operator/internal/grpcsrv/server_test.go`

**Interfaces:**
- Consumes: the S3a `NodeOps` interface (add `SetRegion`), the `nodeWrite` guard helper.
- Produces: `(*Server).SetNodeRegion` handler.

- [ ] **Step 1: Write the failing test**

Add to `server_test.go` (the `stubNodeOps` from S3a gains a `SetRegion` method):

```go
func (s *stubNodeOps) SetRegion(_ context.Context, name, region string) error {
	s.regionNode, s.region = name, region
	return s.err
}

func TestSetNodeRegionDelegates(t *testing.T) {
	sn := &stubNodeOps{}
	srv := New(stubReader{}).WithNodeOps(sn)
	if _, err := srv.SetNodeRegion(context.Background(), &operatorpb.SetNodeRegionRequest{Name: "london", Region: "asia"}); err != nil {
		t.Fatalf("SetNodeRegion: %v", err)
	}
	if sn.regionNode != "london" || sn.region != "asia" {
		t.Errorf("delegation failed: node=%q region=%q", sn.regionNode, sn.region)
	}
}

func TestSetNodeRegionRejectsEmpty(t *testing.T) {
	srv := New(stubReader{}).WithNodeOps(&stubNodeOps{})
	if _, err := srv.SetNodeRegion(context.Background(), &operatorpb.SetNodeRegionRequest{Region: "asia"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty name → want InvalidArgument, got %v", err)
	}
	if _, err := srv.SetNodeRegion(context.Background(), &operatorpb.SetNodeRegionRequest{Name: "london"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty region → want InvalidArgument, got %v", err)
	}
}

func TestSetNodeRegionUnconfiguredIsUnimplemented(t *testing.T) {
	if _, err := New(stubReader{}).SetNodeRegion(context.Background(), &operatorpb.SetNodeRegionRequest{Name: "x", Region: "y"}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("want Unimplemented, got %v", err)
	}
}
```

Add the two fields to the S3a `stubNodeOps` struct: `regionNode, region string`.

- [ ] **Step 2: Run — verify it fails**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./services/operator/internal/grpcsrv/... -run SetNodeRegion`
Expected: FAIL — `SetRegion` not on the `NodeOps` interface / `SetNodeRegion` handler undefined.

- [ ] **Step 3: Implement**

In `server.go`, add `SetRegion(ctx context.Context, name, region string) error` to the `NodeOps` interface. Add the handler, reusing the S3a `nodeWrite` guard with an extra empty-region check placed so a nil-ops deployment still reports Unimplemented (not the region error):

```go
func (s *Server) SetNodeRegion(ctx context.Context, req *operatorpb.SetNodeRegionRequest) (*operatorpb.SetNodeRegionResponse, error) {
	// The region check only applies once we know ops is configured, so an unconfigured
	// deployment still returns Unimplemented (via nodeWrite) rather than InvalidArgument.
	if s.nodeOps != nil && req.GetRegion() == "" {
		return nil, status.Error(codes.InvalidArgument, "region is required")
	}
	if err := s.nodeWrite(ctx, req.GetName(), func() error {
		return s.nodeOps.SetRegion(ctx, req.GetName(), req.GetRegion())
	}); err != nil {
		return nil, err
	}
	return &operatorpb.SetNodeRegionResponse{}, nil
}
```

- [ ] **Step 4: Run — verify it passes**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./services/operator/...`
Expected: PASS (new + existing). `nodeops.Ops` now satisfies the widened `NodeOps` interface (it has `SetRegion` from Task 2) — no main.go change needed.

- [ ] **Step 5: Commit**

```bash
cd kanz && git add services/operator/internal/grpcsrv/
git commit -m "feat(operator): SetNodeRegion gRPC handler (Move Node)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: TUI — `m` key + region input

**Files:**
- Modify: `kanz/cmd/universe/source.go` (widen `nodeSource` with `setRegion`)
- Modify: `kanz/cmd/universe/model.go` (move-input mode + cmd)
- Modify: `kanz/cmd/universe/view.go` (render the region input)
- Test: `kanz/cmd/universe/model_test.go`

**Interfaces:**
- Consumes: `operatorpb.SetNodeRegion*`.
- Produces: `nodeSource.setRegion(ctx, name, region string) error`; `model.movingRegion bool` + `moveInput string`.

- [ ] **Step 1: Widen the source (update ALL implementers)**

In `source.go`, add to `nodeSource`:

```go
	setRegion(ctx context.Context, name, region string) error
```

Add the `grpcSource` impl:

```go
func (g *grpcSource) setRegion(ctx context.Context, name, region string) error {
	_, err := g.client.SetNodeRegion(ctx, &operatorpb.SetNodeRegionRequest{Name: name, Region: region})
	return err
}
```

Add a no-op `setRegion` to the test `stubSource` in `model_test.go` (grep for every `nodeSource` implementer):

```go
func (s stubSource) setRegion(context.Context, string, string) error { return nil }
```

- [ ] **Step 2: Write the failing test**

Add to `model_test.go`:

```go
func TestMKeyOpensRegionInput(t *testing.T) {
	m := newModel(Config{}, stubSource{})
	m.active = paneNodes
	m.nodes = []nodeRow{{Name: "london", schedulable: true}}
	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	if cmd != nil {
		t.Fatalf("m should open the region input, not fire immediately")
	}
	if !u.(model).movingRegion {
		t.Fatalf("m should enter the move-input state")
	}
}

func TestRegionInputTypesAndSubmits(t *testing.T) {
	m := newModel(Config{}, stubSource{})
	m.active = paneNodes
	m.nodes = []nodeRow{{Name: "london", schedulable: true}}
	m.movingRegion = true
	// type "asia"
	for _, r := range "asia" {
		u, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = u.(model)
	}
	if m.moveInput != "asia" {
		t.Fatalf("move input = %q, want asia", m.moveInput)
	}
	// enter fires the command
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatalf("enter should fire the SetNodeRegion command")
	}
}

func TestRegionInputEscCancels(t *testing.T) {
	m := newModel(Config{}, stubSource{})
	m.active = paneNodes
	m.nodes = []nodeRow{{Name: "london", schedulable: true}}
	m.movingRegion = true
	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if u.(model).movingRegion {
		t.Fatalf("esc should cancel the move input")
	}
}
```

- [ ] **Step 3: Run — verify it fails**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./cmd/universe/... -run 'MKey|RegionInput'`
Expected: FAIL — `movingRegion`/`moveInput` not present.

- [ ] **Step 4: Implement the model**

In `model.go`:
- Add fields `movingRegion bool` + `moveInput string`.
- In the nodes-pane key handling (not in a form/confirm/move), add an `m` case: if there is a selected node, set `movingRegion = true`, `moveInput = ""`. (Guard against empty nodes like the c/u/d keys — use the existing `selectedNodeName()` guard.)
- Add a move-input branch (checked BEFORE the general nodes-pane keys while `m.movingRegion`): `enter` → capture the selected node name, clear `movingRegion`, and return `m.moveNodeCmd(name, m.moveInput)`; `esc` → clear `movingRegion`; `backspace` → trim `moveInput`; `tea.KeyRunes` → append to `moveInput`.
- Add the command (reuses the S3a `nodeActionMsg`):

```go
func (m model) moveNodeCmd(name, region string) tea.Cmd {
	src := m.src
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
		defer cancel()
		return nodeActionMsg{err: src.setRegion(ctx, name, region)}
	}
}
```

(`nodeActionMsg` + its handler already exist from S3a; a failed relabel sets `actionErr`, the node's new Region arrives on the next poll.)

- [ ] **Step 5: Render the region input**

In `view.go`'s nodes-pane render, when `m.movingRegion`, render a prompt line (below the table) like `Move <selected node> to region: <moveInput>_`, and add `[m]ove` to the key-hint footer. Keep `render` pure. Use the existing `selectedNodeName()`/guarded accessor for the node name.

- [ ] **Step 6: Run — verify it passes**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./cmd/universe/... && GOFLAGS=-mod=mod go build ./cmd/universe/... && gofmt -l cmd/universe`
Expected: tests PASS (new + existing); build exit 0; gofmt clean.

- [ ] **Step 7: Commit**

```bash
cd kanz && git add cmd/universe/
git commit -m "feat(universe): m key — Move Node (region relabel input)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 5: Whole-slice verification + rig-proof handoff

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
Expected: PASS — including `TestNoSSHPlane` (a k8s patch, no `crypto/ssh`) and the UNCHANGED RBAC guards (`operator-node-writer` still passes — relabel uses the already-granted `nodes: patch`; no manifest change). If the writer guard fails, someone wrongly touched the RBAC — S3b needs none.

- [ ] **Step 3: govulncheck**

Run: `cd kanz && GOFLAGS=-mod=mod go run golang.org/x/vuln/cmd/govulncheck@latest ./...`
Expected: 0 reachable (2 known-unreachable advisories acceptable). If any reachable vuln appears, STOP and report.

- [ ] **Step 4: Rig proof (manual — FULLY provable on the kind rig)**

Relabel is a real k8s patch on the kind rig:
```bash
kubectl -n kanz-operator port-forward deploy/operator 9090:9090
# in universe: Nodes pane → select a node → m → type a region (e.g. "asia") → enter
#   next poll: the node's Region shows "asia" and it moves to the asia grouping in Clusters
#   confirm: kubectl get node <name> -o jsonpath='{.metadata.labels.topology\.kubernetes\.io/region}' → asia
```

- [ ] **Step 5: Board update (controller)** — add the S3b readiness row (fully rig-provable; no new RBAC — reused the writer role), note the mockup's Cluster Management is now complete (Maintenance + Drain + Move; Balance dropped). Validate with `bash tools/validate-board.sh KANZ_TASKS.md`, commit, push.

---

## Notes for the executor

- **No RBAC/guard task by design:** the S3a `operator-node-writer` role's `nodes: patch` already covers a label relabel. Do NOT add a role or touch the RBAC arch guards; Task 5 Step 2 re-asserts the writer guard still passes unchanged.
- **Reuse over redefine:** `estate.RegionLabel` (exported in Task 2) is the single source of truth for the region label key — nodeops uses it, not a second copy.
- **Interface-widening (Tasks 3, 4):** `NodeOps` gains `SetRegion`; `nodeSource` gains `setRegion` — every implementer (real + test stub) must gain them or the package won't compile. `nodeops.Ops` already satisfies the widened `NodeOps` once Task 2's `SetRegion` exists.
- **No confirm for Move:** a relabel is non-destructive — it fires on `enter`, unlike Drain's `y/n`.
- **Git hygiene:** never `git add -A` — stage explicit paths (untracked `universe.exe`/`kanz-monitor.exe`/`kanz-py/gen/` must not be committed).
