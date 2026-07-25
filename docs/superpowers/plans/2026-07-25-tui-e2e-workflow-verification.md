# TUI End-to-End Workflow Verification (OPS-M2e) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prove every existing operator workflow by driving the real TUI binary through a pseudo-terminal against a live two-node cluster, with Kubernetes as the oracle — and fix whatever that finds.

**Architecture:** Two harnesses split by what each can prove. A PTY driver (`kanz/test/e2e/tui/`) spawns the real `universe` binary against the real api-gateway and cluster; it covers the seven state-changing workflows and asserts against `kubectl`, never against rendered output. Model-level tests in `kanz/cmd/universe/` drive `Update()` with synthesized `tea.KeyMsg` and cover the combinatorial surface — navigation permutations, validation branches, error paths — in milliseconds with no cluster.

**Tech Stack:** Go 1.26.5, Bubble Tea (already vendored), `github.com/creack/pty` (new, test-only), `kubectl` as oracle, k3s v1.36.2 two-node cluster.

## Global Constraints

- **Go version: 1.26.5**, matching every Dockerfile in the repo. Do not install a different minor.
- **Assertions match domain text, never layout.** No assertion may depend on column position, box-drawing characters, padding, or colour. Slice 2 rewrites every frame; a layout-coupled harness gets deleted and takes the regression net with it.
- **The oracle is `kubectl` or the cluster API, never the TUI's own rendering.** A UI that renders optimistic intent rather than observed state must fail these tests.
- **No `time.Sleep` as a synchronisation primitive, and no retry loops around assertions.** Use `WaitFor`. If a workflow needs a sleep to be observable, that is a TUI defect to fix, not a test to weaken.
- **PTY tests skip — loudly — when `KANZ_E2E_GATEWAY`, `KANZ_E2E_TOKEN`, `KANZ_E2E_NODE2_IP` or `KANZ_E2E_SSH_KEY` are unset.** A skip is never reported as a pass. Mirrors the existing `TEST_POSTGRES_URL` convention.
- **Node 2 is the only safe target for cordon/drain/region.** Node 1 runs the estate; draining it evicts the trading loop, NATS and Postgres.
- **No credential may appear in a captured frame or a service log.** Test 6 asserts this by searching both.
- **Fixes are in scope.** Every failure this harness finds is fixed in this slice, covered by the test that caught it.

**Environment facts (verified 2026-07-25):**
- Node 1: `57.180.60.86`, internal `172.26.11.140`, k3s **server**, 2 vCPU / 3.8 GB, runs the estate.
- Node 2: `52.195.224.210`, user `ubuntu`, Ubuntu 24.04.4, x86_64, 2 vCPU / **414 MB**, k3s absent, reachable with the same key.
- SSH key: `LightsailDefaultKey-ap-northeast-1.pem`, fingerprint `SHA256:lOvFt9P3Iy72VFK7eQswG949dvGU46oOqpnP4yYdIPg`.
- Gateway dev secrets: JWT `dev-only-not-a-real-jwt-secret`, signing `dev-only-not-a-real-signing-secret`, roles `kanz-user` + `kanz-operator`.

---

## File Structure

| File | Responsibility |
|---|---|
| `kanz/test/e2e/tui/driver.go` | PTY lifecycle: `Start`, `Send`, `WaitFor`, `Frames`, `Close`. Knows nothing about kanz. |
| `kanz/test/e2e/tui/driver_test.go` | Proves the driver against a trivial program, so a driver bug never masquerades as a TUI bug. |
| `kanz/test/e2e/tui/env.go` | Env gating (`requireEnv`), binary build, gateway port-forward, token minting. |
| `kanz/test/e2e/tui/oracle.go` | `kubectl` wrappers: node schedulable state, labels, pods on a node, secret data keys. |
| `kanz/test/e2e/tui/workflow_test.go` | The seven proofs. One test function each. |
| `kanz/cmd/universe/model_nav_test.go` | Navigation permutation coverage (model-level). |
| `kanz/cmd/universe/model_validation_test.go` | Inline validation + error-path coverage (model-level). |
| `docs/superpowers/plans/...-report.md` | Captured run output — the demonstration. |

---

## Task 1: PTY driver, proven against a trivial program

**Files:**
- Create: `kanz/test/e2e/tui/driver.go`
- Test: `kanz/test/e2e/tui/driver_test.go`
- Modify: `kanz/go.mod`, `kanz/go.sum`

**Interfaces:**
- Consumes: nothing.
- Produces: `type Session struct{}`; `func Start(t *testing.T, bin string, env []string) *Session`; `func (s *Session) Send(keys string)`; `func (s *Session) SendKey(b byte)`; `func (s *Session) WaitFor(sub string, timeout time.Duration)`; `func (s *Session) Frames() string`; `func (s *Session) Close()`.

- [ ] **Step 1: Add the dependency**

```bash
cd kanz && GOFLAGS=-mod=mod go get github.com/creack/pty@v1.1.24
```

Expected: `go.mod` gains `github.com/creack/pty v1.1.24`. It enters no service binary's graph — verify with `go list -deps ./services/oms/cmd/oms | grep -c creack` returning `0`.

- [ ] **Step 2: Write the failing driver test**

```go
package tui

import (
	"testing"
	"time"
)

// The driver is proven against a program whose behaviour is not in question, so a
// driver bug can never be mistaken for a TUI bug later.
func TestDriverSeesOutputAndSendsInput(t *testing.T) {
	s := Start(t, "/bin/sh", []string{"-c", `read x; echo "GOT:$x"`}, nil)
	defer s.Close()
	s.Send("hello\r")
	s.WaitFor("GOT:hello", 5*time.Second)
}

func TestWaitForFailsWithTheLastFrameAttached(t *testing.T) {
	s := Start(t, "/bin/sh", []string{"-c", `echo actual-output; sleep 5`}, nil)
	defer s.Close()
	fake := &testing.T{}
	func() {
		defer func() { _ = recover() }()
		s.waitFor(fake, "never-appears", 500*time.Millisecond)
	}()
	if !fake.Failed() {
		t.Fatal("WaitFor must fail when the substring never appears — a harness that hangs " +
			"or passes silently cannot be trusted to prove anything")
	}
	if !strings.Contains(s.Frames(), "actual-output") {
		t.Error("the captured frames must be available for the failure message; without them a " +
			"red E2E test tells you nothing about what the program actually showed")
	}
}
```

