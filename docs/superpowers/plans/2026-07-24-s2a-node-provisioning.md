# S2a Node Provisioning (Add Node → ephemeral k3s-join Job) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the platform's first write path — an operator provisions a new host into the estate from the `universe` TUI, via a one-shot Job that SSHes once and installs the k3s agent, and the node then appears in F0's Nodes pane.

**Architecture:** A new async `operator.v1.AddNode` RPC makes the in-cluster operator service create a short-lived Secret (the bootstrap SSH key) and a one-shot `kanz-provisioner` Job that owns it; the Job SSHes to the target (the sole importer of `crypto/ssh`), runs the k3s-agent install, and exits. Provisioning status is derived from the Job's own status (no separate store). `crypto/ssh` is bounded to the provisioner by a path-scoped arch guard.

**Tech Stack:** Go, `operator.v1` (buf), `k8s.io/client-go` (batch/v1 Jobs + core/v1 Secrets), `golang.org/x/crypto/ssh`, `github.com/charmbracelet/bubbletea` + `lipgloss`.

## Global Constraints

Every task's requirements implicitly include this section.

- **Go toolchain** `go 1.26.1` / `toolchain go1.26.5`; all `go` commands run from `kanz/` with `GOFLAGS=-mod=mod`. **There is no `make` on the dev box** — use `buf generate` directly, and set **`GOTMPDIR="$(pwd)/.gotmp"`** for every `go test` (Windows App Control blocks test binaries in `%TEMP%`).
- **Generated protobuf** imports from `github.com/kanz-eng/kanz-schemas-go/operator/v1`; regenerate with `cd kanz-schemas && buf generate` (not committed).
- **`crypto/ssh` lives in exactly ONE package: `cmd/kanz-provisioner`.** No other file in the module may import anything whose path contains `ssh`. The path-scoped `test/arch/ssh_plane_test.go` guard (Task 2) enforces this; a `crypto/ssh` import anywhere else must fail the build.
- **`golang.org/x/crypto` is already at v0.52.0** (patched past every `x/crypto/ssh` advisory). Do NOT downgrade. `govulncheck ./...` must report **0 reachable** after `crypto/ssh` becomes reachable.
- **SSH is used ONCE, to bootstrap the join.** No `ssh-copy-id`, no persistent key installed, no persistent SSH access. Post-join the node is Kubernetes-managed. The operator-supplied key is a transient bootstrap credential: it lives only in the Job-owned Secret, is never returned by any RPC, never logged.
- **The transient Secret is owned by its Job** via `ownerReferences`, so Kubernetes garbage-collects it with the Job.
- **RBAC growth is bounded and namespaced.** The read-only `operator-node-reader` ClusterRole is untouched. New writes (`jobs`, `secrets`) are a **namespaced Role** in `kanz-operator`; never a verb on `nodes`, never cluster-scoped. The provisioner ServiceAccount has **no** Kubernetes API access.
- **Namespace** is `kanz-operator` throughout. Images are `ghcr.io/kanz-eng/<name>:latest`. Distroless: `FROM gcr.io/distroless/static:nonroot`, `USER nonroot:nonroot` (uid 65532), `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"`, Docker build context = repo root.
- **`ProvisionStatus`** is derived purely from Job status: `Succeeded>0`→`JOINED`, `Failed>0`→`FAILED`, `Active>0`→`INSTALLING`, else `PENDING`.

---

### Task 1: `operator.v1` AddNode + ListProvisions proto + SDK gen

**Files:**
- Modify: `kanz-schemas/proto/operator/v1/operator.proto`
- Regenerates (not committed): `kanz-schemas/gen/go/operator/v1/operator.pb.go`, `operator_grpc.pb.go`

**Interfaces:**
- Produces: `AddNode`/`ListProvisions` RPCs on `OperatorServiceServer`/`Client`; `AddNodeRequest{hostname,ip,ssh_port,ssh_user,ssh_private_key}`, `AddNodeResponse{provision_id,status}`, `ListProvisionsRequest{}`, `ListProvisionsResponse{provisions[]}`, `Provision{id,hostname,status,message}`, enum `ProvisionStatus`.

- [ ] **Step 1: Add the RPCs + messages to the proto**

In `kanz-schemas/proto/operator/v1/operator.proto`, add to the `service OperatorService { ... }` block:

```proto
  // AddNode provisions a new host into the estate: it creates a one-shot Job
  // that SSHes to the target once, installs the k3s agent, and joins. Async —
  // returns a provision_id immediately; poll ListProvisions for progress and
  // watch ListNodes for the joined node.
  rpc AddNode(AddNodeRequest) returns (AddNodeResponse);

  // ListProvisions reports in-flight and recent node provisions.
  rpc ListProvisions(ListProvisionsRequest) returns (ListProvisionsResponse);
```

And append these messages + enum to the file:

```proto
// ProvisionStatus is derived from the provisioning Job's status.
enum ProvisionStatus {
  PROVISION_STATUS_UNSPECIFIED = 0;
  PROVISION_STATUS_PENDING = 1;     // Job created, not yet running
  PROVISION_STATUS_INSTALLING = 2;  // Job active (SSH + k3s install underway)
  PROVISION_STATUS_JOINED = 3;      // Job succeeded — node installed k3s and joined
  PROVISION_STATUS_FAILED = 4;      // Job failed
}

message AddNodeRequest {
  string hostname = 1;
  string ip = 2;
  int32 ssh_port = 3;   // 0 ⇒ 22
  string ssh_user = 4;
  // ssh_private_key is the PEM bootstrap key, used ONCE to install k3s. It is
  // never stored beyond the Job-owned Secret, never returned, never logged.
  bytes ssh_private_key = 5;
}

message AddNodeResponse {
  string provision_id = 1;
  ProvisionStatus status = 2;
}

message ListProvisionsRequest {}

message ListProvisionsResponse {
  repeated Provision provisions = 1;
}

message Provision {
  string id = 1;
  string hostname = 2;
  ProvisionStatus status = 3;
  string message = 4;  // failure reason when FAILED; empty otherwise
}
```

- [ ] **Step 2: Lint**

Run: `cd kanz-schemas && buf lint`
Expected: exit 0, no output.

- [ ] **Step 3: Generate**

Run: `cd kanz-schemas && buf generate`
Expected: `gen/go/operator/v1/operator.pb.go` + `operator_grpc.pb.go` regenerated with the new types.

- [ ] **Step 4: Verify the new types compile**

Run: `cd kanz && GOFLAGS=-mod=mod go build github.com/kanz-eng/kanz-schemas-go/operator/v1`
Expected: exit 0.

- [ ] **Step 5: Commit**

```bash
cd kanz-schemas && git add proto/operator/v1/operator.proto
git commit -m "feat(schemas): operator.v1 AddNode + ListProvisions (node provisioning)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: Bounded `crypto/ssh` — path-scope the SSH guard + the SSH-exec primitive

**Files:**
- Modify: `kanz/test/arch/ssh_plane_test.go` (path-scoped allow)
- Create: `kanz/cmd/kanz-provisioner/sshexec.go`
- Test: `kanz/cmd/kanz-provisioner/sshexec_test.go`
- Modify: `kanz/go.mod`, `kanz/go.sum` (promote `golang.org/x/crypto` to a direct dep)

**Interfaces:**
- Produces: `func sshRun(ctx context.Context, addr, user string, pemKey []byte, cmd string) (string, error)` in `package main` under `cmd/kanz-provisioner`.

**Why the guard change and the import land together:** the guard's anti-rot check fails an allowlist entry that is never matched. The path-scoped allow must be exercised by a real `crypto/ssh` import in the provisioner — so both land in this task.

- [ ] **Step 1: Path-scope the SSH-plane guard**

In `kanz/test/arch/ssh_plane_test.go`, the current guard offends on ANY `ssh` import (empty allowlist). Change it to permit `golang.org/x/crypto/ssh` **only** under `cmd/kanz-provisioner/`. Replace the offence-collection block and the allowlist:

Replace the `sshImportAllowed` declaration:

```go
// sshImportAllowed is now PATH-SCOPED (S2a): golang.org/x/crypto/ssh is permitted
// ONLY inside cmd/kanz-provisioner/, the one-shot Job that bootstraps a node's k3s
// join over SSH. SSH returns to the estate at exactly this one point (KANZ_BRAIN.md,
// "estate is Kubernetes-managed", refined 2026-07-24: the node-join handshake). The
// same import ANYWHERE ELSE is still the cancelled operator plane arriving and
// offends. This is a lead decision (the hybrid-k3s substrate) with a BRAIN amendment,
// per this file's own rule.
//
// Keyed to a (importPath, dirPrefix) pair: the value is the repo-relative directory
// prefix the import is allowed under.
var sshImportAllowed = map[string]string{
	"golang.org/x/crypto/ssh": "cmd/kanz-provisioner/",
}
```

Then, in the walk, change the allow check so it is scoped by the importing file's path. Replace:

```go
			if _, ok := sshImportAllowed[ip]; ok {
				matched[ip] = true
				continue
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				rel = path
			}
			offences = append(offences, offence{importPath: ip, file: filepath.ToSlash(rel)})
