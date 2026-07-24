# S2b — Test Connection + node-exporter DaemonSet Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a pre-flight "Test Connection" reachability probe to the Add Node flow, and make every joined node observable via a cluster-wide node-exporter DaemonSet — without adding any per-node SSH step.

**Architecture:** A new `operator.v1.TestConnection` RPC has the operator service `net.Dial` the target's SSH port (a plain TCP probe — no `crypto/ssh`); the `universe` Add Node form gets a `ctrl+t` key that shows the result inline. A standard node-exporter DaemonSet in `kanz-observability` scrapes every node automatically.

**Tech Stack:** Go, `operator.v1` (buf), `net` (TCP dial), `github.com/charmbracelet/bubbletea`; a Prometheus node-exporter DaemonSet manifest.

## Global Constraints

Every task's requirements implicitly include this section.

- **Go toolchain** `go 1.26.1` / `toolchain go1.26.5`; all `go` commands from `kanz/` with `GOFLAGS=-mod=mod`. **No `make`** — use `buf generate`; set **`GOTMPDIR="$(pwd)/.gotmp"`** for every `go test`.
- Generated protobuf imports from `github.com/kanz-eng/kanz-schemas-go/operator/v1`; regenerate with `cd kanz-schemas && buf generate`.
- **The operator service must NOT import `golang.org/x/crypto/ssh` (or any `ssh` package).** Test Connection is a plain TCP `net.Dial`. `crypto/ssh` stays confined to `cmd/kanz-provisioner` — the path-scoped `test/arch/ssh_plane_test.go` guard (`TestNoSSHPlane`) MUST stay green. A regression means someone reached for an SSH library for the probe.
- **Test Connection is reachability only** — it verifies the TCP port is open + latency, NOT that the SSH key authenticates (that happens at provision time). It returns `reachable=false` as a normal (non-error) result; only an empty `ip` is `codes.InvalidArgument`.
- **`render()` stays pure** — the Test Connection result is a `model` field set by an off-thread message handler, never computed in render.
- **The private key is not involved in Test Connection.**
- `govulncheck ./...` stays 0-reachable. Namespace for observability infra is **`kanz-observability`** (already used in the repo).

---

### Task 1: `operator.v1` TestConnection proto + SDK gen

**Files:**
- Modify: `kanz-schemas/proto/operator/v1/operator.proto`
- Regenerates (not committed): `operator.pb.go`, `operator_grpc.pb.go`

**Interfaces:**
- Produces: `TestConnection` RPC on `OperatorServiceServer`/`Client`; `TestConnectionRequest{ip, ssh_port}`, `TestConnectionResponse{reachable, latency_ms, message}`.

- [ ] **Step 1: Add the RPC + messages**

In the `service OperatorService { ... }` block add:

```proto
  // TestConnection is a pre-flight TCP reachability probe of a candidate host's
  // SSH port — it does NOT authenticate (the key is validated at provision time).
  rpc TestConnection(TestConnectionRequest) returns (TestConnectionResponse);
```

Append the messages:

```proto
message TestConnectionRequest {
  string ip = 1;
  int32 ssh_port = 2;  // 0 ⇒ 22
}

message TestConnectionResponse {
  bool reachable = 1;
  int64 latency_ms = 2;  // dial latency when reachable; 0 otherwise
  string message = 3;    // dial error when unreachable; empty when reachable
}
```

- [ ] **Step 2: Lint + generate**

Run: `cd kanz-schemas && buf lint && buf generate`
Expected: lint exit 0; `gen/go/operator/v1/operator.pb.go` + `operator_grpc.pb.go` now expose `TestConnection`, `TestConnectionRequest/Response`.

- [ ] **Step 3: Verify the types compile**

Run: `cd kanz && GOFLAGS=-mod=mod go build github.com/kanz-eng/kanz-schemas-go/operator/v1`
Expected: exit 0.

- [ ] **Step 4: Commit**