- [ ] **Step 3: Run it to confirm it fails**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./test/e2e/tui/ -run TestDriver -v`
Expected: FAIL — `undefined: Start`.

- [ ] **Step 4: Implement the driver**

```go
// Package tui drives the operator TUI through a pseudo-terminal.
//
// It exists because the TUI is a full-screen program: the only way to prove a
// keypress reaches Kubernetes is to send a real keypress to a real terminal and
// then ask Kubernetes. Model-level tests cannot do that, and API-level tests prove
// the server rather than the client.
package tui

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// fixedCols/fixedRows pin the terminal geometry. A test whose result depends on
// terminal size is a test that fails on somebody else's machine.
const (
	fixedCols = 120
	fixedRows = 40
)

// Session is one running program attached to a pty.
type Session struct {
	cmd  *exec.Cmd
	tty  *os.File
	mu   sync.Mutex
	buf  bytes.Buffer
	done chan struct{}
}

// Start launches bin in a pty and begins draining its output.
func Start(t *testing.T, bin string, args []string, env []string) *Session {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: fixedCols, Rows: fixedRows})
	if err != nil {
		t.Fatalf("start %s in pty: %v", bin, err)
	}
	s := &Session{cmd: cmd, tty: f, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		chunk := make([]byte, 4096)
		for {
			n, err := f.Read(chunk)
			if n > 0 {
				s.mu.Lock()
				s.buf.Write(chunk[:n])
				s.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return s
}

// Send writes keystrokes. "\r" is Enter; use SendKey for control bytes.
func (s *Session) Send(keys string) { _, _ = s.tty.WriteString(keys) }

// SendKey writes one raw byte, for control sequences such as ctrl+t (0x14).
func (s *Session) SendKey(b byte) { _, _ = s.tty.Write([]byte{b}) }

// WaitFor blocks until the captured output contains sub, or fails the test with the
// full capture attached. This is the ONLY synchronisation primitive in the harness:
// a sleep would pass on a fast machine and flake on a slow one, and would hide
// exactly the missing-feedback defects this slice exists to find.
func (s *Session) WaitFor(t *testing.T, sub string, timeout time.Duration) {
	t.Helper()
	s.waitFor(t, sub, timeout)
}

func (s *Session) waitFor(t *testing.T, sub string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(s.Frames(), sub) {
			return
		}
		time.Sleep(50 * time.Millisecond) // polling the CAPTURE, not the workflow
	}
	t.Errorf("timed out after %s waiting for %q.\n--- captured output ---\n%s\n--- end ---",
		timeout, sub, s.Frames())
}

// Frames returns everything the program has written so far, ANSI included.
func (s *Session) Frames() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// Close asks the program to quit, then kills it if it will not.
func (s *Session) Close() {
	s.Send("q")
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
		_ = s.cmd.Process.Kill()
	}
	_ = s.tty.Close()
}
```

Add `"strings"` to the test file's imports.

- [ ] **Step 5: Run the driver tests**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./test/e2e/tui/ -run TestDriver -v`
Expected: PASS, both tests.

- [ ] **Step 6: Commit**

```bash
git add kanz/go.mod kanz/go.sum kanz/test/e2e/tui/driver.go kanz/test/e2e/tui/driver_test.go
git commit -m "test(e2e): a pty driver for the operator TUI, proven against a trivial program

The TUI is a full-screen program, so the only way to prove a keypress reaches
Kubernetes is to send a real keypress to a real terminal and then ask Kubernetes.
WaitFor is the harness's only synchronisation primitive and attaches the full capture
to its failure: a sleep would pass on a fast machine, flake on a slow one, and hide
exactly the missing-feedback defects this slice exists to find.

The driver is tested against /bin/sh rather than against the TUI, so a driver bug can
never be mistaken for a TUI bug."
```

---

## Task 2: Environment — Go on node 1, node 2 swap, join Secret, gating, oracle

**Files:**
- Create: `kanz/test/e2e/tui/env.go`, `kanz/test/e2e/tui/oracle.go`
- Test: `kanz/test/e2e/tui/oracle_test.go`

**Interfaces:**
- Consumes: Task 1's `Session`.
- Produces: `func requireEnv(t *testing.T) Env`; `type Env struct{ GatewayURL, Token, SigningSecret, Node2IP, SSHKeyPath, Binary string }`; `func mintOperatorToken(secret string) string`; `func kubectl(t *testing.T, args ...string) string`; `func nodeSchedulable(t *testing.T, node string) bool`; `func nodeLabel(t *testing.T, node, key string) string`; `func podsOnNode(t *testing.T, node string) []string`; `func secretDataKeys(t *testing.T, ns, name string) []string`.

- [ ] **Step 1: Prepare node 1 — Go toolchain and the gateway port-forward prerequisite**

Run on node 1 (`57.180.60.86`):

```bash
curl -fsSLo /tmp/go.tgz https://go.dev/dl/go1.26.5.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf /tmp/go.tgz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
export PATH=$PATH:/usr/local/go/bin && go version
```

Expected: `go version go1.26.5 linux/amd64`. The E2E tests are Go tests, so the toolchain must be where the cluster is.

- [ ] **Step 2: Prepare node 2 — swap, because 414 MB will not hold a k3s agent comfortably**

Run on node 2 (`52.195.224.210`):

```bash
sudo fallocate -l 2G /swapfile && sudo chmod 600 /swapfile
sudo mkswap /swapfile && sudo swapon /swapfile
echo '/swapfile none swap sw 0 0' | sudo tee -a /etc/fstab
free -m
```

Expected: `Swap: 2047`. Without this, kubelet is the likely OOM victim and the node will flap NotReady — which reads as a join failure and would send the next engineer debugging the wrong thing.

- [ ] **Step 3: Create the `operator-k3s-join` Secret**

`operator-deploy.yaml` references it with `optional: true`, so the operator starts without it and `AddNode` would create a Job that cannot join.

```bash
# on node 1
export KUBECONFIG=$HOME/.kube/config
TOKEN=$(sudo cat /var/lib/rancher/k3s/server/node-token)
kubectl -n kanz-operator create secret generic operator-k3s-join \
  --from-literal=server_url="https://172.26.11.140:6443" \
  --from-literal=token="$TOKEN"
kubectl -n kanz-operator rollout restart deploy/operator
kubectl -n kanz-operator logs deploy/operator | grep -i "node provisioning enabled"
```

Expected: the log line is present. The server URL is the node's **internal** IP — the joining agent dials it from inside the VPC.

