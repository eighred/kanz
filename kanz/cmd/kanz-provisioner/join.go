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
