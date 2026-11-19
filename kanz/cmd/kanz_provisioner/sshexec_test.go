package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
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

var errUnauthorized = errors.New("unauthorized")

// serveOne handshakes one connection and answers every exec request with reply,
// then exit-status. The test observes the script only on the server side.
func serveOne(c net.Conn, cfg *ssh.ServerConfig, reply string, exitStatus uint32, received chan<- string) {
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
					var command struct{ Value string }
					if ssh.Unmarshal(req.Payload, &command) != nil || command.Value != "sh -s" {
						_ = req.Reply(false, nil)
						_ = ch.Close()
						return
					}
					_ = req.Reply(true, nil)
					script, _ := io.ReadAll(ch)
					received <- string(script)
					_, _ = ch.Write([]byte(reply))
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{exitStatus}))
					_ = ch.Close()
					return
				}
				_ = req.Reply(false, nil)
			}
		}()
		_ = sc
	}
}

// trustHostKey writes a known_hosts entry pinning hostKey for addr and points
// PROVISION_KNOWN_HOSTS_FILE at it, so sshRun's fail-closed host key policy is
// satisfied for this test only.
//
// knownhosts.Line, not a hand-built string: the test servers listen on an ephemeral
// port, and known_hosts spells a non-22 port "[127.0.0.1]:54321" while port 22 is bare.
// Hand-rolling that is how a test ends up asserting the wrong thing about the matcher.
func trustHostKey(t *testing.T, addr string, hostKey ssh.PublicKey) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(knownhosts.Line([]string{addr}, hostKey)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envKnownHostsFile, path)
}

// startEchoSSHServer runs a minimal SSH server on a random port that accepts the
// given authorized key and, for any exec request, replies with a fixed banner. It
// returns the listener address and the server's own host public key — the caller needs
// the latter to satisfy sshRun's host key verification (or, in the mismatch tests, to
// deliberately pin something else).
func startEchoSSHServer(t *testing.T, authorized ssh.PublicKey, reply string, exitStatus uint32) (string, ssh.PublicKey, <-chan string) {
	t.Helper()
	received := make(chan string, 1)
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(authorized.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errUnauthorized
		},
	}
	hostKey := newHostKey(t)
	cfg.AddHostKey(hostKey)

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
			go serveOne(c, cfg, reply, exitStatus, received)
		}
	}()
	return ln.Addr().String(), hostKey.PublicKey(), received
}

// serveOneHanging handshakes one connection, accepts the session/exec request,
// replies true, and then blocks forever without ever writing output or an
// exit-status. It simulates a remote command that never returns.
func serveOneHanging(c net.Conn, cfg *ssh.ServerConfig, beforeSession bool, reached chan<- struct{}) {
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
		if beforeSession {
			close(reached)
			_ = sc.Wait()
			return
		}
		ch, chReqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range chReqs {
				if req.Type == "exec" {
					_ = ch
					_ = req.Reply(true, nil)
					close(reached)
					_ = sc.Wait()
					return
				}
				_ = req.Reply(false, nil)
			}
		}()
		_ = sc
	}
}

// startHangingSSHServer runs a minimal SSH server on a random port that accepts
// the given authorized key and, for any exec request, accepts it but never
// completes — proving sshRun's ctx-honoring behavior on a truly hung command.
func startHangingSSHServer(t *testing.T, authorized ssh.PublicKey, beforeSession bool, reached chan<- struct{}) (string, ssh.PublicKey) {
	t.Helper()
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(authorized.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errUnauthorized
		},
	}
	hostKey := newHostKey(t)
	cfg.AddHostKey(hostKey)

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
			go serveOneHanging(c, cfg, beforeSession, reached)
		}
	}()
	return ln.Addr().String(), hostKey.PublicKey()
}

func TestSSHRunHonorsContextOnHungRun(t *testing.T) {
	pem, pub := clientKeyPEM(t)
	addr, hostPub := startHangingSSHServer(t, pub, false, make(chan struct{}))
	trustHostKey(t, addr, hostPub)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	const command = "synthetic-private-command"
	err := sshRun(ctx, addr, "root", pem, command)
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), command) {
		t.Fatal("a canceled install must report its deadline without disclosing the command or output")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("sshRun did not abort on ctx cancellation (took %s)", time.Since(start))
	}
}

func TestSSHRunCancelsPendingSessionOpen(t *testing.T) {
	key, public := clientKeyPEM(t)
	reached := make(chan struct{})
	addr, hostKey := startHangingSSHServer(t, public, true, reached)
	trustHostKey(t, addr, hostKey)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sshRun(ctx, addr, "fixture", key, "synthetic-script") }()
	select {
	case <-reached:
	case <-time.After(3 * time.Second):
		t.Fatal("authenticated test server never received a session request")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "synthetic-script") {
			t.Fatal("the pending session did not return the local cancellation cause")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pending SSH session did not observe cancellation")
	}
}

