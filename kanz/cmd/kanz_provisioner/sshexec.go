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
		return fmt.Errorf("parse private key: %w", err)
	}
	hostKeys, err := hostKeyCallback()
	if err != nil {
		return fmt.Errorf("ssh host key policy: %w", err)
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKeys,
		Timeout:         dialTimeout,
	}

	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	sc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("ssh handshake %s: %w", addr, err)
	}
	client := ssh.NewClient(sc, chans, reqs)
	defer func() { _ = client.Close() }()

	sess, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("new session: %w", err)
	}
	defer func() { _ = sess.Close() }()

	// The installer can echo its environment on either stream. With nil
	// Stdout/Stderr the SSH client drains both to io.Discard.
	sess.Stdin = strings.NewReader(script)

	done := make(chan error, 1)
	go func() { done <- sess.Run("sh -s") }()
	select {
	case <-ctx.Done():
		_ = client.Close()
		// Cancellation is the selected result; join Run before reading its output.
		<-done //nolint:errcheck // Cancellation already owns the terminal result.
		return fmt.Errorf("remote install interrupted: %w", ctx.Err())
	case err := <-done:
		if err != nil {
			// The command contains the cluster admission token. Both the
			// output and SSH exit-signal text are controlled by the remote
			// installer. Only the numeric exit status is safe for Job logs.
			var exit *ssh.ExitError
			if errors.As(err, &exit) && exit.ExitStatus() >= 0 {
				return fmt.Errorf("remote install exited with status %d", exit.ExitStatus())
			}
			return errors.New("remote install ended without a confirmed exit status")
		}
		return nil
	}
}
