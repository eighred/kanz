package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// dialTimeout bounds both the TCP dial and the SSH handshake.
const dialTimeout = 30 * time.Second

// Only this locally produced handshake diagnostic may be reported verbatim.
type hostVerificationError struct{ error }

// sshRun opens one authenticated session. Only the fixed shell command appears
// in SSH's exec request. The install script is input to that shell.
//
// BOTH DIRECTIONS ARE AUTHENTICATED, and the second one is not optional. The signer
// below proves us to the host; HostKeyCallback proves the host to us. This session is
// the estate's only SSH session and it carries K3S_TOKEN — see hostkey.go for what
// answering it without checking who answered actually costs, and for the argument this
// replaces. hostKeyCallback fails closed, so an unconfigured or misconfigured policy
// stops the provision HERE, before the dial and long before the token is in flight.
func sshRun(ctx context.Context, addr, user string, pemKey []byte, script string) error {
	signer, err := ssh.ParsePrivateKey(pemKey)
	if err != nil {
		return errors.New("parse private key: unsupported, encrypted or malformed SSH bootstrap key")
	}
	cfg := &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}}
	hostKeys, err := hostKeyCallback(cfg)
	if err != nil {
		return fmt.Errorf("ssh host key policy: %w", err)
	}
	cfg.HostKeyCallback = func(name string, remote net.Addr, key ssh.PublicKey) error {
		if err := hostKeys(name, remote, key); err != nil {
			return &hostVerificationError{err}
		}
		return nil
	}

	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()
	// NewClientConn and NewSession do not accept a context. Closing the
	// underlying transport interrupts either phase and a running installer.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline := time.Now().Add(dialTimeout)
	if parent, ok := ctx.Deadline(); ok && parent.Before(deadline) {
		deadline = parent
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return errors.New("could not bound the SSH handshake")
	}
	sc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("SSH handshake interrupted: %w", ctx.Err())
		}
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			return fmt.Errorf("SSH handshake timed out: %w", context.DeadlineExceeded)
		}
		var verification *hostVerificationError
		if errors.As(err, &verification) {
			return fmt.Errorf("SSH host verification: %w", verification)
		}
		return errors.New("SSH handshake or user authentication failed")
	}
	client := ssh.NewClient(sc, chans, reqs)
	defer func() { _ = client.Close() }()
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return errors.New("could not clear the SSH handshake deadline")
	}

	sess, err := client.NewSession()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("open SSH session interrupted: %w", ctx.Err())
		}
		return errors.New("could not open the authenticated SSH session")
	}
	defer func() { _ = sess.Close() }()

	// The installer can echo its environment on either stream. With nil
	// Stdout/Stderr the SSH client drains both to io.Discard.
	sess.Stdin = strings.NewReader(script)

	err = sess.Run("sh -s")
	if ctx.Err() != nil {
		return fmt.Errorf("remote install interrupted: %w", ctx.Err())
	}
	if err != nil {
		// The script on stdin contains the cluster admission token.
		// The peer controls exit-signal text. Expose only the numeric status.
		var exit *ssh.ExitError
		if errors.As(err, &exit) && exit.ExitStatus() >= 0 {
			return fmt.Errorf("remote install exited with status %d", exit.ExitStatus())
		}
		return errors.New("remote install ended without a confirmed exit status")
	}
	return nil
}
