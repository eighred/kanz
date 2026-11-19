package main

import (
	"context"
	"fmt"
	"strings"
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
type runFunc func(ctx context.Context, addr, user string, key []byte, script string) error

// sshJoiner joins by running the official k3s install script over one SSH session.
type sshJoiner struct {
	run runFunc
}

func newSSHJoiner() *sshJoiner { return &sshJoiner{run: sshRun} }

// The fixed SSH command receives this script on stdin. A failed or empty
// download must fail the Job before an installer can run.
func k3sInstallScript(k K3sJoin) string {
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	return fmt.Sprintf(`set -eu
umask 077
installer=$(mktemp)
trap 'rm -f "$installer"' 0
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --output "$installer" https://get.k3s.io
test -s "$installer"
K3S_URL=%s K3S_TOKEN=%s sh "$installer" agent
`, quote(k.ServerURL), quote(k.Token))
}

func (j *sshJoiner) Join(ctx context.Context, t Target, k K3sJoin) error {
	err := j.run(ctx, t.Addr, t.User, t.Key, k3sInstallScript(k))
	if err != nil {
		return fmt.Errorf("k3s agent install failed: %w", err)
	}
	return nil
}

// compile-time assertion.
var _ Joiner = (*sshJoiner)(nil)