```bash
cd kanz-schemas && git add proto/operator/v1/operator.proto
git commit -m "feat(schemas): operator.v1 TestConnection (pre-flight reachability probe)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: Operator TestConnection handler (TCP dial, no crypto/ssh)

**Files:**
- Modify: `kanz/services/operator/internal/grpcsrv/server.go`
- Test: `kanz/services/operator/internal/grpcsrv/server_test.go`

**Interfaces:**
- Produces: `(*Server).TestConnection` — available on BOTH `New` and `NewWithProvisioner` servers (a probe needs no provisioner).

- [ ] **Step 1: Write the failing test**

Add to `server_test.go`:

```go
func TestTestConnectionReachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	srv := New(stubReader{})
	resp, err := srv.TestConnection(context.Background(), &operatorpb.TestConnectionRequest{Ip: host, SshPort: int32(port)})
	if err != nil {
		t.Fatalf("TestConnection: %v", err)
	}
	if !resp.GetReachable() {
		t.Errorf("want reachable, got %+v", resp)
	}
	if resp.GetLatencyMs() < 0 {
		t.Errorf("latency should be non-negative, got %d", resp.GetLatencyMs())
	}
}

func TestTestConnectionUnreachable(t *testing.T) {
	// Bind then immediately close, so the port is (almost certainly) closed.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close()
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	resp, err := New(stubReader{}).TestConnection(context.Background(), &operatorpb.TestConnectionRequest{Ip: host, SshPort: int32(port)})
	if err != nil {
		t.Fatalf("TestConnection returned an RPC error for an unreachable host (should be a normal result): %v", err)
	}
	if resp.GetReachable() {
		t.Errorf("want unreachable")
	}
	if resp.GetMessage() == "" {
		t.Errorf("unreachable result should carry a message")
	}
}

func TestTestConnectionRejectsEmptyIP(t *testing.T) {
	_, err := New(stubReader{}).TestConnection(context.Background(), &operatorpb.TestConnectionRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for empty ip, got %v", err)
	}
}
```

Add imports to the test file if missing: `"net"`, `"strconv"`.

- [ ] **Step 2: Run — verify it fails**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./services/operator/internal/grpcsrv/... -run TestTestConnection`
Expected: FAIL — `TestConnection` undefined.

- [ ] **Step 3: Implement the handler**

In `server.go`, add (and imports `net`, `strconv`, `time`):

```go
// testDialTimeout bounds the reachability probe.
const testDialTimeout = 5 * time.Second

// TestConnection is a pre-flight TCP reachability probe. It is a plain net.Dial —
// deliberately NOT crypto/ssh — so the operator never imports the SSH plane; the
// key is authenticated at provision time, not here. reachable=false is a normal
// result, not an RPC error.
//
// The caller (root@universe, reaching the operator only via a kubeconfig-gated
// port-forward) can make the operator dial an arbitrary ip:port. Accepted: the
// caller could already reach anything a cluster operator can, the probe returns
// only reachable/latency (never response bytes), and the dial is timeout-bounded.
func (s *Server) TestConnection(ctx context.Context, req *operatorpb.TestConnectionRequest) (*operatorpb.TestConnectionResponse, error) {
	if req.GetIp() == "" {
		return nil, status.Error(codes.InvalidArgument, "ip is required")
	}
	port := req.GetSshPort()
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(req.GetIp(), strconv.Itoa(int(port)))

	start := time.Now()
	d := net.Dialer{Timeout: testDialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return &operatorpb.TestConnectionResponse{Reachable: false, Message: dialMessage(err)}, nil
	}
	_ = conn.Close()
	return &operatorpb.TestConnectionResponse{Reachable: true, LatencyMs: time.Since(start).Milliseconds()}, nil
}

// dialMessage trims a dial error to a short, client-safe reason.
func dialMessage(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 && i+2 < len(msg) {
		return msg[i+2:]
	}
	return msg
}
```

`strings` is already imported by `server.go` (used elsewhere); if not, add it.

- [ ] **Step 4: Run — verify it passes**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./services/operator/internal/grpcsrv/...`
Expected: PASS (the three new tests + all existing grpcsrv tests).

- [ ] **Step 5: Confirm the operator did NOT gain crypto/ssh**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./test/arch/ -run TestNoSSHPlane`
Expected: PASS — the operator uses `net`, not `crypto/ssh`. If this FAILS, an SSH library was wrongly used for the probe; revert to `net.Dial`.

- [ ] **Step 6: Commit**

