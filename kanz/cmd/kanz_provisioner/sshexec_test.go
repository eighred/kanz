package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
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
func startEchoSSHServer(t *testing.T, authorized ssh.PublicKey, reply string) (string, ssh.PublicKey) {
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
			go serveOne(c, cfg, reply)
		}
	}()
	return ln.Addr().String(), hostKey.PublicKey()
}

// serveOneHanging handshakes one connection, accepts the session/exec request,
// replies true, and then blocks forever without ever writing output or an
// exit-status. It simulates a remote command that never returns.
func serveOneHanging(c net.Conn, cfg *ssh.ServerConfig) {
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
					_ = ch
					_ = req.Reply(true, nil)
					// Hang forever: never write output, never send exit-status,
					// never close the channel.
					select {}
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
func startHangingSSHServer(t *testing.T, authorized ssh.PublicKey) (string, ssh.PublicKey) {
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
			go serveOneHanging(c, cfg)
		}
	}()
	return ln.Addr().String(), hostKey.PublicKey()
}

func TestSSHRunHonorsContextOnHungRun(t *testing.T) {
	pem, pub := clientKeyPEM(t)
	addr, hostPub := startHangingSSHServer(t, pub) // accepts + auths, never completes exec
	trustHostKey(t, addr, hostPub)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := sshRun(ctx, addr, "root", pem, "hang-forever")
	if err == nil {
		t.Fatal("expected a context error from a hung run")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("sshRun did not abort on ctx cancellation (took %s)", time.Since(start))
	}
}

func TestSSHRunExecutesCommand(t *testing.T) {
	pemKey, pub := clientKeyPEM(t)
	addr, hostPub := startEchoSSHServer(t, pub, "k3s installed ok")
	trustHostKey(t, addr, hostPub)

	out, err := sshRun(context.Background(), addr, "root", pemKey, "install-k3s")
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

// TestSSHRunRefusesWithoutHostKeyPolicy is the fail-closed proof, and it is the one to
// keep if any test here is ever trimmed: with a REAL, REACHABLE, correctly-authenticating
// server in front of it and no host key configured, sshRun must not connect. A regression
// that reinstated an insecure default would leave every other test in this file green.
func TestSSHRunRefusesWithoutHostKeyPolicy(t *testing.T) {
	pemKey, pub := clientKeyPEM(t)
	addr, _ := startEchoSSHServer(t, pub, "k3s installed ok")
	// Deliberately no trustHostKey, and cleared explicitly so an ambient value in the
	// developer's environment cannot turn this assertion into a no-op.
	t.Setenv(envKnownHostsFile, "")
	t.Setenv(envHostKey, "")
	t.Setenv(envInsecureSkip, "")

	out, err := sshRun(context.Background(), addr, "root", pemKey, "install-k3s")
	if err == nil {
		t.Fatal("sshRun connected with no host key policy configured — it must fail closed")
	}
	if !strings.Contains(err.Error(), envKnownHostsFile) {
		t.Errorf("err = %v, want it to name %s so an operator knows what to set", err, envKnownHostsFile)
	}
	if strings.Contains(out, "k3s installed ok") {
		t.Error("the remote command RAN before the host was verified — the token would already be gone")
	}
}

// TestSSHRunRejectsWrongHostKey is the interception case: something answers on the
// address, authenticates us fine, and is happy to run our command — but it is not the
// host we pinned. The handshake must fail and the command must never reach it.
func TestSSHRunRejectsWrongHostKey(t *testing.T) {
	pemKey, pub := clientKeyPEM(t)
	addr, _ := startEchoSSHServer(t, pub, "k3s installed ok")
	trustHostKey(t, addr, newHostKey(t).PublicKey()) // pin a DIFFERENT host's key

	out, err := sshRun(context.Background(), addr, "root", pemKey, "install-k3s")
	if err == nil {
		t.Fatal("sshRun accepted a host key that was not the pinned one")
	}
	if !strings.Contains(err.Error(), "HOST KEY MISMATCH") {
		t.Errorf("err = %v, want a mismatch (not merely some handshake failure)", err)
	}
	if strings.Contains(out, "k3s installed ok") {
		t.Error("the remote command ran against an unverified host")
	}
}

// TestSSHRunAcceptsPinnedHostKey covers the PROVISION_HOST_KEY branch end to end, since
// it is the form an operator with a fingerprint but no file will reach for.
func TestSSHRunAcceptsPinnedHostKey(t *testing.T) {
	pemKey, pub := clientKeyPEM(t)
	addr, hostPub := startEchoSSHServer(t, pub, "k3s installed ok")
	t.Setenv(envHostKey, string(ssh.MarshalAuthorizedKey(hostPub)))

	out, err := sshRun(context.Background(), addr, "root", pemKey, "install-k3s")
	if err != nil {
		t.Fatalf("sshRun with a correctly pinned host key: %v", err)
	}
	if !strings.Contains(out, "k3s installed ok") {
		t.Errorf("output = %q, want the server reply", out)
	}
}
