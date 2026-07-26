package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// dialTimeout bounds both the TCP dial and the SSH handshake.
const dialTimeout = 30 * time.Second

// syncBuffer is the combined stdout+stderr collector, guarded because
// crypto/ssh copies the two streams from TWO GOROUTINES.
//
// This exists because a plain *bytes.Buffer here was a data race, reported twice
// by CI's race detector (x/crypto/ssh/session.go:514 and :527 both reaching
// bytes.Buffer from sshRun). The failure it produced was not a crash: a remote
// command's output came back EMPTY with a nil error, and occasionally as 16 NUL
// bytes — so a node that failed to provision was reported with no detail at all.
// It also made TestSSHRunExecutesCommand fail ~75% of whole-package runs.
//
// WHY b IS A FIELD AND NOT AN EMBEDDED bytes.Buffer. io.Copy prefers a
// destination's ReadFrom over Write, and bytes.Buffer.ReadFrom grows and mutates
// the buffer REGARDLESS of how many bytes it reads — which is why even the stderr
// copier, which carries no data here, still raced. Embedding would promote
// ReadFrom and hand io.Copy the racy path straight back. Keeping the buffer
// unexported and offering only Write forces io.Copy through the mutex.
//
// A mutex rather than two separate buffers, deliberately: the two streams stay
// interleaved in arrival order, which is how sshRun's output has always read and
// is what makes a k3s install failure legible — the error line keeps its position
// relative to the output around it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

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

	var out syncBuffer
	sess.Stdout = &out
	sess.Stderr = &out

	// crypto/ssh has no context-aware Run. Run in a goroutine and honor ctx by
	// closing the client on cancellation, which unblocks sess.Run. The <-done
	// barrier in both branches means out is read only after the goroutine returns.
	//
	// THAT BARRIER WAS ONCE MISTAKEN FOR THE WHOLE ARGUMENT, and the sentence that
	// used to sit here — "so there is no concurrent access to the buffer" — was
	// false. It reasons only about this goroutine versus the reader. Inside
	// sess.Run, crypto/ssh copies stdout and stderr from two further goroutines
	// that write to out concurrently with each other; the barrier says nothing
	// about them. syncBuffer is what actually makes this safe.
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	select {
	case <-ctx.Done():
		_ = client.Close()
		<-done
		return out.String(), fmt.Errorf("run %q: %w", cmd, ctx.Err())
	case err := <-done:
		if err != nil {
			return out.String(), fmt.Errorf("run %q: %w", cmd, err)
		}
		return out.String(), nil
	}
}