```bash
cd kanz && git add services/operator/internal/grpcsrv/
git commit -m "feat(operator): TestConnection — TCP reachability probe (no crypto/ssh)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: TUI — `ctrl+t` Test Connection + inline result

**Files:**
- Modify: `kanz/cmd/universe/source.go` (widen `nodeSource` with `testConnection`)
- Modify: `kanz/cmd/universe/model.go` (ctrl+t handler + cmd + msg + result field)
- Modify: `kanz/cmd/universe/view.go` (render the result on the form)
- Test: `kanz/cmd/universe/form_test.go` (or model_test.go)

**Interfaces:**
- Consumes: `operatorpb.TestConnection*`.
- Produces: `nodeSource.testConnection(ctx, ip string, port int32) (testConnResult, error)`; `type testConnResult struct { reachable bool; latencyMs int64; message string }`.

- [ ] **Step 1: Widen the source (and update ALL implementers)**

In `source.go`, add to the `nodeSource` interface:

```go
	testConnection(ctx context.Context, ip string, port int32) (testConnResult, error)
```

Add the type + the `grpcSource` impl:

```go
type testConnResult struct {
	reachable bool
	latencyMs int64
	message   string
}

func (g *grpcSource) testConnection(ctx context.Context, ip string, port int32) (testConnResult, error) {
	resp, err := g.client.TestConnection(ctx, &operatorpb.TestConnectionRequest{Ip: ip, SshPort: port})
	if err != nil {
		return testConnResult{}, err
	}
	return testConnResult{reachable: resp.GetReachable(), latencyMs: resp.GetLatencyMs(), message: resp.GetMessage()}, nil
}
```

Update the test `stubSource` (in `model_test.go`) to satisfy the widened interface:

```go
func (s stubSource) testConnection(context.Context, string, int32) (testConnResult, error) {
	return testConnResult{reachable: true, latencyMs: 7}, nil
}
```

(Grep `func (s stubSource)` / any other `nodeSource` implementer and add the method to each, or the package won't compile.)

- [ ] **Step 2: Write the failing test**

Add to `form_test.go`:

```go
func TestCtrlTFiresTestConnection(t *testing.T) {
	m := newModel(Config{}, stubSource{})
	m.showForm = true
	m.form = newAddForm()
	// focus/complete the ip field then press ctrl+t
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	if cmd == nil {
		t.Fatal("ctrl+t should return a test-connection command")
	}
}

func TestTestConnResultRenders(t *testing.T) {
	m := model{showForm: true, form: newAddForm(), testResult: "✓ reachable (7ms)"}
	if !strings.Contains(m.render(), "reachable (7ms)") {
		t.Errorf("form render should show the test result:\n%s", m.render())
	}
}
```

- [ ] **Step 3: Run — verify it fails**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./cmd/universe/... -run 'CtrlT|TestConnResult'`
Expected: FAIL — `testResult`/`KeyCtrlT` handling not present.

- [ ] **Step 4: Implement the model handling**

In `model.go`: add a `testResult string` field to `model`. In the form-mode `KeyMsg` handling (the branch that runs while `m.showForm`), add a case for `ctrl+t` that fires the probe, and reset `testResult` when the form opens (the `a` handler / `newAddForm`). Add the command + message:

```go
// testConnCmd probes the form's ip:port off the UI thread.
func (m model) testConnCmd() tea.Cmd {
	ip := m.form.value("ip")
	port := atoi32(m.form.value("ssh_port"))
	src := m.src
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
		defer cancel()
		res, err := src.testConnection(ctx, ip, port)
		return testConnResultMsg{res: res, err: err}
	}
}

type testConnResultMsg struct {
	res testConnResult
	err error
}
```

In `Update`, while `showForm`: `case "ctrl+t": return m, m.testConnCmd()`. And handle the message (outside the form key switch, alongside the other msg cases):

```go
	case testConnResultMsg:
		switch {
		case msg.err != nil:
			m.testResult = "✗ test failed: " + msg.err.Error()
		case msg.res.reachable:
			m.testResult = fmt.Sprintf("✓ reachable (%dms)", msg.res.latencyMs)
		default:
			m.testResult = "✗ unreachable: " + msg.res.message
		}
		return m, nil
```