- [ ] **Step 4: Write the failing oracle test**

```go
package tui

import "testing"

func TestOracleReadsNodeState(t *testing.T) {
	env := requireEnv(t) // skips unless the E2E env is configured
	node1 := kubectl(t, "get", "nodes", "-o", "jsonpath={.items[0].metadata.name}")
	if node1 == "" {
		t.Fatal("oracle returned no node name; kubectl is not usable from here, so no proof " +
			"in this package can assert against the cluster")
	}
	if !nodeSchedulable(t, node1) {
		t.Errorf("node %s reports unschedulable at rest — check nothing left it cordoned", node1)
	}
	_ = env
}
```

- [ ] **Step 5: Run it to confirm it fails**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./test/e2e/tui/ -run TestOracle -v`
Expected: FAIL — `undefined: requireEnv`.

- [ ] **Step 6: Implement `env.go`**

```go
package tui

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Env is everything a PTY proof needs to reach the platform.
type Env struct {
	GatewayURL    string
	Token         string
	SigningSecret string
	Node2IP       string
	SSHKeyPath    string
	Binary        string
}

// requireEnv gathers the environment or SKIPS — loudly, naming what is missing.
//
// A skip is not a pass. The board records these workflows as verified only against a
// run in which they executed, so a silent skip that reads as green is the one outcome
// this function must make impossible.
func requireEnv(t *testing.T) Env {
	t.Helper()
	need := map[string]string{
		"KANZ_E2E_GATEWAY":  os.Getenv("KANZ_E2E_GATEWAY"),
		"KANZ_E2E_TOKEN":    os.Getenv("KANZ_E2E_TOKEN"),
		"KANZ_E2E_NODE2_IP": os.Getenv("KANZ_E2E_NODE2_IP"),
		"KANZ_E2E_SSH_KEY":  os.Getenv("KANZ_E2E_SSH_KEY"),
	}
	var missing []string
	for k, v := range need {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		t.Skipf("SKIPPING a real end-to-end proof — unset: %s. This is NOT a pass: the workflow "+
			"was not exercised. Set these against a live two-node cluster to run it.",
			strings.Join(missing, ", "))
	}
	return Env{
		GatewayURL:    need["KANZ_E2E_GATEWAY"],
		Token:         need["KANZ_E2E_TOKEN"],
		SigningSecret: os.Getenv("KANZ_E2E_SIGNING_SECRET"),
		Node2IP:       need["KANZ_E2E_NODE2_IP"],
		SSHKeyPath:    need["KANZ_E2E_SSH_KEY"],
		Binary:        buildTUI(t),
	}
}

// buildTUI compiles the real binary under test once per run.
func buildTUI(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "universe")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/universe")
	cmd.Dir = moduleDir(t)
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build ./cmd/universe: %v\n%s", err, b)
	}
	return out
}

// moduleDir walks up to the directory holding go.mod.
func moduleDir(t *testing.T) string {
	t.Helper()
	d, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			t.Fatal("no go.mod found above the test directory")
		}
		d = parent
	}
}

// sessionEnv is the environment the TUI itself needs.
func (e Env) sessionEnv() []string {
	env := []string{
		"KANZ_GATEWAY_URL=" + e.GatewayURL,
		"KANZ_TOKEN=" + e.Token,
	}
	if e.SigningSecret != "" {
		env = append(env, "KANZ_SIGNING_SECRET="+e.SigningSecret)
	}
	return env
}

// mintOperatorToken builds an HS256 token carrying the baseline and operator roles —
// the dev-validator path. Production authenticates with OIDC and this helper has no
// equivalent there, which is why slice 4 exists.
func mintOperatorToken(secret string) string {
	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	header := b64([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"sub":    "e2e",
		"tenant": "fund-alpha",
		"roles":  []string{"kanz-user", "kanz-operator"},
		"exp":    time.Now().Add(time.Hour).Unix(),
	})
	payload := b64(claims)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(header + "." + payload))
	return fmt.Sprintf("%s.%s.%s", header, payload, b64(mac.Sum(nil)))
}
```

- [ ] **Step 7: Implement `oracle.go`**

```go
package tui

import (
	"os/exec"
	"strings"
	"testing"
)