```

with:

```go
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				rel = path
			}
			relSlash := filepath.ToSlash(rel)
			if prefix, ok := sshImportAllowed[ip]; ok && strings.HasPrefix(relSlash, prefix) {
				matched[ip] = true
				continue
			}
			offences = append(offences, offence{importPath: ip, file: relSlash})
```

(The anti-rot loop at the end already fails any allowlist entry never matched — so if the provisioner stops importing `crypto/ssh`, or moves out of `cmd/kanz-provisioner/`, the dead entry fails the build. Leave it unchanged.)

- [ ] **Step 2: Run the guard — it must now FAIL (non-vacuity: the allow is unmatched)**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./test/arch/ -run TestNoSSHPlane -v`
Expected: FAIL — the anti-rot check reports `sshImportAllowed has a DEAD entry: "golang.org/x/crypto/ssh"` because nothing imports it yet. This confirms the allow is real, not vacuous. It goes green once Step 4 adds the import.

- [ ] **Step 3: Promote x/crypto to a direct dependency**

Run: `cd kanz && GOFLAGS=-mod=mod go get golang.org/x/crypto/ssh && GOFLAGS=-mod=mod go mod tidy`
Expected: `golang.org/x/crypto` moves out of the `// indirect` block in `go.mod` (still v0.52.0 — do not change the version).

- [ ] **Step 4: Write the SSH-exec primitive**

Create `kanz/cmd/kanz-provisioner/sshexec.go`:

```go
package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

// dialTimeout bounds both the TCP dial and the SSH handshake.
const dialTimeout = 30 * time.Second

// sshRun dials addr ("host:port") over SSH as user, authenticating with the PEM
// private key, runs cmd, and returns its combined stdout+stderr.
//
// HostKeyCallback is InsecureIgnoreHostKey BY DESIGN: this is FIRST CONTACT with a
// freshly-provisioned host whose key we do not and cannot yet know. The bootstrap
// trust is the operator-supplied key plus the ephemeral, RBAC-gated,
// NetworkPolicy-scoped Job this runs in — not TOFU host verification. SSH touches
// this host exactly once (the k3s join); afterward it is Kubernetes-managed and
// never reached over SSH again.
func sshRun(ctx context.Context, addr, user string, pemKey []byte, cmd string) (string, error) {
	signer, err := ssh.ParsePrivateKey(pemKey)
	if err != nil {
		return "", fmt.Errorf("parse private key: %w", err)
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         dialTimeout,
	}

	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", fmt.Errorf("dial %s: %w", addr, err)
	}
	sc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		return "", fmt.Errorf("ssh handshake %s: %w", addr, err)
	}
	client := ssh.NewClient(sc, chans, reqs)
	defer func() { _ = client.Close() }()

	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("new session: %w", err)
	}
	defer func() { _ = sess.Close() }()

	var out bytes.Buffer
	sess.Stdout = &out
	sess.Stderr = &out
	if err := sess.Run(cmd); err != nil {
		return out.String(), fmt.Errorf("run %q: %w", cmd, err)
	}
	return out.String(), nil
}
```

- [ ] **Step 5: Write the test (in-process SSH server — no external sshd)**

Create `kanz/cmd/kanz-provisioner/sshexec_test.go`:

```go
package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// newHostKey generates a throwaway RSA host key for the in-process test server.
func newHostKey(t *testing.T) ssh.Signer {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// clientKeyPEM generates a client keypair and returns (PEM private key, authorized public key).
func clientKeyPEM(t *testing.T) ([]byte, ssh.PublicKey) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	blk, err := ssh.MarshalPrivateKey(k, "")
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(blk), pub
}

// startEchoSSHServer runs a minimal SSH server on a random port that accepts the
// given authorized key and, for any exec request, replies with a fixed banner. It
// returns the listener address and a cleanup func.
func startEchoSSHServer(t *testing.T, authorized ssh.PublicKey, reply string) string {
	t.Helper()
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(authorized.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errUnauthorized
		},
	}
	cfg.AddHostKey(newHostKey(t))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveOne(c, cfg, reply)
		}
	}()
	return ln.Addr().String()
}

func TestSSHRunExecutesCommand(t *testing.T) {
	pem, pub := clientKeyPEM(t)
	addr := startEchoSSHServer(t, pub, "k3s installed ok")

	out, err := sshRun(context.Background(), addr, "root", pem, "install-k3s")
	if err != nil {
		t.Fatalf("sshRun: %v", err)
	}
	if !strings.Contains(out, "k3s installed ok") {
		t.Errorf("output = %q, want it to contain the server reply", out)
	}
}

func TestSSHRunRejectsBadKey(t *testing.T) {
	_, err := sshRun(context.Background(), "127.0.0.1:1", "root", []byte("not a key"), "x")
	if err == nil {
		t.Fatal("expected an error parsing a bad private key")
	}
	if !strings.Contains(err.Error(), "parse private key") {
		t.Errorf("err = %v, want a parse-private-key error", err)
	}
}
```

Then add the small server-side helpers this test needs to `sshexec_test.go` (kept in the test file so they never ship in the binary). Use a **single** `golang.org/x/crypto/ssh` import for the whole file (the server helpers use the same package as `sshRun`); the full test import block is: `context`, `crypto/rand`, `crypto/rsa`, `encoding/pem`, `errors`, `net`, `strings`, `testing`, and `golang.org/x/crypto/ssh`.

```go
var errUnauthorized = errors.New("unauthorized")

// serveOne handshakes one connection and answers every exec request with reply,
// then exit-status 0. Minimal: enough to prove sshRun dials, authenticates, opens a
// session, runs a command, and reads output.
func serveOne(c net.Conn, cfg *ssh.ServerConfig, reply string) {
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		_ = c.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "only session")
			continue
		}
		ch, chReqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range chReqs {
				if req.Type == "exec" {
					_, _ = ch.Write([]byte(reply))
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
					_ = req.Reply(true, nil)
					_ = ch.Close()
					return
				}
				_ = req.Reply(false, nil)
			}
		}()
		_ = sc
	}
}
```

- [ ] **Step 6: Run the guard (now GREEN) and the sshexec test**

Run:
```bash
cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./test/arch/ -run TestNoSSHPlane -v && \
GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./cmd/kanz-provisioner/...
```
Expected: `TestNoSSHPlane` PASS (the allow is now matched by the real provisioner import); sshexec tests PASS.

- [ ] **Step 7: Mutation-prove the guard is still bounded**

Temporarily add `import _ "golang.org/x/crypto/ssh"` to `kanz/services/operator/internal/estate/estate.go`, run `TestNoSSHPlane`.
Expected: FAIL — "the CANCELLED SSH plane is back in the build: services/operator/internal/estate/estate.go imports golang.org/x/crypto/ssh". Revert the import; re-run: PASS.

- [ ] **Step 8: Confirm govulncheck is 0-reachable**

Run: `cd kanz && GOFLAGS=-mod=mod go run golang.org/x/vuln/cmd/govulncheck@latest ./cmd/kanz-provisioner/...`
Expected: no reachable vulnerabilities (x/crypto v0.52.0 is patched). If any reachable vuln is reported, STOP and report it — do not proceed.

- [ ] **Step 9: Commit**

```bash
cd kanz && git add test/arch/ssh_plane_test.go cmd/kanz-provisioner/sshexec.go cmd/kanz-provisioner/sshexec_test.go go.mod go.sum
git commit -m "feat(provisioner): bounded crypto/ssh exec + path-scoped ssh-plane guard

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: The Joiner seam + k3s install + `kanz-provisioner` main

**Files:**
- Create: `kanz/cmd/kanz-provisioner/join.go`
- Test: `kanz/cmd/kanz-provisioner/join_test.go`
- Create: `kanz/cmd/kanz-provisioner/main.go`

**Interfaces:**
- Consumes: `sshRun` (Task 2).
- Produces: `type Target struct{ Addr, User string; Key []byte }`; `type K3sJoin struct{ ServerURL, Token string }`; `type Joiner interface { Join(ctx context.Context, t Target, k K3sJoin) error }`; `sshJoiner` (real, uses `sshRun`); `main()` reading env + the mounted key.

- [ ] **Step 1: Write the failing Joiner test**

Create `kanz/cmd/kanz-provisioner/join_test.go`:

```go
package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeRunner captures the command sshJoiner would run, so we can assert the k3s
// install line without a network.
type fakeRunner struct {
	gotCmd string
	out    string
	err    error
}