(`fmt`, `context` are already imported by model.go from Task 8's submit path.)

- [ ] **Step 5: Render the result + hint**

In `view.go`'s `if m.showForm` branch, append the test result under the form (below any `formErr`), and add a `ctrl+t` hint. For example, after the form render:

```go
	if m.showForm {
		out := m.form.render()
		if m.testResult != "" {
			out += "\n" + m.testResult
		}
		if m.formErr != nil {
			out += "\n" + styleErr.Render("error: "+m.formErr.Error())
		}
		return out
	}
```

And add `[ctrl+t] test` to the form's key hint line (in `form.go`'s `render`, alongside `[tab] next  [enter] save  [esc] cancel`).

- [ ] **Step 6: Run — verify it passes**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./cmd/universe/...`
Expected: PASS (new ctrl+t / render tests + all existing universe tests). Then `GOFLAGS=-mod=mod go build ./cmd/universe/...` and `gofmt -l cmd/universe` — clean.

- [ ] **Step 7: Commit**

```bash
cd kanz && git add cmd/universe/
git commit -m "feat(universe): ctrl+t Test Connection on the Add Node form

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: node-exporter DaemonSet + a parse guard

**Files:**
- Create: `kanz/infra/observability/node-exporter.yaml`
- Test: `kanz/test/arch/node_exporter_test.go`

**Interfaces:**
- Consumes: `moduleRoot`, `gopkg.in/yaml.v3` (existing patterns).

- [ ] **Step 1: Write the DaemonSet manifest**

Create `kanz/infra/observability/node-exporter.yaml`:

```yaml
# Cluster-native node observability (S2b): a Prometheus node-exporter on every node,
# so a node S2a provisions is scraped automatically — no per-node SSH step. Applied
# once. Wiring a Prometheus to scrape port 9100 (via the annotations below) is infra
# and out of this slice's scope.
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: node-exporter
  namespace: kanz-observability
  labels: { app.kubernetes.io/part-of: kanz, app: node-exporter }
spec:
  selector:
    matchLabels: { app: node-exporter }
  template:
    metadata:
      labels: { app: node-exporter, app.kubernetes.io/part-of: kanz }
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "9100"
    spec:
      hostNetwork: true
      hostPID: true
      # Survive node pressure (evicting the metrics agent loses metrics when they
      # matter most). system-node-critical is a built-in priority class.
      priorityClassName: system-node-critical
      # Run on EVERY node, including tainted/control-plane ones.
      tolerations:
        - operator: Exists
      securityContext:
        runAsNonRoot: true
        runAsUser: 65534
        seccompProfile: { type: RuntimeDefault }
      containers:
        - name: node-exporter
          image: quay.io/prometheus/node-exporter:v1.8.2
          args:
            - --path.procfs=/host/proc
            - --path.sysfs=/host/sys
            - --path.rootfs=/host/root
            - --web.listen-address=:9100
          ports:
            - { name: metrics, containerPort: 9100, hostPort: 9100 }
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: { drop: ["ALL"] }
          resources:
            requests: { cpu: 50m, memory: 30Mi }
            limits: { memory: 60Mi }
          volumeMounts:
            - { name: proc, mountPath: /host/proc, readOnly: true }
            - { name: sys, mountPath: /host/sys, readOnly: true }
            - { name: root, mountPath: /host/root, mountPropagation: HostToContainer, readOnly: true }
      volumes:
        - { name: proc, hostPath: { path: /proc } }
        - { name: sys, hostPath: { path: /sys } }
        - { name: root, hostPath: { path: / } }
```

First confirm the namespace exists in-repo: `grep -rn "name: kanz-observability" kanz/infra/`. If `kanz-observability` is NOT declared by any manifest, prepend a `Namespace` doc to this file (`apiVersion: v1 / kind: Namespace / metadata: {name: kanz-observability, labels: {app.kubernetes.io/part-of: kanz}}`).

- [ ] **Step 2: Write the failing parse guard**

Create `kanz/test/arch/node_exporter_test.go`:

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

