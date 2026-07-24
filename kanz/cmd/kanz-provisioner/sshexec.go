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
