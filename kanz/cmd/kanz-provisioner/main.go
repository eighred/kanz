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