// TestNodeExporterDaemonSet asserts the node-exporter manifest is a valid DaemonSet
// that tolerates every node (so a newly-joined node is actually scraped). A malformed
// or too-narrowly-tolerated manifest would silently skip nodes.
func TestNodeExporterDaemonSet(t *testing.T) {
	path := filepath.Join(moduleRoot(t), "infra", "observability", "node-exporter.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	type doc struct {
		Kind string `yaml:"kind"`
		Spec struct {
			Template struct {
				Spec struct {
					Tolerations []struct {
						Operator string `yaml:"operator"`
					} `yaml:"tolerations"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	var found bool
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	for {
		var d doc
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if d.Kind != "DaemonSet" {
			continue
		}
		found = true
		var tolerated bool
		for _, tol := range d.Spec.Template.Spec.Tolerations {
			if tol.Operator == "Exists" {
				tolerated = true
			}
		}
		if !tolerated {
			t.Errorf("node-exporter DaemonSet must tolerate every node (a toleration with operator: Exists), else new/tainted nodes are not scraped")
		}
	}
	if !found {
		t.Fatalf("no DaemonSet found in %s", path)
	}
}
```

- [ ] **Step 3: Run the guard**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./test/arch/ -run TestNodeExporterDaemonSet -v`
Expected: PASS.

- [ ] **Step 4: Mutation-check (non-vacuity)**

Temporarily remove the `tolerations:` block from the manifest, re-run: expect FAIL ("must tolerate every node"). Restore it; re-run: PASS. Confirm `git diff` on the manifest is empty before committing.

- [ ] **Step 5: Commit**

```bash
cd kanz && git add infra/observability/node-exporter.yaml test/arch/node_exporter_test.go
git commit -m "feat(observability): node-exporter DaemonSet (scrapes every joined node)

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

- [ ] **Step 2: Full arch suite (incl. the SSH-plane guard)**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./test/arch/...`
Expected: PASS — including **`TestNoSSHPlane`** (the operator gained a TCP dial, NOT `crypto/ssh`), the operator RBAC guards, and the new `TestNodeExporterDaemonSet`.

- [ ] **Step 3: govulncheck**

Run: `cd kanz && GOFLAGS=-mod=mod go run golang.org/x/vuln/cmd/govulncheck@latest ./...`
Expected: 0 reachable (an unreachable openpgp advisory with no fix is acceptable). If a reachable vuln appears, STOP and report.

- [ ] **Step 4: Rig-proof handoff (manual — operator runs it)**

*Test Connection (fully provable on the F0 rig — no k3s needed):*
```bash
kubectl -n kanz-operator port-forward deploy/operator 9090:9090
# in universe: [a] Add Node → fill IP + SSH Port → ctrl+t
# Expect: "✓ reachable (Nms)" against a reachable host:port; "✗ unreachable: ..." against a closed one.
```

*node-exporter (needs a real cluster; scrape needs a Prometheus):*
```bash
kubectl apply -f kanz/infra/observability/node-exporter.yaml
kubectl -n kanz-observability rollout status ds/node-exporter
# Expect: one node-exporter pod per node; curl a pod's :9100/metrics returns node_* series.
# Being scraped requires a Prometheus with pod-annotation discovery (infra, out of scope).
```

- [ ] **Step 5: Board update (controller)**

The controller adds the S2b readiness row (Test Connection unit-proven + rig-provable; node-exporter DaemonSet infra-gated) and notes the deferred control-plane addons. Validate with `bash tools/validate-board.sh KANZ_TASKS.md`, commit, push.

---

## Notes for the executor

- **The `crypto/ssh` direction flips this slice:** S2a *added* it (bounded to the provisioner); S2b must keep it OUT of the operator. Task 2 Step 5 and Task 5 Step 2 both assert `TestNoSSHPlane` stays green. If Test Connection ever imports an SSH library, that is the wrong design — it is a TCP dial.
- **Interface-widening (Task 3):** `nodeSource` gains `testConnection`; every implementer (`grpcSource` + the test `stubSource`) must gain it or the package won't compile.
- **`ctrl+t`, not `t`:** the form's letter keys are typed into fields, so the Test key must be a non-rune chord (`ctrl+t`).
- **Deferred by design (do NOT add here):** system-upgrade-controller (auto-updates), `flannel-backend=wireguard-native` (encrypted mesh) — both k3s control-plane concerns for a later control-plane bring-up slice; a full SSH-auth pre-flight; deploying Prometheus.
- **Git hygiene:** never `git add -A` — untracked build artifacts (`universe.exe`, `kanz-monitor.exe`, `kanz-py/gen/`) must not be committed; stage explicit paths.