// kubectl runs a read-only query against the cluster. Every assertion in this package
// goes through here rather than through the TUI's rendering: a UI that shows its
// optimistic intent instead of observed state must FAIL these proofs, and it can only
// do that if the oracle is independent of it.
func kubectl(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("kubectl", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// nodeSchedulable reports whether the node accepts new pods. Absent means schedulable
// — .spec.unschedulable is only set when cordoned.
func nodeSchedulable(t *testing.T, node string) bool {
	t.Helper()
	return kubectl(t, "get", "node", node, "-o", "jsonpath={.spec.unschedulable}") != "true"
}

func nodeLabel(t *testing.T, node, key string) string {
	t.Helper()
	return kubectl(t, "get", "node", node, "-o", "jsonpath={.metadata.labels."+
		strings.ReplaceAll(key, ".", "\\.")+"}")
}

// podsOnNode lists non-terminated pods bound to a node, DaemonSets included — the
// drain proof needs to distinguish what may be evicted from what may not.
func podsOnNode(t *testing.T, node string) []string {
	t.Helper()
	out := kubectl(t, "get", "pods", "-A", "--field-selector",
		"spec.nodeName="+node+",status.phase=Running",
		"-o", "jsonpath={range .items[*]}{.metadata.namespace}/{.metadata.name} {end}")
	if out == "" {
		return nil
	}
	return strings.Fields(out)
}

func secretDataKeys(t *testing.T, ns, name string) []string {
	t.Helper()
	out := kubectl(t, "-n", ns, "get", "secret", name,
		"-o", "jsonpath={range $k, $v := .data}{$k} {end}")
	return strings.Fields(out)
}

func nodeExists(t *testing.T, node string) bool {
	t.Helper()
	out, err := exec.Command("kubectl", "get", "node", node, "-o", "name").CombinedOutput()
	_ = out
	return err == nil
}
```

- [ ] **Step 8: Run the oracle test with the environment set**

Run on node 1:

```bash
export KUBECONFIG=$HOME/.kube/config PATH=$PATH:/usr/local/go/bin
kubectl -n kanz-services port-forward svc/api-gateway 18080:8080 >/dev/null 2>&1 &
export KANZ_E2E_GATEWAY=http://localhost:18080
export KANZ_E2E_SIGNING_SECRET=dev-only-not-a-real-signing-secret
export KANZ_E2E_NODE2_IP=52.195.224.210
export KANZ_E2E_SSH_KEY=$HOME/.ssh/lightsail.pem
export KANZ_E2E_TOKEN=$(cd ~/kanz/kanz && go run ./cmd/kanz-devtoken \
  --secret dev-only-not-a-real-jwt-secret --tenant fund-alpha \
  --role kanz-user,kanz-operator)
cd ~/kanz/kanz && GOFLAGS=-mod=mod go test ./test/e2e/tui/ -run TestOracle -v
```

Expected: PASS. Also confirm the skip path is honest: `env -u KANZ_E2E_GATEWAY go test ./test/e2e/tui/ -run TestOracle -v` prints `SKIPPING a real end-to-end proof` and reports `SKIP`, not `ok`.

- [ ] **Step 9: Commit**

```bash
git add kanz/test/e2e/tui/env.go kanz/test/e2e/tui/oracle.go kanz/test/e2e/tui/oracle_test.go
git commit -m "test(e2e): environment gating and a kubectl oracle independent of the TUI

Every assertion in this package goes through kubectl rather than through rendered
output. A UI that shows its optimistic intent instead of observed state must FAIL
these proofs, and it can only do that if the oracle cannot see the UI.

requireEnv SKIPS loudly and names what is missing. A skip is not a pass: the board
records these workflows as verified only against a run in which they executed, so a
silent skip reading as green is the one outcome this must make impossible."
```

---

## Task 3: Proof 1 — navigation, and quitting from everywhere

**Files:**
- Create: `kanz/test/e2e/tui/workflow_test.go`

**Interfaces:**
- Consumes: `Start`, `Session.WaitFor`, `requireEnv`, `Env.sessionEnv`.
- Produces: `func startTUI(t *testing.T, env Env) *Session` — used by every later proof.

- [ ] **Step 1: Write the failing test**

```go
package tui

import (
	"testing"
	"time"
)

// startTUI launches the real binary against the real gateway and waits until it has
// rendered its first estate poll.
func startTUI(t *testing.T, env Env) *Session {
	t.Helper()
	s := Start(t, env.Binary, nil, env.sessionEnv())
	s.WaitFor(t, "Nodes", 20*time.Second)
	return s
}

func TestNavigationReachesEveryPaneAndQuitsCleanly(t *testing.T) {
	env := requireEnv(t)
	s := startTUI(t, env)
	defer s.Close()

	// tab cycles Nodes -> Clusters -> API Manager -> Nodes. Assert on the pane's own
	// domain text, never on layout.
	s.Send("\t")
	s.WaitFor(t, "Clusters", 5*time.Second)
	s.Send("\t")
	s.WaitFor(t, "API", 5*time.Second)
	s.Send("\t")
	s.WaitFor(t, "Nodes", 5*time.Second)
}

// A form must not submit on quit. An operator who opens Add Node, changes their mind
// and presses q must not have provisioned anything.
func TestQuitFromInsideAFormSubmitsNothing(t *testing.T) {
	env := requireEnv(t)
	before := len(kubectlLines(t, "get", "jobs", "-n", "kanz-operator", "-o", "name"))

	s := startTUI(t, env)
	s.Send("a")
	s.WaitFor(t, "Hostname", 5*time.Second)
	s.Send("q")
	s.Close()

	after := len(kubectlLines(t, "get", "jobs", "-n", "kanz-operator", "-o", "name"))
	if after != before {
		t.Errorf("quitting from inside the Add Node form changed the Job count (%d -> %d); "+
			"abandoning a form must provision nothing", before, after)
	}
}
```

Add to `oracle.go`:

```go
// kubectlLines splits an oracle query into non-empty lines.
func kubectlLines(t *testing.T, args ...string) []string {
	t.Helper()
	out := kubectl(t, args...)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}
```

- [ ] **Step 2: Run it**

Run: `cd ~/kanz/kanz && GOFLAGS=-mod=mod go test ./test/e2e/tui/ -run TestNavigation -v`
Expected: either PASS, or a real failure. **If it fails, that is a finding, not a broken test** — record the captured frame and fix the TUI in this task before moving on.

- [ ] **Step 3: Run the quit-from-form proof**

Run: `cd ~/kanz/kanz && GOFLAGS=-mod=mod go test ./test/e2e/tui/ -run TestQuitFrom -v`
Expected: PASS. A failure here means `q` inside a form submits — a real defect; fix it and keep the test.

- [ ] **Step 4: Commit**

```bash
git add kanz/test/e2e/tui/workflow_test.go kanz/test/e2e/tui/oracle.go
git commit -m "test(e2e): prove navigation and that abandoning a form provisions nothing

First proof driven through a real terminal against a real cluster. The quit-from-form
case has an oracle rather than a rendering assertion: an operator who opens Add Node,
changes their mind and presses q must not have provisioned anything, and only the Job
count can say so."
```

---

## Task 4: Proof 2 — Add Node, Test Connection, and the live k3s join

This closes the S2a live-join gap and the S2b `ctrl+t` gap together. It is the longest-running proof.

**Files:**
- Modify: `kanz/test/e2e/tui/workflow_test.go`

**Interfaces:**
- Consumes: `startTUI`, `nodeExists`, `podsOnNode`, `kubectl`.
- Produces: nothing new.

- [ ] **Step 1: Write the failing test**

```go
// ctrl+t is byte 0x14. The Add Node form's Test Connection probe is a TCP dial, so a
// reachable SSH port answers and a closed port does not.
const ctrlT = byte(0x14)

func TestAddNodeProbesThenJoinsTheNodeLive(t *testing.T) {
	env := requireEnv(t)
	if nodeExists(t, "ip-"+dashed(env.Node2IP)) {
		t.Skip("node 2 is already joined; delete it from the cluster to re-prove the join")
	}

	s := startTUI(t, env)
	defer s.Close()

	s.Send("a")
	s.WaitFor(t, "Hostname", 5*time.Second)

	// Fields in order: Hostname, IP, SSH Port (prefilled 22), User, Key Path.
	s.Send("e2e-node2\t")
	s.Send(env.Node2IP + "\t")
	s.Send("\t")            // accept the prefilled port
	s.Send("ubuntu\t")
	s.Send(env.SSHKeyPath)

	// S2b: probe before committing.
	s.SendKey(ctrlT)
	s.WaitFor(t, "reachable", 20*time.Second)

	s.Send("\r") // save
	// The join installs k3s over SSH on a 414MB host; allow real time for it.
	s.WaitFor(t, "Installing", 30*time.Second)

	waitForNodeReady(t, 6*time.Minute)
}

// dashed renders an IP the way k3s names a node from its hostname.
func dashed(ip string) string { return strings.ReplaceAll(ip, ".", "-") }
```

Add to `oracle.go`:

```go
// waitForNodeReady blocks until a second node reports Ready, polling the CLUSTER.
// The TUI's own strip is not evidence that a node joined.
func waitForNodeReady(t *testing.T, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out := kubectl(t, "get", "nodes",
			"-o", "jsonpath={range .items[*]}{.metadata.name}={.status.conditions[?(@.type==\"Ready\")].status} {end}")
		for _, pair := range strings.Fields(out) {
			name, status, _ := strings.Cut(pair, "=")
			if status == "True" && !strings.Contains(name, "172-26-11-140") {
				return name
			}
		}
		time.Sleep(5 * time.Second) // polling the CLUSTER, not a workflow's feedback
	}
	t.Fatalf("no second node reached Ready within %s. Check `journalctl -u k3s-agent` on "+
		"node 2 — on a 414MB host an OOM-killed kubelet presents as a node that never "+
		"appears, which reads as a join failure and is not one.", timeout)
	return ""
}
```

- [ ] **Step 2: Run it**

Run: `cd ~/kanz/kanz && GOFLAGS=-mod=mod go test ./test/e2e/tui/ -run TestAddNode -v -timeout 15m`
Expected: PASS with a second node Ready.

**If the probe fails:** the operator cannot reach node 2 on :22 — check the `node-provisioner-egress` NetworkPolicy still permits :22 and that node 2's security group allows the cluster's egress IP.
**If the join fails:** read the Job's logs (`kubectl -n kanz-operator logs job/<name>`) and node 2's `journalctl -u k3s-agent`. The likely causes are a wrong `server_url` in the join Secret, or an OOM on the 414 MB host despite the swap added in Task 2.

- [ ] **Step 3: Verify the transient Secret's ownership and GC**

```bash
JOB=$(kubectl -n kanz-operator get jobs -o name | tail -1 | cut -d/ -f2)
kubectl -n kanz-operator get secret -o json | \
  python3 -c "import json,sys; [print(s['metadata']['name'], s['metadata'].get('ownerReferences')) for s in json.load(sys.stdin)['items']]"
kubectl -n kanz-operator delete job "$JOB"
sleep 5
kubectl -n kanz-operator get secrets -o name
```

Expected: the per-provision SSH-key Secret carries an `ownerReferences` entry naming the Job, and disappears when the Job is deleted. `operator-k3s-join` (long-lived, created in Task 2) must remain.

- [ ] **Step 4: Commit**

```bash
git add kanz/test/e2e/tui/workflow_test.go kanz/test/e2e/tui/oracle.go
git commit -m "test(e2e): prove Add Node end to end — probe, then a LIVE k3s join

Closes two board rows that have been unexercised since they were written: S2a's live
join (infra-gated because the rig's single node was already a server) and S2b's ctrl+t
(never pressed). Both are now driven from the TUI, with kubectl as the oracle — the
provisioning strip saying Joined is not evidence that a node joined.

The failure message names the 414MB OOM case explicitly, because an OOM-killed kubelet
presents as a node that never appears and reads as a join failure that it is not."
```

---

## Task 5: Proof 3 — cordon and uncordon

**Files:** Modify `kanz/test/e2e/tui/workflow_test.go`

**Interfaces:** Consumes `startTUI`, `nodeSchedulable`, `selectNode2`.

- [ ] **Step 1: Write the failing test**

```go
// selectNode2 moves the Nodes-pane selection onto node 2. Node ordering is the
// cluster's, so the helper searches rather than assuming an index.
func selectNode2(t *testing.T, s *Session, node2 string) {
	t.Helper()
	s.WaitFor(t, node2, 10*time.Second)
	for i := 0; i < 10; i++ {
		if strings.Contains(s.Frames(), "> "+node2) || strings.Contains(s.Frames(), node2+" <") {
			return
		}
		s.Send("\x1b[B") // down arrow
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("could not move the selection onto %s in the Nodes pane. If the TUI does not "+
		"mark the selected row in a machine-readable way, that is the finding: an operator "+
		"cannot tell which node an action will hit either.", node2)
}

func TestCordonAndUncordonChangeTheCluster(t *testing.T) {
	env := requireEnv(t)
	node2 := waitForNodeReady(t, 2*time.Minute)
	if !nodeSchedulable(t, node2) {
		kubectl(t, "uncordon", node2) // start from a known state
	}

	s := startTUI(t, env)
	defer s.Close()
	selectNode2(t, s, node2)

	s.Send("c")
	waitForSchedulable(t, node2, false, 30*time.Second)

	s.Send("u")
	waitForSchedulable(t, node2, true, 30*time.Second)
}
```

Add to `oracle.go`:

```go
// waitForSchedulable polls the CLUSTER until the node reaches the wanted state.
func waitForSchedulable(t *testing.T, node string, want bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if nodeSchedulable(t, node) == want {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("node %s did not become schedulable=%v within %s — the keypress did not reach "+
		"Kubernetes, whatever the TUI rendered", node, want, timeout)
}
```

- [ ] **Step 2: Run it**

Run: `cd ~/kanz/kanz && GOFLAGS=-mod=mod go test ./test/e2e/tui/ -run TestCordonAndUncordon -v -timeout 5m`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add kanz/test/e2e/tui/workflow_test.go kanz/test/e2e/tui/oracle.go
git commit -m "test(e2e): prove cordon and uncordon reach Kubernetes

The assertion is .spec.unschedulable on the node, not the TUI's redraw. selectNode2
fails with an explicit message if the selected row is not machine-readable, because if
a harness cannot tell which node an action will hit, neither can an operator."
```

---

## Task 6: Proof 4 — drain, both branches, on node 2 only

**Files:** Modify `kanz/test/e2e/tui/workflow_test.go`

**Interfaces:** Consumes `startTUI`, `selectNode2`, `podsOnNode`, `kubectl`.

- [ ] **Step 1: Pin a tiny evictable workload to node 2**

Node 2 has 414 MB, so the workload must be deliberately minimal — its purpose is to be evicted, not to do anything.

```bash
kubectl create ns e2e-drain 2>/dev/null || true
cat <<'YAML' | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata: { name: drain-target, namespace: e2e-drain }
spec:
  replicas: 1
  selector: { matchLabels: { app: drain-target } }
  template:
    metadata: { labels: { app: drain-target } }
    spec:
      nodeSelector: { kubernetes.io/hostname: NODE2_NAME }
      tolerations: [{ operator: Exists }]
      containers:
        - name: pause
          image: registry.k8s.io/pause:3.9
          resources: { requests: { memory: "8Mi", cpu: "10m" }, limits: { memory: "16Mi" } }
YAML
```

Replace `NODE2_NAME` with the joined node's name. Confirm: `kubectl -n e2e-drain get pods -o wide` shows it Running on node 2.

- [ ] **Step 2: Write the failing test**

```go
func TestDrainConfirmAbortsAndProceeds(t *testing.T) {
	env := requireEnv(t)
	node2 := waitForNodeReady(t, 2*time.Minute)
	node1 := kubectl(t, "get", "nodes", "-o",
		"jsonpath={.items[?(@.metadata.labels.node-role\\.kubernetes\\.io/control-plane)].metadata.name}")

	before2 := podsOnNode(t, node2)
	before1 := len(podsOnNode(t, node1))
	if len(before2) == 0 {
		t.Fatal("node 2 runs no pods, so a drain would prove nothing. Apply the e2e-drain " +
			"workload from step 1 first.")
	}

	s := startTUI(t, env)
	defer s.Close()
	selectNode2(t, s, node2)

	// BRANCH 1: n must abort. Nothing evicted, node stays schedulable.
	s.Send("d")
	s.WaitFor(t, "y/n", 5*time.Second)
	s.Send("n")
	time.Sleep(3 * time.Second) // give a wrongly-fired drain time to do damage
	if got := len(podsOnNode(t, node2)); got != len(before2) {
		t.Errorf("answering n to the drain confirm evicted pods anyway (%d -> %d). A confirm "+
			"dialog that acts on the wrong answer is worse than no dialog.", len(before2), got)
	}
	if !nodeSchedulable(t, node2) {
		t.Error("answering n cordoned the node; abort must change nothing at all")
	}

	// BRANCH 2: y must drain.
	s.Send("d")
	s.WaitFor(t, "y/n", 5*time.Second)
	s.Send("y")
	waitForSchedulable(t, node2, false, 60*time.Second) // drain cordons first
	waitForNoEvictablePods(t, node2, 3*time.Minute)

	// The estate must be untouched: drain is scoped to the selected node.
	if after1 := len(podsOnNode(t, node1)); after1 != before1 {
		t.Errorf("draining node 2 changed the pod count on node 1 (%d -> %d) — the trading "+
			"loop must not be affected by draining another node", before1, after1)
	}

	kubectl(t, "uncordon", node2)
}
```

Add to `oracle.go`:

```go
// waitForNoEvictablePods waits until only DaemonSet-owned and mirror pods remain.
// Drain is eviction-only by design, so those are exactly what must survive.
func waitForNoEvictablePods(t *testing.T, node string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out := kubectl(t, "get", "pods", "-A", "--field-selector",
			"spec.nodeName="+node+",status.phase=Running",
			"-o", "jsonpath={range .items[*]}{.metadata.name}:{.metadata.ownerReferences[0].kind} {end}")
		evictable := 0
		for _, p := range strings.Fields(out) {
			if _, kind, _ := strings.Cut(p, ":"); kind != "DaemonSet" && kind != "Node" {
				evictable++
			}
		}
		if evictable == 0 {
			return
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("evictable pods still running on %s after %s. If a PodDisruptionBudget is "+
		"blocking, that is CORRECT behaviour — check `kubectl get pdb -A` and whether the "+
		"blocked pod should have been on this node at all.", node, timeout)
}
```

- [ ] **Step 3: Run it**

Run: `cd ~/kanz/kanz && GOFLAGS=-mod=mod go test ./test/e2e/tui/ -run TestDrainConfirm -v -timeout 10m`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add kanz/test/e2e/tui/workflow_test.go kanz/test/e2e/tui/oracle.go
git commit -m "test(e2e): prove the drain confirm dialog in BOTH directions, on node 2 only

The n branch is the one that matters and is usually untested: a confirm dialog that
acts on the wrong answer is worse than no dialog. It also asserts node 1's pod count is
unchanged — draining one node must not disturb the trading loop on another, which is
the whole reason a second node was provisioned rather than draining the rig."
```

---

## Task 7: Proof 5 — move region

**Files:** Modify `kanz/test/e2e/tui/workflow_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestMoveRegionRelabelsTheNode(t *testing.T) {
	env := requireEnv(t)
	node2 := waitForNodeReady(t, 2*time.Minute)
	const want = "asia"
	if nodeLabel(t, node2, "topology.kubernetes.io/region") == want {
		kubectl(t, "label", "node", node2, "topology.kubernetes.io/region-")
	}

	s := startTUI(t, env)
	defer s.Close()
	selectNode2(t, s, node2)

	s.Send("m")
	s.WaitFor(t, "egion", 5*time.Second) // matches Region/region without pinning the label
	s.Send(want + "\r")

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if nodeLabel(t, node2, "topology.kubernetes.io/region") == want {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("topology.kubernetes.io/region on %s never became %q", node2, want)
}
```

- [ ] **Step 2: Run it**

Run: `cd ~/kanz/kanz && GOFLAGS=-mod=mod go test ./test/e2e/tui/ -run TestMoveRegion -v -timeout 5m`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add kanz/test/e2e/tui/workflow_test.go
git commit -m "test(e2e): prove move region relabels the node in the cluster

Waits on 'egion' rather than an exact label so the proof survives slice 2's copy
changes; the assertion itself is the node's topology label."
```

---

## Task 8: Proof 6 — venue keys through the form, with a leak check

**Files:** Modify `kanz/test/e2e/tui/workflow_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestVenueKeysWrittenFromTheFormAndNeverLeaked(t *testing.T) {
	env := requireEnv(t)
	const canary = "E2E-CANARY-SECRET-8f3a"
	kubectl(t, "-n", "kanz-services", "delete", "secret", "venue-binance-keys",
		"--ignore-not-found")

	s := startTUI(t, env)
	defer s.Close()

	s.Send("\t\t") // to the API Manager pane
	s.WaitFor(t, "binance", 10*time.Second)
	s.Send("k")
	s.WaitFor(t, "Key", 5*time.Second)
	s.Send("e2e-key\t")
	s.Send(canary + "\r")

	keys := waitForSecret(t, "kanz-services", "venue-binance-keys", 30*time.Second)
	for _, want := range []string{"api-key", "api-secret"} {
		if !contains(keys, want) {
			t.Errorf("secret data keys are %v, missing %q. HYPHENS matter: the dev rig mounts "+
				"the Secret as a plain volume with no CSI mapping, so the data key IS the "+
				"filename the adapter reads.", keys, want)
		}
	}

	// LEAK CHECK, executed rather than assumed.
	if strings.Contains(s.Frames(), canary) {
		t.Error("the submitted credential appeared in a rendered frame — a masked field that " +
			"echoes on any screen is a credential on a shared terminal")
	}
	for _, target := range [][]string{
		{"-n", "kanz-services", "logs", "deploy/api-gateway", "--since=5m"},
		{"-n", "kanz-operator", "logs", "deploy/operator", "--since=5m"},
	} {
		if strings.Contains(kubectl(t, target...), canary) {
			t.Errorf("the credential appeared in %v logs", target)
		}
	}

	kubectl(t, "-n", "kanz-services", "delete", "secret", "venue-binance-keys",
		"--ignore-not-found")
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
```

Add to `oracle.go`:

```go
// waitForSecret waits for a Secret to exist and returns its data keys. Separate from
// secretDataKeys because a write driven through a UI is asynchronous: the form returns
// before the RPC completes, and failing on the first miss would be a race, not a proof.
func waitForSecret(t *testing.T, ns, name string, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := exec.Command("kubectl", "-n", ns, "get", "secret", name,
			"-o", "name").Run(); err == nil {
			return secretDataKeys(t, ns, name)
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("secret %s/%s never appeared within %s — the form did not produce it", ns, name, timeout)
	return nil
}
```

Add `"os/exec"` and `"time"` to `oracle.go`'s imports.

- [ ] **Step 2: Run it**

Run: `cd ~/kanz/kanz && GOFLAGS=-mod=mod go test ./test/e2e/tui/ -run TestVenueKeys -v -timeout 5m`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add kanz/test/e2e/tui/workflow_test.go
git commit -m "test(e2e): prove venue keys are written FROM THE FORM, and never leak

OPS-M2d proved this write over HTTP with curl. This proves the form that is supposed
to call it — the gap the board flagged. The leak check is executed, not assumed: a
canary string is searched for in every captured frame and in both services' logs."
```

---

## Task 9: Proof 7 — live updates from out-of-band change

**Files:** Modify `kanz/test/e2e/tui/workflow_test.go`

- [ ] **Step 1: Write the failing test**

```go
// The poller's default interval is 3s. A bound of two intervals plus one fails on a
// stalled poller instead of waiting indefinitely.
func TestPollerReflectsAnOutOfBandChange(t *testing.T) {
	env := requireEnv(t)
	node2 := waitForNodeReady(t, 2*time.Minute)
	kubectl(t, "uncordon", node2)

	s := startTUI(t, env)
	defer s.Close()
	s.WaitFor(t, node2, 15*time.Second)

	// Change the world WITHOUT touching the TUI.
	kubectl(t, "cordon", node2)

	// The TUI must notice on its own, with no keypress and no restart.
	s.WaitFor(t, "SchedulingDisabled", 12*time.Second)

	kubectl(t, "uncordon", node2)
}
```

- [ ] **Step 2: Run it**

Run: `cd ~/kanz/kanz && GOFLAGS=-mod=mod go test ./test/e2e/tui/ -run TestPollerReflects -v -timeout 5m`
Expected: PASS.

**If it fails:** either the poller is not running, or the status the TUI renders for a cordoned node is not `SchedulingDisabled`. Read the captured frame in the failure output and assert on whatever the TUI *does* render for that state — but only after confirming the cluster really is cordoned, because the alternative finding is that the TUI shows stale state, which is the defect this proof exists to catch.

- [ ] **Step 3: Commit**

```bash
git add kanz/test/e2e/tui/workflow_test.go
git commit -m "test(e2e): prove the poller reflects a change nobody made in the TUI

A static first render is not a live update — that conflation is what OPS-M2c was
reported on. The world is changed with kubectl and the TUI must notice unprompted,
within two poll intervals plus one so a stalled poller fails rather than hangs."
```

---

## Task 10: Model-level coverage for the combinatorial surface

The PTY proofs are slow and need a cluster. Everything that does not need one belongs here.

**Files:**
- Create: `kanz/cmd/universe/model_nav_test.go`, `kanz/cmd/universe/model_validation_test.go`

**Interfaces:** Consumes the existing `stubSource`, `newModel`, `addForm`, `keyForm`.

- [ ] **Step 1: Write the navigation permutation tests**

```go
package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// tab must be a cycle with no dead end, from every starting pane.
func TestTabCyclesThroughAllPanesFromAnyStart(t *testing.T) {
	for start := 0; start < 3; start++ {
		m := newModel(Config{}, stubSource{})
		for i := 0; i < start; i++ {
			u, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
			m = u.(model)
		}
		seen := map[pane]bool{m.active: true}
		for i := 0; i < 3; i++ {
			u, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
			m = u.(model)
			seen[m.active] = true
		}
		if len(seen) != 3 {
			t.Errorf("starting from pane %d, tab reached only %d of 3 panes — a pane an "+
				"operator cannot reach is a feature that does not exist", start, len(seen))
		}
	}
}

// Actions must not fire when no row is selectable.
func TestActionKeysAreInertWithAnEmptyEstate(t *testing.T) {
	for _, key := range []string{"c", "u", "d", "m"} {
		m := newModel(Config{}, stubSource{})
		u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		if cmd != nil {
			t.Errorf("%q issued a command against an empty node list — an action with no "+
				"target must do nothing rather than act on index 0 of nothing", key)
		}
		if u.(model).confirmingDrain {
			t.Errorf("%q opened the drain confirm with no node selected", key)
		}
	}
}
```

- [ ] **Step 2: Run them**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./cmd/universe/ -run 'TestTabCycles|TestActionKeysAreInert' -v`
Expected: PASS, or a real finding to fix.

- [ ] **Step 3: Write the validation tests**

```go
package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// An empty required field must be caught BEFORE a request is issued. Inline
// validation is requirement 7 of the console brief; today the form submits and lets
// the server refuse, which is a round-trip an operator should not have to wait for.
func TestAddFormRefusesAnEmptyRequiredFieldWithoutCallingTheServer(t *testing.T) {
	m := newModel(Config{}, stubSource{})
	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m = u.(model)
	if !m.showForm {
		t.Fatal("pressing a did not open the Add Node form")
	}
	// Submit with every field empty except the prefilled port.
	u2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := u2.(model)
	if cmd != nil && m2.formErr == nil {
		t.Error("the form issued a request with an empty hostname and IP. Validate inline: " +
			"the operator should see the problem next to the field, not as a server error " +
			"after a round trip.")
	}
	if !m2.showForm {
		t.Error("a rejected submit closed the form; the operator must stay in it to fix the " +
			"field rather than retype everything")
	}
}

// A failed action must not silently corrupt the rendered estate.
func TestAFailedActionSurfacesAndLeavesTheEstateAlone(t *testing.T) {
	m := newModel(Config{}, stubSource{msg: fetchMsg{nodes: []nodeRow{{Name: "n1"}}}})
	u, _ := m.Update(fetchMsg{nodes: []nodeRow{{Name: "n1"}}})
	m = u.(model)
	u2, _ := m.Update(actionErrMsg{err: errStub})
	m2 := u2.(model)
	if m2.actionErr == nil {
		t.Error("a failed action produced no visible error — silence is the worst outcome " +
			"for an operator who just pressed a destructive key")
	}
	if len(m2.nodes) != 1 || m2.nodes[0].Name != "n1" {
		t.Error("a failed action mutated the node list; the next poll is the source of truth")
	}
	if !strings.Contains(render(m2), "n1") {
		t.Error("the estate stopped rendering after a failed action")
	}
}
```

Before running, confirm the exact message type and error field names for a failed action in `model.go` (search for `actionErr`) and adjust `actionErrMsg`/`errStub` to match; declare `var errStub = errors.New("stub failure")`.

- [ ] **Step 4: Run them**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./cmd/universe/ -run 'TestAddFormRefuses|TestAFailedAction' -v`
Expected: the first may FAIL — that is the finding. **Inline validation is a requirement of the console brief, and if it does not exist yet, implement it here** (validate in `submitAddForm` before the RPC, set `formErr`, keep `showForm` true).

- [ ] **Step 5: Run the whole universe package**

Run: `cd kanz && GOFLAGS=-mod=mod go test ./cmd/universe/ -count=1`
Expected: `ok`.

- [ ] **Step 6: Commit**

```bash
git add kanz/cmd/universe/model_nav_test.go kanz/cmd/universe/model_validation_test.go kanz/cmd/universe/model.go
git commit -m "test(universe): cover the combinatorial surface the PTY proofs must not

Navigation permutations from every starting pane, action keys against an empty estate,
inline validation before a round trip, and a failed action leaving the rendered estate
intact. These are milliseconds and need no cluster, which is why they belong here
rather than in the PTY harness."
```

---

## Task 11: Capture the demonstration and correct the board

**Files:**
- Create: `docs/superpowers/plans/2026-07-25-tui-e2e-workflow-verification-report.md`
- Modify: `KANZ_TASKS.md`

- [ ] **Step 1: Run the whole suite and capture it**

```bash
cd ~/kanz/kanz && GOFLAGS=-mod=mod go test ./test/e2e/tui/ -v -timeout 30m 2>&1 | tee /tmp/e2e.log
GOFLAGS=-mod=mod go test ./cmd/universe/ -count=1 -v 2>&1 | tail -40 >> /tmp/e2e.log
sh ../tools/validate-board.sh ../KANZ_TASKS.md
```

Expected: every PTY proof PASS, no SKIP.

- [ ] **Step 2: Write the report**

Include, per proof: the workflow, the keys sent, the oracle query, and its observed output. Then a section listing every defect the harness found and the commit that fixed it. Then a section naming what is still NOT proven and why.

- [ ] **Step 3: Update the board**

- Close **S2a** (live join proven) and **S2b** (`ctrl+t` proven).
- Move **OPS-M2c** from `TRANSPORT VERIFIED ONLY` to verified per workflow, listing any that remain gated.
- Move **OPS-M2d** to verified through the TUI, keeping the account-proof gate open.
- Close **OPS-M2e**, or record precisely which proofs did not run.

- [ ] **Step 4: Validate and commit**

```bash
sh tools/validate-board.sh KANZ_TASKS.md
cd kanz && GOFLAGS=-mod=mod go test ./test/arch/ -count=1
cd .. && git add KANZ_TASKS.md docs/superpowers/plans/2026-07-25-tui-e2e-workflow-verification-report.md
git commit -m "docs(board): OPS-M2e — every existing TUI workflow proven from the TUI

Closes S2a and S2b, which had carried a next-action describing the deleted port-forward
path since they were written. OPS-M2c moves off TRANSPORT VERIFIED ONLY per workflow.

The report is the demonstration: for each proof, the keys sent, the oracle query, and
its observed output — plus every defect the harness found, the commit that fixed it,
and what remains unproven and why."
```

---

## Self-Review

**Spec coverage.** Prerequisites 1–3 → Task 2 (host, join Secret, pty dep in Task 1). Two harnesses → Tasks 1 and 10. PTY driver's four operations → Task 1. Gating → Task 2. All seven proofs → Tasks 3–9 in order. Fixes in scope → stated in Tasks 3, 4, 9, 10. "What the harness must not become" → Global Constraints. Definition of done items 1–5 → Task 11.

**Placeholders.** None: `NODE2_NAME` in Task 6 is an explicit substitution with the command to obtain it, and Task 10 Step 3 names the field to confirm and how.

**Type consistency.** `Start(t, bin, args, env)` is used with that signature in Tasks 1, 3. `WaitFor(t, sub, timeout)` consistently takes `t`. `waitForNodeReady` returns the node name, used as such in Tasks 5–9. `nodeSchedulable`/`nodeLabel`/`podsOnNode`/`secretDataKeys`/`kubectlLines`/`waitForSchedulable`/`waitForNoEvictablePods` are each defined once, in Task 2 or where first needed, and reused.

**Fixed during self-review:** Task 8's secret poll was clumsy (`nodeExists(t, "") || true`). Replaced with `waitForSecret`, defined in Task 8 and added to `oracle.go` — a write driven through a UI is asynchronous, so failing on the first miss would have been a race rather than a proof.