func TestSSHRunBoundsSilentHandshake(t *testing.T) {
	key, _ := clientKeyPEM(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	addr := listener.Addr().String()
	trustHostKey(t, addr, newHostKey(t).PublicKey())
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := listener.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sshRun(ctx, addr, "fixture", key, "synthetic-script") }()
	select {
	case c := <-accepted:
		defer func() { _ = c.Close() }()
	case <-time.After(3 * time.Second):
		t.Fatal("test server did not receive the TCP connection")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("expected the handshake deadline")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("silent handshake remained open beyond the deadline")
	}
}

func TestSSHRunExecutesCommand(t *testing.T) {
	pemKey, pub := clientKeyPEM(t)
	addr, hostPub, received := startEchoSSHServer(t, pub, "k3s installed ok", 0)
	trustHostKey(t, addr, hostPub)

	err := sshRun(context.Background(), addr, "root", pemKey, "install-k3s")
	if err != nil {
		t.Fatalf("sshRun: %v", err)
	}
	if <-received != "install-k3s" {
		t.Fatal("the authenticated server did not receive the install script on stdin")
	}
}

func TestSSHRunRejectsBadKey(t *testing.T) {
	err := sshRun(context.Background(), "127.0.0.1:1", "root", []byte("not a key"), "x")
	if err == nil {
		t.Fatal("expected an error parsing a bad private key")
	}
	if !strings.Contains(err.Error(), "parse private key") {
		t.Errorf("err = %v, want a parse-private-key error", err)
	}
}

func TestSSHRunReportsOnlyExitStatusAfterRemoteFailure(t *testing.T) {
	key, public := clientKeyPEM(t)
	addr, hostKey, _ := startEchoSSHServer(t, public, "synthetic-private-output", 13)
	trustHostKey(t, addr, hostKey)
	err := sshRun(context.Background(), addr, "fixture-user", key, "synthetic-private-command")
	if err == nil || err.Error() != "remote install exited with status 13" {
		t.Fatal("remote failure must report only the operation and numeric exit status")
	}
}

// TestSSHRunRefusesWithoutHostKeyPolicy is the fail-closed proof, and it is the one to
// keep if any test here is ever trimmed: with a REAL, REACHABLE, correctly-authenticating
// server in front of it and no host key configured, sshRun must not connect. A regression
// that reinstated an insecure default would leave every other test in this file green.
func TestSSHRunRefusesWithoutHostKeyPolicy(t *testing.T) {
	pemKey, pub := clientKeyPEM(t)
	addr, _, received := startEchoSSHServer(t, pub, "k3s installed ok", 0)
	// Deliberately no trustHostKey, and cleared explicitly so an ambient value in the
	// developer's environment cannot turn this assertion into a no-op.
	t.Setenv(envKnownHostsFile, "")
	t.Setenv(envHostKey, "")
	t.Setenv(envInsecureSkip, "")

	err := sshRun(context.Background(), addr, "root", pemKey, "install-k3s")
	if err == nil {
		t.Fatal("sshRun connected with no host key policy configured — it must fail closed")
	}
	if !strings.Contains(err.Error(), envKnownHostsFile) {
		t.Errorf("err = %v, want it to name %s so an operator knows what to set", err, envKnownHostsFile)
	}
	select {
	case <-received:
		t.Error("the remote command RAN before the host was verified — the token would already be gone")
	default:
	}
}

// TestSSHRunRejectsWrongHostKey is the interception case: something answers on the
// address, authenticates us fine, and is happy to run our command — but it is not the
// host we pinned. The handshake must fail and the command must never reach it.
func TestSSHRunRejectsWrongHostKey(t *testing.T) {
	pemKey, pub := clientKeyPEM(t)
	addr, _, received := startEchoSSHServer(t, pub, "k3s installed ok", 0)
	trustHostKey(t, addr, newHostKey(t).PublicKey()) // pin a DIFFERENT host's key

	err := sshRun(context.Background(), addr, "root", pemKey, "install-k3s")
	if err == nil {
		t.Fatal("sshRun accepted a host key that was not the pinned one")
	}
	if !strings.Contains(err.Error(), "HOST KEY MISMATCH") {
		t.Errorf("err = %v, want a mismatch (not merely some handshake failure)", err)
	}
	select {
	case <-received:
		t.Error("the remote command ran against an unverified host")
	default:
	}
}

// TestSSHRunAcceptsPinnedHostKey covers the PROVISION_HOST_KEY branch end to end, since
// it is the form an operator with a fingerprint but no file will reach for.
func TestSSHRunAcceptsPinnedHostKey(t *testing.T) {
	pemKey, pub := clientKeyPEM(t)
	addr, hostPub, received := startEchoSSHServer(t, pub, "k3s installed ok", 0)
	t.Setenv(envHostKey, string(ssh.MarshalAuthorizedKey(hostPub)))

	err := sshRun(context.Background(), addr, "root", pemKey, "install-k3s")
	if err != nil {
		t.Fatalf("sshRun with a correctly pinned host key: %v", err)
	}
	if <-received != "install-k3s" {
		t.Fatal("the pinned server did not receive the script on stdin")
	}
}