func (f *fakeRunner) run(_ context.Context, _, _ string, _ []byte, cmd string) (string, error) {
	f.gotCmd = cmd
	return f.out, f.err
}

func TestSSHJoinerRunsK3sInstall(t *testing.T) {
	fr := &fakeRunner{out: "ok"}
	j := &sshJoiner{run: fr.run}
	err := j.Join(context.Background(),
		Target{Addr: "10.0.0.5:22", User: "root", Key: []byte("k")},
		K3sJoin{ServerURL: "https://cp:6443", Token: "tok"})
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	for _, want := range []string{"get.k3s.io", "K3S_URL='https://cp:6443'", "K3S_TOKEN='tok'", "agent"} {
		if !strings.Contains(fr.gotCmd, want) {
			t.Errorf("install cmd %q missing %q", fr.gotCmd, want)
		}
	}
}

func TestSSHJoinerPropagatesFailure(t *testing.T) {
	fr := &fakeRunner{err: errors.New("dial refused")}
	j := &sshJoiner{run: fr.run}
	if err := j.Join(context.Background(), Target{Addr: "x:22", User: "root", Key: []byte("k")}, K3sJoin{}); err == nil {
		t.Fatal("expected the runner error to propagate")
	}
}
```

- [ ] **Step 2: Run — verify it fails**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./cmd/kanz-provisioner/... -run TestSSHJoiner`
Expected: FAIL — `undefined: sshJoiner` / `Joiner`.

- [ ] **Step 3: Write the Joiner**

Create `kanz/cmd/kanz-provisioner/join.go`:

```go
package main

import (
	"context"
	"fmt"
)

// Target is the host to provision.
type Target struct {
	Addr string // "ip:port"
	User string
	Key  []byte // PEM bootstrap key, used once
}

// K3sJoin is the estate's control-plane join coordinate.
type K3sJoin struct {
	ServerURL string
	Token     string
}

// Joiner installs the k3s agent on a target and joins it to the estate.
type Joiner interface {
	Join(ctx context.Context, t Target, k K3sJoin) error
}

// runFunc is the SSH-exec seam (sshRun in production; a fake in tests).
type runFunc func(ctx context.Context, addr, user string, key []byte, cmd string) (string, error)

// sshJoiner joins by running the official k3s install script over one SSH session.
type sshJoiner struct {
	run runFunc
}

func newSSHJoiner() *sshJoiner { return &sshJoiner{run: sshRun} }

// k3sInstallCmd is the agent-join one-liner. Single-quoted values so a URL/token
// cannot break the shell line; the script is idempotent (k3s re-runs are safe).
func k3sInstallCmd(k K3sJoin) string {
	return fmt.Sprintf(
		"curl -sfL https://get.k3s.io | K3S_URL='%s' K3S_TOKEN='%s' sh -s - agent",
		k.ServerURL, k.Token)
}

func (j *sshJoiner) Join(ctx context.Context, t Target, k K3sJoin) error {
	out, err := j.run(ctx, t.Addr, t.User, t.Key, k3sInstallCmd(k))
	if err != nil {
		return fmt.Errorf("k3s agent install failed: %w (output: %s)", err, out)
	}
	return nil
}

// compile-time assertion.
var _ Joiner = (*sshJoiner)(nil)
```

- [ ] **Step 4: Run — verify it passes**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./cmd/kanz-provisioner/... -run TestSSHJoiner`
Expected: PASS.

- [ ] **Step 5: Write the provisioner entrypoint**

Create `kanz/cmd/kanz-provisioner/main.go`:

```go
// kanz-provisioner is a ONE-SHOT Job that provisions a single host into the estate:
// it SSHes once with an operator-supplied bootstrap key, installs the k3s agent, and
// joins. It is the sole importer of crypto/ssh in the module (bounded to the node-join
// handshake). It reads all inputs from env + a mounted Secret, does its work, and exits
// 0 on join / non-zero on failure — the operator service derives status from the Job.
package main

import (
	"context"
	"fmt"
	"os"
	"time"
)

// provisionTimeout bounds the whole provisioning attempt.
const provisionTimeout = 10 * time.Minute

func main() {
	if err := run(newSSHJoiner()); err != nil {
		fmt.Fprintln(os.Stderr, "kanz-provisioner: "+err.Error())
		os.Exit(1)
	}
}

// run reads config and drives the Joiner. Split from main so a test can pass a fake
// Joiner (env-driven), keeping main() a thin shell.
func run(j Joiner) error {
	addr := os.Getenv("PROVISION_TARGET_ADDR") // "ip:port"
	user := os.Getenv("PROVISION_SSH_USER")
	serverURL := os.Getenv("K3S_SERVER_URL")
	token := os.Getenv("K3S_TOKEN")
	keyPath := envOr("PROVISION_SSH_KEY_FILE", "/etc/provision/ssh_key")

	if addr == "" || user == "" || serverURL == "" || token == "" {
		return fmt.Errorf("missing required env (PROVISION_TARGET_ADDR, PROVISION_SSH_USER, K3S_SERVER_URL, K3S_TOKEN)")
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("read bootstrap key %s: %w", keyPath, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), provisionTimeout)
	defer cancel()
	return j.Join(ctx, Target{Addr: addr, User: user, Key: key}, K3sJoin{ServerURL: serverURL, Token: token})
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
```

- [ ] **Step 6: Add a run() test (fake Joiner via env)**

Append to `kanz/cmd/kanz-provisioner/join_test.go`:

```go
type fakeJoiner struct {
	called bool
	err    error
}

func (f *fakeJoiner) Join(context.Context, Target, K3sJoin) error {
	f.called = true
	return f.err
}

func TestRunRequiresEnv(t *testing.T) {
	t.Setenv("PROVISION_TARGET_ADDR", "")
	if err := run(&fakeJoiner{}); err == nil {
		t.Fatal("expected an error when required env is missing")
	}
}

func TestRunReadsKeyAndJoins(t *testing.T) {
	dir := t.TempDir()
	keyFile := dir + "/key"
	if err := os.WriteFile(keyFile, []byte("PEM"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROVISION_TARGET_ADDR", "10.0.0.5:22")
	t.Setenv("PROVISION_SSH_USER", "root")
	t.Setenv("K3S_SERVER_URL", "https://cp:6443")
	t.Setenv("K3S_TOKEN", "tok")
	t.Setenv("PROVISION_SSH_KEY_FILE", keyFile)

	fj := &fakeJoiner{}
	if err := run(fj); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !fj.called {
		t.Fatal("expected Join to be called")
	}
}
```

Add `"os"` and `"testing"` to the test imports if not already present.

- [ ] **Step 7: Run the provisioner tests**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./cmd/kanz-provisioner/...`
Expected: PASS (sshexec + join + run tests).

- [ ] **Step 8: Commit**

```bash
cd kanz && git add cmd/kanz-provisioner/join.go cmd/kanz-provisioner/join_test.go cmd/kanz-provisioner/main.go
git commit -m "feat(provisioner): Joiner seam, k3s install, one-shot entrypoint

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: Operator provision orchestrator (transient Secret + Job, status from Job)

**Files:**
- Create: `kanz/services/operator/internal/provision/provision.go`
- Test: `kanz/services/operator/internal/provision/provision_test.go`

**Interfaces:**
- Consumes: `k8s.io/client-go` `kubernetes.Interface`.
- Produces:
  - `type Config struct { Namespace, ProvisionerImage, K3sServerURL, K3sToken string }`
  - `type Request struct { Hostname, IP string; SSHPort int32; SSHUser string; SSHKey []byte }`
  - `type Provisioner struct { ... }`, `func New(cs kubernetes.Interface, cfg Config) *Provisioner`
  - `func (p *Provisioner) AddNode(ctx, Request) (id string, err error)`
  - `type Status int` (`StatusPending/Installing/Joined/Failed`) and `type Provision struct { ID, Hostname string; Status Status; Message string }`
  - `func (p *Provisioner) List(ctx) ([]Provision, error)`

- [ ] **Step 1: Write the failing test**

Create `kanz/services/operator/internal/provision/provision_test.go`:

```go
package provision

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func cfg() Config {
	return Config{Namespace: "kanz-operator", ProvisionerImage: "ghcr.io/kanz-eng/kanz-provisioner:latest",
		K3sServerURL: "https://cp:6443", K3sToken: "tok"}
}

func TestAddNodeCreatesJobAndOwnedSecret(t *testing.T) {
	cs := fake.NewSimpleClientset()
	p := New(cs, cfg())
	id, err := p.AddNode(context.Background(), Request{
		Hostname: "london", IP: "10.0.0.5", SSHPort: 22, SSHUser: "root", SSHKey: []byte("PEM")})
	if err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	job, err := cs.BatchV1().Jobs("kanz-operator").Get(context.Background(), id, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("job not created: %v", err)
	}
	// The Secret must exist and be OWNED by the Job (GC cascades).
	secList, _ := cs.CoreV1().Secrets("kanz-operator").List(context.Background(), metav1.ListOptions{})
	if len(secList.Items) != 1 {
		t.Fatalf("want 1 secret, got %d", len(secList.Items))
	}
	sec := secList.Items[0]
	if string(sec.Data["ssh_key"]) != "PEM" {
		t.Errorf("secret does not carry the bootstrap key")
	}
	owned := false
	for _, or := range sec.OwnerReferences {
		if or.Kind == "Job" && or.Name == job.Name {
			owned = true
		}
	}
	if !owned {
		t.Errorf("secret is not owned by the job — it could orphan")
	}
	// The key must NOT appear in the Job's pod spec in plaintext (only mounted from the Secret).
	for _, c := range job.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Value == "PEM" {
				t.Errorf("bootstrap key leaked into Job env in plaintext")
			}
		}
	}
}

func TestListMapsJobStatus(t *testing.T) {
	cs := fake.NewSimpleClientset(
		provJob("p-joined", "london", batchv1.JobStatus{Succeeded: 1}),
		provJob("p-failed", "tokyo", batchv1.JobStatus{Failed: 1}),
		provJob("p-running", "paris", batchv1.JobStatus{Active: 1}),
	)
	got, err := New(cs, cfg()).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byID := map[string]Provision{}
	for _, pr := range got {
		byID[pr.ID] = pr
	}
	if byID["p-joined"].Status != StatusJoined {
		t.Errorf("joined = %v", byID["p-joined"].Status)
	}
	if byID["p-failed"].Status != StatusFailed {
		t.Errorf("failed = %v", byID["p-failed"].Status)
	}
	if byID["p-running"].Status != StatusInstalling {
		t.Errorf("running = %v", byID["p-running"].Status)
	}
	if byID["p-joined"].Hostname != "london" {
		t.Errorf("hostname not surfaced")
	}
}

func provJob(name, host string, st batchv1.JobStatus) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "kanz-operator",
			Labels:      map[string]string{"app.kubernetes.io/component": "node-provisioner"},
			Annotations: map[string]string{"kanz.io/provision-hostname": host},
		},
		Status: st,
	}
}
```

- [ ] **Step 2: Run — verify it fails**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./services/operator/internal/provision/...`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write the orchestrator**

