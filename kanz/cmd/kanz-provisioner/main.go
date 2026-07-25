// kanz-provisioner is a ONE-SHOT Job that provisions a single host into the estate:
// it SSHes once with an operator-supplied bootstrap key, installs the k3s agent, and
// joins. It is the sole importer of crypto/ssh in the module (bounded to the node-join
// handshake). It reads all inputs from env + a mounted Secret, does its work, and exits
// 0 on join / non-zero on failure — the operator service derives status from the Job.
//
// PROVISION_MODE=probe selects a second, credential-free mode: one TCP dial at the
// target and a one-line verdict (see probe.go). Both modes live in this binary
// because both need the SAME pod identity — the one node-provisioner-egress grants
// destination-open :22 — and shipping a second image to make one dial would be a
// second image to build, scan, publish and keep in step for no gain.
package main

import (
	"context"
	"fmt"
	"os"
	"time"
)

// provisionTimeout bounds the whole provisioning attempt.
const provisionTimeout = 10 * time.Minute

// modeProbe is the only recognised PROVISION_MODE value; unset means join.
const modeProbe = "probe"

func main() {
	if err := run(newSSHJoiner()); err != nil {
		fmt.Fprintln(os.Stderr, "kanz-provisioner: "+err.Error())
		os.Exit(1)
	}
}

// run picks a mode and drives it. Split from main so a test can pass a fake Joiner
// (env-driven), keeping main() a thin shell.
//
// An unrecognised non-empty PROVISION_MODE is a startup error, NOT a fall-through to
// the join path. Joining installs k3s on someone else's host; probing opens and closes
// one socket. A typo must never turn the harmless request into the destructive one.
func run(j Joiner) error {
	switch mode := os.Getenv("PROVISION_MODE"); mode {
	case "":
		return runJoin(j)
	case modeProbe:
		return runProbe()
	default:
		return fmt.Errorf("unknown PROVISION_MODE %q (want %q, or unset to join)", mode, modeProbe)
	}
}

// runJoin is the original, unchanged behaviour: read the full credential set and join.
func runJoin(j Joiner) error {
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
