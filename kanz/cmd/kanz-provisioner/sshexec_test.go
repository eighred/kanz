package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"net"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
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

// startEchoSSHServer runs a minimal SSH server on a random port that accepts the
// given authorized key and, for any exec request, replies with a fixed banner. It
// returns the listener address and a cleanup func.
func startEchoSSHServer(t *testing.T, authorized ssh.PublicKey, reply string) string {
	t.Helper()
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(authorized.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errUnauthorized
		},
	}
	cfg.AddHostKey(newHostKey(t))

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
	return ln.Addr().String()
}

func TestSSHRunExecutesCommand(t *testing.T) {
	pemKey, pub := clientKeyPEM(t)
	addr := startEchoSSHServer(t, pub, "k3s installed ok")

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