Create `kanz/services/operator/internal/provision/provision.go`:

```go
// Package provision is the operator service's node-provisioning orchestrator: it
// turns an AddNode request into a short-lived Secret (the bootstrap SSH key) and a
// one-shot kanz-provisioner Job that owns it, and it derives provisioning status
// from the Jobs' own status. It holds no SSH itself — crypto/ssh lives only in the
// provisioner Job (cmd/kanz-provisioner).
package provision

import (
	"context"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
)

const (
	componentLabel = "app.kubernetes.io/component"
	componentValue = "node-provisioner"
	hostnameAnnot  = "kanz.io/provision-hostname"
	keySecretKey   = "ssh_key"
	keyMountPath   = "/etc/provision"
)

// Config is the orchestrator's deploy-time configuration.
type Config struct {
	Namespace        string
	ProvisionerImage string
	K3sServerURL     string
	K3sToken         string
}

// Request is one AddNode call, decoded from the RPC.
type Request struct {
	Hostname string
	IP       string
	SSHPort  int32
	SSHUser  string
	SSHKey   []byte
}

// Status is the coarse provisioning status derived from a Job.
type Status int

const (
	StatusPending Status = iota
	StatusInstalling
	StatusJoined
	StatusFailed
)

// Provision is one node provisioning, surfaced to ListProvisions.
type Provision struct {
	ID       string
	Hostname string
	Status   Status
	Message  string
}

// Provisioner creates the Secret+Job and reads status back.
type Provisioner struct {
	cs  kubernetes.Interface
	cfg Config
}

func New(cs kubernetes.Interface, cfg Config) *Provisioner { return &Provisioner{cs: cs, cfg: cfg} }

// AddNode creates the transient Secret and the one-shot Job, then points the Secret's
// ownerReference at the Job so Kubernetes GCs the key with the Job. Returns the Job name
// as the provision id.
func (p *Provisioner) AddNode(ctx context.Context, r Request) (string, error) {
	if r.IP == "" || r.SSHUser == "" || len(r.SSHKey) == 0 {
		return "", fmt.Errorf("ip, ssh_user and ssh_private_key are required")
	}
	port := r.SSHPort
	if port == 0 {
		port = 22
	}
	name := "provision-" + sanitize(r.Hostname) + "-" + rand.String(5)

	// 1. Create the Job FIRST. Its UID is assigned synchronously on Create, so the
	//    Secret can then be created ALREADY owner-referenced — there is never a moment
	//    when a credential-bearing Secret exists un-owned. A crash after this but before
	//    the Secret leaves NO key material to orphan; the pod merely fails to mount the
	//    not-yet-created Secret and the Job self-expires at ActiveDeadlineSeconds.
	job, err := p.cs.BatchV1().Jobs(p.cfg.Namespace).Create(ctx, p.jobSpec(name, r, port), metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("create provisioning job: %w", err)
	}

	// 2. Create the bootstrap-key Secret already owned by the Job (Kubernetes GCs it with
	//    the Job; it can never be orphaned). If this fails, best-effort delete the Job so a
	//    doomed mount-pending pod does not linger for the full deadline.
	_, err = p.cs.CoreV1().Secrets(p.cfg.Namespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: p.cfg.Namespace,
			Labels:          map[string]string{componentLabel: componentValue},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))},
		},
		Data: map[string][]byte{keySecretKey: r.SSHKey},
	}, metav1.CreateOptions{})
	if err != nil {
		_ = p.cs.BatchV1().Jobs(p.cfg.Namespace).Delete(ctx, job.Name, metav1.DeleteOptions{})
		return "", fmt.Errorf("create bootstrap secret: %w", err)
	}
	return job.Name, nil
}

func (p *Provisioner) jobSpec(name string, r Request, port int32) *batchv1.Job {
	var backoff int32 = 0    // one attempt; a retry would re-SSH, and the operator re-issues AddNode
	var deadline int64 = 900 // 15m hard cap on a provisioning attempt — a belt to sshRun's ctx, so a
	//                          wedged Job cannot linger indefinitely holding the bootstrap-key Secret.
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: p.cfg.Namespace,
			Labels:      map[string]string{componentLabel: componentValue},
			Annotations: map[string]string{hostnameAnnot: r.Hostname},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoff,
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: "kanz-node-provisioner",
					RestartPolicy:      corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr(true), RunAsUser: ptr64(65532),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:  "provisioner",
						Image: p.cfg.ProvisionerImage,
						Env: []corev1.EnvVar{
							{Name: "PROVISION_TARGET_ADDR", Value: fmt.Sprintf("%s:%d", r.IP, port)},
							{Name: "PROVISION_SSH_USER", Value: r.SSHUser},
							{Name: "K3S_SERVER_URL", Value: p.cfg.K3sServerURL},
							{Name: "K3S_TOKEN", Value: p.cfg.K3sToken},
							{Name: "PROVISION_SSH_KEY_FILE", Value: keyMountPath + "/" + keySecretKey},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: "bootstrap-key", MountPath: keyMountPath, ReadOnly: true}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr(false), ReadOnlyRootFilesystem: ptr(true),
							RunAsNonRoot: ptr(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "bootstrap-key",
						VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
							SecretName: name, DefaultMode: ptr32(0o400)}},
					}},
				},
			},
		},
	}
}

// List returns every provisioning Job's status.
func (p *Provisioner) List(ctx context.Context) ([]Provision, error) {
	jobs, err := p.cs.BatchV1().Jobs(p.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: componentLabel + "=" + componentValue})
	if err != nil {
		return nil, fmt.Errorf("list provisioning jobs: %w", err)
	}
	out := make([]Provision, 0, len(jobs.Items))
	for i := range jobs.Items {
		j := &jobs.Items[i]
		out = append(out, Provision{
			ID:       j.Name,
			Hostname: j.Annotations[hostnameAnnot],
			Status:   statusOf(j),
			Message:  failureMessage(j),
		})
	}
	return out, nil
}

func statusOf(j *batchv1.Job) Status {
	switch {
	case j.Status.Succeeded > 0:
		return StatusJoined
	case j.Status.Failed > 0:
		return StatusFailed
	case j.Status.Active > 0:
		return StatusInstalling
	default:
		return StatusPending
	}
}

func failureMessage(j *batchv1.Job) string {
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return c.Message
		}
	}
	return ""
}

// sanitize lowercases and keeps DNS-label-safe chars so the Job name is valid.
func sanitize(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "node"
	}
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

func ptr(b bool) *bool     { return &b }
func ptr32(i int32) *int32 { return &i }
func ptr64(i int64) *int64 { return &i }
```

- [ ] **Step 4: Run — verify it passes**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./services/operator/internal/provision/...`
Expected: PASS (both tests).

- [ ] **Step 5: Commit**

```bash
cd kanz && git add services/operator/internal/provision/
git commit -m "feat(operator): provisioning orchestrator — Job-owned transient Secret + status

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 5: Operator gRPC — AddNode + ListProvisions handlers + wiring

**Files:**
- Modify: `kanz/services/operator/internal/grpcsrv/server.go`
- Test: `kanz/services/operator/internal/grpcsrv/server_test.go`
- Modify: `kanz/services/operator/internal/config/config.go` (+ test)
- Modify: `kanz/services/operator/cmd/operator/main.go`

**Interfaces:**
- Consumes: `provision.Provisioner` (Task 4) via a small interface; `operatorpb` (Task 1).
- Produces: extended `grpcsrv.New(reader estate.Reader, prov Provisioner)`; `AddNode`/`ListProvisions` methods.

- [ ] **Step 1: Write the failing handler test**

Add to `kanz/services/operator/internal/grpcsrv/server_test.go`:

```go
type stubProvisioner struct {
	gotReq     provision.Request
	id         string
	provisions []provision.Provision
	err        error
}

func (s *stubProvisioner) AddNode(_ context.Context, r provision.Request) (string, error) {
	s.gotReq = r
	return s.id, s.err
}
func (s *stubProvisioner) List(context.Context) ([]provision.Provision, error) {
	return s.provisions, s.err
}

func TestAddNodeDecodesRequestAndReturnsID(t *testing.T) {
	sp := &stubProvisioner{id: "provision-london-abcde"}
	srv := NewWithProvisioner(stubReader{}, sp)
	resp, err := srv.AddNode(context.Background(), &operatorpb.AddNodeRequest{
		Hostname: "london", Ip: "10.0.0.5", SshPort: 22, SshUser: "root", SshPrivateKey: []byte("PEM")})
	if err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if resp.GetProvisionId() != "provision-london-abcde" {
		t.Errorf("provision_id = %q", resp.GetProvisionId())
	}
	if sp.gotReq.IP != "10.0.0.5" || string(sp.gotReq.SSHKey) != "PEM" {
		t.Errorf("request not decoded: %+v", sp.gotReq)
	}
}

func TestAddNodeRejectsEmptyKey(t *testing.T) {
	srv := NewWithProvisioner(stubReader{}, &stubProvisioner{})
	_, err := srv.AddNode(context.Background(), &operatorpb.AddNodeRequest{Ip: "10.0.0.5", SshUser: "root"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for empty key, got %v", err)
	}
}

func TestListProvisionsMapsStatus(t *testing.T) {
	sp := &stubProvisioner{provisions: []provision.Provision{
		{ID: "p1", Hostname: "london", Status: provision.StatusJoined},
		{ID: "p2", Hostname: "tokyo", Status: provision.StatusFailed, Message: "dial refused"},
	}}
	srv := NewWithProvisioner(stubReader{}, sp)
	resp, err := srv.ListProvisions(context.Background(), &operatorpb.ListProvisionsRequest{})
	if err != nil {
		t.Fatalf("ListProvisions: %v", err)
	}
	if len(resp.GetProvisions()) != 2 {
		t.Fatalf("want 2, got %d", len(resp.GetProvisions()))
	}
	if resp.GetProvisions()[0].GetStatus() != operatorpb.ProvisionStatus_PROVISION_STATUS_JOINED {
		t.Errorf("p1 status = %v", resp.GetProvisions()[0].GetStatus())
	}
	if resp.GetProvisions()[1].GetMessage() != "dial refused" {
		t.Errorf("p2 message not surfaced")
	}
}
```

Add imports to the test file: `"github.com/kanz-eng/kanz/services/operator/internal/provision"`, `"google.golang.org/grpc/codes"`, `"google.golang.org/grpc/status"`.

- [ ] **Step 2: Run — verify it fails**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./services/operator/internal/grpcsrv/...`
Expected: FAIL — `undefined: NewWithProvisioner`.

- [ ] **Step 3: Extend the server**

In `kanz/services/operator/internal/grpcsrv/server.go`, add a `Provisioner` interface, extend `Server`, and add the two handlers. Add near the top (after imports):

```go
// Provisioner is the node-provisioning surface the operator gRPC depends on
// (provision.Provisioner satisfies it). Kept as an interface so the handler is
// unit-tested against a stub with no Kubernetes client.
type Provisioner interface {
	AddNode(ctx context.Context, r provision.Request) (string, error)
	List(ctx context.Context) ([]provision.Provision, error)
}
```

Change the `Server` struct and constructors:

```go
type Server struct {
	operatorpb.UnimplementedOperatorServiceServer
	reader estate.Reader
	prov   Provisioner
}

// New returns a read-only Server (no provisioning). Retained for callers/tests that
// only exercise ListNodes/ListClusters.
func New(r estate.Reader) *Server { return &Server{reader: r} }

// NewWithProvisioner returns a Server with the write path wired.
func NewWithProvisioner(r estate.Reader, p Provisioner) *Server {
	return &Server{reader: r, prov: p}
}
```

Add the handlers (and imports `provision`, `codes`, `status`):

```go
func (s *Server) AddNode(ctx context.Context, req *operatorpb.AddNodeRequest) (*operatorpb.AddNodeResponse, error) {
	if s.prov == nil {
		return nil, status.Error(codes.Unimplemented, "provisioning not configured")
	}
	if req.GetIp() == "" || req.GetSshUser() == "" || len(req.GetSshPrivateKey()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "ip, ssh_user and ssh_private_key are required")
	}
	id, err := s.prov.AddNode(ctx, provision.Request{
		Hostname: req.GetHostname(), IP: req.GetIp(), SSHPort: req.GetSshPort(),
		SSHUser: req.GetSshUser(), SSHKey: req.GetSshPrivateKey(),
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &operatorpb.AddNodeResponse{ProvisionId: id, Status: operatorpb.ProvisionStatus_PROVISION_STATUS_PENDING}, nil
}

func (s *Server) ListProvisions(ctx context.Context, _ *operatorpb.ListProvisionsRequest) (*operatorpb.ListProvisionsResponse, error) {
	if s.prov == nil {
		return &operatorpb.ListProvisionsResponse{}, nil
	}
	ps, err := s.prov.List(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := make([]*operatorpb.Provision, 0, len(ps))
	for _, p := range ps {
		out = append(out, &operatorpb.Provision{
			Id: p.ID, Hostname: p.Hostname, Status: provStatus(p.Status), Message: p.Message,
		})
	}
	return &operatorpb.ListProvisionsResponse{Provisions: out}, nil
}

func provStatus(s provision.Status) operatorpb.ProvisionStatus {
	switch s {
	case provision.StatusInstalling:
		return operatorpb.ProvisionStatus_PROVISION_STATUS_INSTALLING
	case provision.StatusJoined:
		return operatorpb.ProvisionStatus_PROVISION_STATUS_JOINED
	case provision.StatusFailed:
		return operatorpb.ProvisionStatus_PROVISION_STATUS_FAILED
	default:
		return operatorpb.ProvisionStatus_PROVISION_STATUS_PENDING
	}
}
```

- [ ] **Step 4: Run — verify it passes**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./services/operator/internal/grpcsrv/...`
Expected: PASS (existing ListNodes/ListClusters tests + the three new ones).

- [ ] **Step 5: Extend config with the provisioning knobs**

In `kanz/services/operator/internal/config/config.go`, add fields + env reads:

```go
	// Node-provisioning (S2a). Empty ProvisionerImage ⇒ AddNode is unconfigured
	// and returns Unimplemented (read-only deployment).
	ProvisionerImage string
	K3sServerURL     string
	K3sToken         string
```

In `Load()`:

```go
		ProvisionerImage: os.Getenv("OPERATOR_PROVISIONER_IMAGE"),
		K3sServerURL:     os.Getenv("OPERATOR_K3S_SERVER_URL"),
		K3sToken:         os.Getenv("OPERATOR_K3S_TOKEN"),
```

Add to `config_test.go`:

```go
func TestLoadProvisioningEnv(t *testing.T) {
	t.Setenv("OPERATOR_PROVISIONER_IMAGE", "ghcr.io/kanz-eng/kanz-provisioner:latest")
	t.Setenv("OPERATOR_K3S_SERVER_URL", "https://cp:6443")
	t.Setenv("OPERATOR_K3S_TOKEN", "tok")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProvisionerImage == "" || cfg.K3sServerURL == "" || cfg.K3sToken == "" {
		t.Errorf("provisioning env not loaded: %+v", cfg)
	}
}
```

- [ ] **Step 6: Wire it in main.go**

In `kanz/services/operator/cmd/operator/main.go`, replace the registration line
`grpcsrv.New(estate.NewK8s(cs)).Register(grpcSrv)` with a provisioning-aware wiring:

```go
	reader := estate.NewK8s(cs)
	var srv *grpcsrv.Server
	if cfg.ProvisionerImage != "" {
		prov := provision.New(cs, provision.Config{
			Namespace:        namespaceOr("kanz-operator"),
			ProvisionerImage: cfg.ProvisionerImage,
			K3sServerURL:     cfg.K3sServerURL,
			K3sToken:         cfg.K3sToken,
		})
		srv = grpcsrv.NewWithProvisioner(reader, prov)
		logger.Info("node provisioning enabled", "image", cfg.ProvisionerImage)
	} else {
		srv = grpcsrv.New(reader)
		logger.Warn("no OPERATOR_PROVISIONER_IMAGE — AddNode disabled (read-only)")
	}
	srv.Register(grpcSrv)
```

Add imports `"github.com/kanz-eng/kanz/services/operator/internal/provision"`, and a helper:

```go
// namespaceOr returns the pod's namespace (downward API file) or a default. The
// operator provisions in its own namespace.
func namespaceOr(def string) string {
	if b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(b)); ns != "" {
			return ns
		}
	}
	return def
}
```

Add `"strings"` to the main.go imports.

- [ ] **Step 7: Build + test the operator service**

Run:
```bash
cd kanz && GOFLAGS=-mod=mod go build ./services/operator/... && \
GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./services/operator/...
```
Expected: build exit 0; all operator package tests pass.

- [ ] **Step 8: Commit**

```bash
cd kanz && git add services/operator/internal/grpcsrv/ services/operator/internal/config/ services/operator/cmd/operator/main.go
git commit -m "feat(operator): AddNode + ListProvisions gRPC handlers, provisioning wiring

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 6: Manifests + RBAC + NetworkPolicies + provisioner image

**Files:**
- Modify: `kanz/infra/deploy/operator-deploy.yaml`
- Create: `kanz/cmd/kanz-provisioner/Dockerfile`
- Modify: `.github/workflows/build.yml`

**Interfaces:**
- Produces: the `operator-provisioner` namespaced Role + binding; the `kanz-node-provisioner` SA (no cluster access); NetworkPolicies; the provisioner image build.

- [ ] **Step 1: Add RBAC + SA + NetworkPolicies to the operator manifest**

Append to `kanz/infra/deploy/operator-deploy.yaml` (multi-doc). The operator SA now also needs namespaced write on jobs/secrets:

```yaml
---
# S2a: the operator service creates one-shot provisioning Jobs + their transient
# Secrets in its OWN namespace. Namespaced Role (never a ClusterRole) — these are
# namespaced resources, and the operator must never gain a verb on cluster-scoped
# nodes (a joining node self-registers).
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: operator-provisioner
  namespace: kanz-operator
  labels: { app.kubernetes.io/part-of: kanz }
rules:
  - apiGroups: ["batch"]
    resources: ["jobs"]
    verbs: ["create", "delete", "get", "list"]
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["create", "delete", "get", "update"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: operator-provisioner
  namespace: kanz-operator
  labels: { app.kubernetes.io/part-of: kanz }
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: Role, name: operator-provisioner }
subjects:
  - { kind: ServiceAccount, name: operator, namespace: kanz-operator }
---
# The provisioning Job's identity. It needs NO Kubernetes API access — only outbound
# SSH — so it is bound to nothing.
apiVersion: v1
kind: ServiceAccount
metadata:
  name: kanz-node-provisioner
  namespace: kanz-operator
  labels: { app.kubernetes.io/part-of: kanz }
automountServiceAccountToken: false
---
# Deny-by-default ingress to the operator pod, closing the F0+S1 residual gap: the
# plaintext gRPC listener (0.0.0.0:9090) was reachable by any in-cluster pod. Only the
# health port is left ingressable (kubelet probes); gRPC is reached solely via
# kubeconfig-gated port-forward, which does not traverse a NetworkPolicy.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: operator-ingress
  namespace: kanz-operator
  labels: { app.kubernetes.io/part-of: kanz }
spec:
  podSelector: { matchLabels: { app: operator } }
  policyTypes: ["Ingress"]
  ingress:
    - ports:
        - { protocol: TCP, port: 8091 }
---
# The provisioner Job egresses to DNS + arbitrary target SSH + the k3s control plane.
# It is ephemeral; this policy scopes what a compromised provisioning run could reach.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: node-provisioner-egress
  namespace: kanz-operator
  labels: { app.kubernetes.io/part-of: kanz }
spec:
  podSelector: { matchLabels: { app.kubernetes.io/component: node-provisioner } }
  policyTypes: ["Egress"]
  egress:
    - ports:
        - { protocol: UDP, port: 53 }
        - { protocol: TCP, port: 53 }
    - ports:
        - { protocol: TCP, port: 22 }
        - { protocol: TCP, port: 6443 }
        - { protocol: TCP, port: 443 }
```

Also add the provisioning env to the operator Deployment's container (so `AddNode` is enabled). In the `containers: [{ name: operator, ... }]` block, add an `env:` (sourced from a Secret the infra provides — referenced, not inlined):

```yaml
          env:
            - name: OPERATOR_PROVISIONER_IMAGE
              value: ghcr.io/kanz-eng/kanz-provisioner:latest
            - name: OPERATOR_K3S_SERVER_URL
              valueFrom: { secretKeyRef: { name: operator-k3s-join, key: server_url, optional: true } }
            - name: OPERATOR_K3S_TOKEN
              valueFrom: { secretKeyRef: { name: operator-k3s-join, key: token, optional: true } }
```

The `operator-k3s-join` Secret is infra-provided (out of scope here). The `secretKeyRef`s are **`optional: true`** so the operator pod still starts without it — a deployment with no k3s config keeps the read-only surface working; only a provisioning attempt then fails cleanly (the Job runs with an empty server URL/token and reports FAILED), rather than the whole pod refusing to start. Do NOT inline the token value.

- [ ] **Step 2: Write the provisioner Dockerfile**

Create `kanz/cmd/kanz-provisioner/Dockerfile` (mirrors `kanz/cmd/kanz-halt/Dockerfile`; build context = repo root):

```dockerfile
# syntax=docker/dockerfile:1
#
# kanz-provisioner: the one-shot node-join Job (S2a). The SOLE binary importing
# crypto/ssh. Distroless static. Build context is the repo ROOT:
#   docker build -f kanz/cmd/kanz-provisioner/Dockerfile .
FROM golang:1.26.5 AS build
WORKDIR /src
ENV GOFLAGS=-mod=mod

COPY kanz-schemas/gen/go/ ./kanz-schemas/gen/go/
COPY kanz/go.mod kanz/go.sum ./kanz/
WORKDIR /src/kanz
RUN go mod download

COPY kanz/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
      -o /out/kanz-provisioner ./cmd/kanz-provisioner

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/kanz-provisioner /kanz-provisioner
USER nonroot:nonroot
ENTRYPOINT ["/kanz-provisioner"]
```

Note: the provisioner needs `curl` on the TARGET host (the k3s script uses it) — that runs on the remote node, not in this image; the distroless image only needs the Go SSH client. No shell needed in this image.

- [ ] **Step 3: Add the build.yml matrix entry**

In `.github/workflows/build.yml`, add an entry matching the existing two-field shape (`service:` + `dockerfile:`) that Task 5 of F0 established:

```yaml
        - service: kanz-provisioner
          dockerfile: kanz/cmd/kanz-provisioner/Dockerfile
```

Confirm the field set/format against a neighbouring entry first: `grep -n "dockerfile:" .github/workflows/build.yml | head`.

- [ ] **Step 4: Verify manifests parse + deployability guard**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./test/arch/ -run 'Deployab|Volume' -v`
Expected: PASS (the new docs are valid YAML; `kanz-provisioner` is a `cmd/` tool, not a `services/` entry, so it is not subject to the service-deployability check — but the operator manifest's new volumes/probes still validate).

- [ ] **Step 5: Commit**

```bash
cd kanz && git add infra/deploy/operator-deploy.yaml cmd/kanz-provisioner/Dockerfile
cd .. && git add .github/workflows/build.yml
git commit -m "feat(operator): provisioning RBAC, provisioner SA + NetworkPolicies, image

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

(Adjust the two `git add` paths: the workflow file is at the repo root, the rest under `kanz/`.)

---

### Task 7: Arch guards — bounded provisioning RBAC + provisioner isolation

**Files:**
- Modify: `kanz/test/arch/operator_rbac_test.go`

**Interfaces:**
- Consumes: `moduleRoot`, `yaml.v3` (existing patterns).

- [ ] **Step 1: Write the guard extensions**

Add to `kanz/test/arch/operator_rbac_test.go` two tests. The first asserts the new namespaced Role is bounded (no node writes, no cluster scope, verbs limited to the provisioning set); the second asserts the provisioner SA does not automount a token:

```go
// roleDoc / saDoc reuse the yaml.v3 multi-doc decode pattern from
// TestOperatorClusterRoleIsNodeReadOnly.
type roleDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	AutomountServiceAccountToken *bool `yaml:"automountServiceAccountToken"`
	Rules                        []struct {
		APIGroups []string `yaml:"apiGroups"`
		Resources []string `yaml:"resources"`
		Verbs     []string `yaml:"verbs"`
	} `yaml:"rules"`
}

func TestOperatorProvisionerRoleIsBounded(t *testing.T) {
	docs := decodeOperatorManifest(t)
	allowedResources := map[string]bool{"jobs": true, "secrets": true, "pods": true}
	var found bool
	for _, d := range docs {
		if d.Kind != "Role" || d.Metadata.Name != "operator-provisioner" {
			continue
		}
		found = true
		if d.Metadata.Namespace != "kanz-operator" {
			t.Errorf("operator-provisioner Role must be namespaced to kanz-operator, got %q", d.Metadata.Namespace)
		}
		for _, r := range d.Rules {
			for _, res := range r.Resources {
				if res == "nodes" {
					t.Errorf("operator-provisioner grants a verb on NODES — the operator must never write node objects")
				}
				if !allowedResources[res] {
					t.Errorf("operator-provisioner grants resource %q; only jobs/secrets/pods are allowed", res)
				}
			}
		}
	}
	if !found {
		t.Fatalf("no namespaced Role operator-provisioner found — S2a RBAC missing")
	}
}

func TestProvisionerServiceAccountHasNoToken(t *testing.T) {
	docs := decodeOperatorManifest(t)
	var found bool
	for _, d := range docs {
		if d.Kind != "ServiceAccount" || d.Metadata.Name != "kanz-node-provisioner" {
			continue
		}
		found = true
		if d.AutomountServiceAccountToken == nil || *d.AutomountServiceAccountToken {
			t.Errorf("kanz-node-provisioner must set automountServiceAccountToken: false — the Job needs no k8s API access")
		}
	}
	if !found {
		t.Fatalf("no ServiceAccount kanz-node-provisioner found — provisioner isolation missing")
	}
}

// decodeOperatorManifest reads infra/deploy/operator-deploy.yaml into typed docs.
func decodeOperatorManifest(t *testing.T) []roleDoc {
	t.Helper()
	path := filepath.Join(moduleRoot(t), "infra", "deploy", "operator-deploy.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var docs []roleDoc
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	for {
		var d roleDoc
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		docs = append(docs, d)
	}
	if len(docs) == 0 {
		t.Fatalf("no docs decoded from %s", path)
	}
	return docs
}
```

Ensure the file's imports include `errors`, `io`, `os`, `path/filepath`, `strings`, and `gopkg.in/yaml.v3` (the existing `TestOperatorClusterRoleIsNodeReadOnly` already imports most).

- [ ] **Step 2: Run the guards**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./test/arch/ -run 'Operator|Provisioner' -v`
Expected: PASS (the existing node-reader guard + the two new ones).

- [ ] **Step 3: Mutation-prove the new guard**

Temporarily add `nodes` to the `operator-provisioner` Role's resources in the manifest; run `TestOperatorProvisionerRoleIsBounded`.
Expected: FAIL — "grants a verb on NODES". Revert; re-run: PASS. Confirm `git diff` on the manifest is empty before committing.

- [ ] **Step 4: Commit**

```bash
cd kanz && git add test/arch/operator_rbac_test.go
git commit -m "test(arch): provisioning RBAC bounded (no node writes) + provisioner SA tokenless

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 8: TUI — Add Node form + Save + provisioning status

**Files:**
- Modify: `kanz/cmd/universe/source.go` (add `addNode` + `listProvisions` to `nodeSource`)
- Modify: `kanz/cmd/universe/model.go` (form mode + state)
- Create: `kanz/cmd/universe/form.go` (the hand-rolled Add Node form)
- Modify: `kanz/cmd/universe/view.go` (render the form + a provisioning strip)
- Test: `kanz/cmd/universe/form_test.go`

**Interfaces:**
- Consumes: `operatorpb` client (AddNode/ListProvisions).
- Produces: `mode` on `model` (`modeNodes`/`modeClusters`/`modeAddForm`); a `formState`; the form is a pure function of state for testing.

- [ ] **Step 1: Write the failing form test**

Create `kanz/cmd/universe/form_test.go`:

```go
package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestAddFormCollectsFields(t *testing.T) {
	f := newAddForm()
	f = typeInto(f, "london")       // hostname (field 0)
	f = f.next()
	f = typeInto(f, "10.0.0.5")     // ip (field 1)
	if f.value("hostname") != "london" || f.value("ip") != "10.0.0.5" {
		t.Fatalf("form did not capture fields: %+v", f)
	}
}

func TestAddFormBackspace(t *testing.T) {
	f := newAddForm()
	f = typeInto(f, "lonX")
	f = f.backspace()
	if f.value("hostname") != "lon" {
		t.Errorf("backspace failed: %q", f.value("hostname"))
	}
}

func TestAddFormRendersFocusedField(t *testing.T) {
	f := newAddForm()
	out := f.render()
	for _, want := range []string{"Add Node", "Hostname", "IP", "SSH Port", "User", "Key Path"} {
		if !strings.Contains(out, want) {
			t.Errorf("form render missing %q", want)
		}
	}
}

// typeInto feeds a string to the form as individual key runes.
func typeInto(f addForm, s string) addForm {
	for _, r := range s {
		f = f.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return f
}
```

- [ ] **Step 2: Run — verify it fails**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./cmd/universe/... -run TestAddForm`
Expected: FAIL — `undefined: newAddForm`.

- [ ] **Step 3: Write the form**

Create `kanz/cmd/universe/form.go`:

```go
package main

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// addField is one labelled single-line input. The Key Path field is a FILE PATH,
// not the key itself — the PEM is read client-side at submit and never displayed.
type addField struct {
	key   string
	label string
	value string
}

// addForm is a minimal hand-rolled form (no bubbles dependency): a fixed list of
// single-line text fields with a focused index. Pure state — render() is a pure
// function so form_test can drive it with no TTY.
type addForm struct {
	fields  []addField
	focused int
}

func newAddForm() addForm {
	return addForm{fields: []addField{
		{key: "hostname", label: "Hostname"},
		{key: "ip", label: "IP"},
		{key: "ssh_port", label: "SSH Port", value: "22"},
		{key: "ssh_user", label: "User"},
		{key: "key_path", label: "Key Path"},
	}}
}

func (f addForm) value(key string) string {
	for _, fl := range f.fields {
		if fl.key == key {
			return fl.value
		}
	}
	return ""
}

func (f addForm) key(msg tea.KeyMsg) addForm {
	if msg.Type == tea.KeyRunes {
		f.fields[f.focused].value += string(msg.Runes)
	}
	return f
}

func (f addForm) backspace() addForm {
	v := f.fields[f.focused].value
	if v != "" {
		f.fields[f.focused].value = v[:len(v)-1]
	}
	return f
}

func (f addForm) next() addForm {
	f.focused = (f.focused + 1) % len(f.fields)
	return f
}

func (f addForm) prev() addForm {
	f.focused = (f.focused - 1 + len(f.fields)) % len(f.fields)
	return f
}

var styleFocused = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))

func (f addForm) render() string {
	var b strings.Builder
	b.WriteString(styleTitle.Render("Add Node") + "\n\n")
	for i, fl := range f.fields {
		marker := "  "
		label := fmt.Sprintf("%-10s", fl.label+":")
		line := fmt.Sprintf("%s%s %s", marker, label, fl.value)
		if i == f.focused {
			line = styleFocused.Render("▸ "+label) + " " + fl.value + "_"
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n" + styleDim.Render("[tab] next  [enter] save  [esc] cancel"))
	return b.String()
}
```

- [ ] **Step 4: Run — verify it passes**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./cmd/universe/... -run TestAddForm`
Expected: PASS.

- [ ] **Step 5: Extend the source with the write RPCs**

In `kanz/cmd/universe/source.go`, extend the `nodeSource` interface and the gRPC impl:

```go
// nodeSource fetches estate reads and drives provisioning.
type nodeSource interface {
	fetch(ctx context.Context) (fetchMsg, error)
	addNode(ctx context.Context, req addNodeInput) (string, error)
	listProvisions(ctx context.Context) ([]provisionRow, error)
}

type addNodeInput struct {
	hostname, ip, sshUser string
	sshPort               int32
	sshKey                []byte
}

type provisionRow struct {
	id, hostname, status, message string
}
```

Add to `grpcSource`:

```go
func (g *grpcSource) addNode(ctx context.Context, in addNodeInput) (string, error) {
	resp, err := g.client.AddNode(ctx, &operatorpb.AddNodeRequest{
		Hostname: in.hostname, Ip: in.ip, SshPort: in.sshPort, SshUser: in.sshUser, SshPrivateKey: in.sshKey,
	})
	if err != nil {
		return "", err
	}
	return resp.GetProvisionId(), nil
}

func (g *grpcSource) listProvisions(ctx context.Context) ([]provisionRow, error) {
	resp, err := g.client.ListProvisions(ctx, &operatorpb.ListProvisionsRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]provisionRow, 0, len(resp.GetProvisions()))
	for _, p := range resp.GetProvisions() {
		out = append(out, provisionRow{id: p.GetId(), hostname: p.GetHostname(), status: provLabel(p.GetStatus()), message: p.GetMessage()})
	}
	return out, nil
}

func provLabel(s operatorpb.ProvisionStatus) string {
	switch s {
	case operatorpb.ProvisionStatus_PROVISION_STATUS_INSTALLING:
		return "Installing"
	case operatorpb.ProvisionStatus_PROVISION_STATUS_JOINED:
		return "Joined"
	case operatorpb.ProvisionStatus_PROVISION_STATUS_FAILED:
		return "Failed"
	default:
		return "Pending"
	}
}
```

The existing `stubSource` in `model_test.go` must satisfy the widened interface — add no-op `addNode`/`listProvisions` methods to it:

```go
func (s stubSource) addNode(context.Context, addNodeInput) (string, error) { return "p-1", nil }
func (s stubSource) listProvisions(context.Context) ([]provisionRow, error) { return nil, nil }
```

- [ ] **Step 6: Wire the form mode into the model**

In `kanz/cmd/universe/model.go`, add a form mode. Add to the `model` struct: `form addForm`, `showForm bool`, `provisions []provisionRow`, `formErr error`. In `Update`, handle:
- when `showForm` is false and key is `a` (on the nodes view) → `m.showForm = true; m.form = newAddForm()`.
- when `showForm` is true: `esc` → cancel (`m.showForm = false`); `tab` → `m.form = m.form.next()`; `shift+tab` → `.prev()`; `backspace` → `.backspace()`; `enter` → submit (build `addNodeInput`, read the key file, return a `tea.Cmd` that calls `m.src.addNode`); runes → `m.form = m.form.key(msg)`.

Add the submit command:

```go
// submitAddForm reads the key file named in the form and fires AddNode off the UI
// thread, returning an addNodeResultMsg. The key bytes never touch the model.
func (m model) submitAddForm() tea.Cmd {
	in := addNodeInput{
		hostname: m.form.value("hostname"),
		ip:       m.form.value("ip"),
		sshUser:  m.form.value("ssh_user"),
		sshPort:  atoi32(m.form.value("ssh_port")),
	}
	keyPath := m.form.value("key_path")
	src := m.src
	return func() tea.Msg {
		key, err := os.ReadFile(keyPath)
		if err != nil {
			return addNodeResultMsg{err: fmt.Errorf("read key %s: %w", keyPath, err)}
		}
		in.sshKey = key
		ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
		defer cancel()
		id, err := src.addNode(ctx, in)
		return addNodeResultMsg{id: id, err: err}
	}
}

type addNodeResultMsg struct {
	id  string
	err error
}
```

Handle `addNodeResultMsg` in `Update`: on success `m.showForm = false` (the node will appear via the normal poll); on error `m.formErr = msg.err` (stay on the form). Add `atoi32` (a tiny `strconv.Atoi` wrapper returning `int32`, 22 on parse failure) and imports `context`, `fmt`, `os`, `strconv`.

- [ ] **Step 7: Render the form + provisioning strip**

In `kanz/cmd/universe/view.go`, at the top of `render()`, branch to the form when active:

```go
	if m.showForm {
		out := m.form.render()
		if m.formErr != nil {
			out += "\n" + styleErr.Render("error: "+m.formErr.Error())
		}
		return out
	}
```

And extend `renderStatus` (or the nodes pane footer) to show `[a] add node`, plus a one-line provisioning strip when `len(m.provisions) > 0` (e.g. `"provisioning: london=Installing tokyo=Failed"`). Keep render pure.

- [ ] **Step 8: Run the universe tests + build**

Run:
```bash
cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./cmd/universe/... && \
GOFLAGS=-mod=mod go build ./cmd/universe/... && gofmt -l cmd/universe
```
Expected: tests PASS; build exit 0; gofmt prints nothing.

- [ ] **Step 9: Commit**

```bash
cd kanz && git add cmd/universe/
git commit -m "feat(universe): Add Node form + AddNode/ListProvisions wiring

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 9: Whole-slice verification + rig-proof handoff

**Files:** none (verification only).

- [ ] **Step 1: Whole-slice build/vet/gofmt**

Run:
```bash
cd kanz && GOFLAGS=-mod=mod go build ./... && \
GOFLAGS=-mod=mod go vet ./cmd/kanz-provisioner/... ./cmd/universe/... ./services/operator/... && \
gofmt -l cmd/kanz-provisioner cmd/universe services/operator
```
Expected: exit 0; `gofmt -l` prints nothing.

- [ ] **Step 2: Full arch suite**

Run: `cd kanz && GOFLAGS=-mod=mod GOTMPDIR="$(pwd)/.gotmp" go test ./test/arch/...`
Expected: PASS — including `TestNoSSHPlane` (now path-scoped: `crypto/ssh` only in `cmd/kanz-provisioner`), the operator RBAC guards (node-reader + provisioner-bounded + tokenless SA), and `deployability`.

- [ ] **Step 3: govulncheck (crypto/ssh now reachable)**

Run: `cd kanz && GOFLAGS=-mod=mod go run golang.org/x/vuln/cmd/govulncheck@latest ./...`
Expected: 0 reachable vulnerabilities. If any appear (crypto/ssh or otherwise), STOP and report.

- [ ] **Step 4: Commit any formatting-only fixups (if Step 1 required them)**

```bash
cd kanz && git add -A && git commit -m "chore(s2a): whole-slice verification fixups

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>" || echo "nothing to commit"
```

- [ ] **Step 5: Rig-proof handoff (manual — the operator runs this)**

The live k3s join cannot be exercised on `kind`. What CAN be proven on the F0 rig, and what needs a k3s rig, is:

*On the existing kind rig (proves the write path up to the Job):*
```bash
docker build -f kanz/cmd/kanz-provisioner/Dockerfile -t ghcr.io/kanz-eng/kanz-provisioner:latest .
kind load docker-image ghcr.io/kanz-eng/kanz-provisioner:latest --name kanz-dryrun
# provide the operator-k3s-join Secret (dummy values are fine to exercise Job creation):
kubectl -n kanz-operator create secret generic operator-k3s-join \
  --from-literal=server_url=https://cp:6443 --from-literal=token=dummy
kubectl -n kanz-operator set env deploy/operator OPERATOR_PROVISIONER_IMAGE=ghcr.io/kanz-eng/kanz-provisioner:latest
kubectl -n kanz-operator port-forward deploy/operator 9090:9090
# in universe: press [a], fill the form with an UNREACHABLE ip + any key file, Save.
# Expect: a provisioning Job is created (kubectl -n kanz-operator get jobs), the transient
# Secret exists and is owner-referenced to the Job, ListProvisions shows Installing→Failed
# (dial refused), and the Secret is GC'd when the Job is deleted.
```

*Needs a k3s rig (proves the actual join):* point `operator-k3s-join` at a real single-node k3s server, target a reachable host, and confirm the node appears in `universe`'s Nodes pane. Record the outcome on the board.

- [ ] **Step 6: Update the board**

Add/adjust the S2a readiness row (IMPLEMENTED BUT UNVERIFIED until the k3s-rig join proof) and note the F0 operator-pod NetworkPolicy is now shipped (Task 6). Validate with `bash tools/validate-board.sh KANZ_TASKS.md`, commit, push.

---

## Notes for the executor

- **Task order matters:** Task 2 (guard + crypto/ssh) must precede Task 3 (which builds on `sshRun`). Task 4 (orchestrator) precedes Task 5 (handlers that call it). Task 1 (proto) precedes any task using `operatorpb.AddNode*`.
- **The guard-and-import coupling (Task 2):** the path-scoped allow is deliberately made to FAIL first (dead entry) and go green only when the provisioner imports `crypto/ssh` — do not "fix" the intermediate red by deleting the allow.
- **The key never widens its blast radius:** it exists as bytes only in (a) the AddNode RPC, (b) the Job-owned Secret, (c) the provisioner's in-memory read. It is never in a model field, never in Job env, never logged, never returned. Any task that violates this is wrong.
- **Deferred by design (do NOT add here):** WireGuard, node-exporter, auto-updates, Test Connection (all S2b); Delete/Edit/Drain (S3); standing up the k3s control plane and a k3s rig (infra).
