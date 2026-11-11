package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// clearHostKeyEnv unsets all three policy vars.
//
// Called even by tests that only set one of them: t.Setenv restores whatever was there
// before, but it does not clear what the DEVELOPER'S shell exported, and a stray
// KANZ_PROVISIONER_INSECURE_SKIP_HOST_KEY_VERIFY in an environment would turn the
// conflict tests below into passes-for-the-wrong-reason.
func clearHostKeyEnv(t *testing.T) {
	t.Helper()
	t.Setenv(envKnownHostsFile, "")
	t.Setenv(envHostKey, "")
	t.Setenv(envInsecureSkip, "")
}

// writeKnownHosts writes a known_hosts pinning key for addr and returns its path.
func writeKnownHosts(t *testing.T, addr string, key ssh.PublicKey) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(knownhosts.Line([]string{addr}, key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHostKeyCallbackFailsClosedWhenUnconfigured(t *testing.T) {
	clearHostKeyEnv(t)

	cb, err := hostKeyCallback()
	if err == nil {
		t.Fatal("unconfigured host key policy returned a callback — the default must never be permissive")
	}
	if cb != nil {
		t.Error("a callback was returned alongside the error; a caller ignoring err would connect unverified")
	}
	// The error is the entire operator-facing documentation of this feature, so assert
	// it actually names the knobs rather than just being non-nil.
	for _, want := range []string{envKnownHostsFile, envHostKey, "K3S_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
}

func TestHostKeyCallbackRejectsConflictingPolicy(t *testing.T) {
	clearHostKeyEnv(t)
	_, hostPub := newTestHostKey(t)
	t.Setenv(envKnownHostsFile, writeKnownHosts(t, "10.0.0.5:22", hostPub))
	t.Setenv(envInsecureSkip, "true")

	// The dangerous resolution is "insecure wins" (or "known_hosts wins" while the
	// insecure flag sits there unread). Either way one of the two settings is silently
	// discarded, so this must refuse rather than choose.
	if _, err := hostKeyCallback(); err == nil {
		t.Fatal("a known_hosts file AND the insecure skip were both accepted")
	} else if !strings.Contains(err.Error(), "conflicting") {
		t.Errorf("err = %v, want a conflict error", err)
	}
}

func TestHostKeyCallbackRejectsMissingKnownHostsFile(t *testing.T) {
	clearHostKeyEnv(t)
	t.Setenv(envKnownHostsFile, filepath.Join(t.TempDir(), "does-not-exist"))

	// A Secret that failed to mount looks exactly like this. It must be an outage, not
	// a downgrade — this is the path by which the original defect would return unseen.
	cb, err := hostKeyCallback()
	if err == nil {
		t.Fatal("a missing known_hosts file was tolerated — it must never fall back to trusting anyone")
	}
	if cb != nil {
		t.Error("a callback was returned alongside the error")
	}
}

func TestHostKeyCallbackRejectsUnparseableInsecureFlag(t *testing.T) {
	clearHostKeyEnv(t)
	t.Setenv(envInsecureSkip, "yes-please")

	// Fail-closed on garbage is safe, but silently so; the operator must be told the
	// var they set was not understood rather than debugging a handshake failure.
	if _, err := hostKeyCallback(); err == nil {
		t.Fatal("an unparseable insecure-skip value was accepted")
	}
}

func TestHostKeyCallbackInsecureSkipIsOptInAndWarns(t *testing.T) {
	clearHostKeyEnv(t)
	t.Setenv(envInsecureSkip, "true")

	var warned bytes.Buffer
	old := warnOut
	warnOut = &warned
	t.Cleanup(func() { warnOut = old })

	cb, err := hostKeyCallback()
	if err != nil {
		t.Fatalf("explicit opt-in should be honoured: %v", err)
	}
	_, anyKey := newTestHostKey(t)
	if err := cb("10.0.0.5:22", &net.TCPAddr{}, anyKey); err != nil {
		t.Errorf("insecure mode rejected a key: %v", err)
	}
	// An escape hatch that stops announcing itself is an escape hatch that becomes the
	// default by attrition, so the warning is a tested property, not a courtesy.
	got := warned.String()
	for _, want := range []string{"WARNING", envInsecureSkip, "DISABLED", "K3S_TOKEN"} {
		if !strings.Contains(got, want) {
			t.Errorf("warning = %q, want it to contain %q", got, want)
		}
	}
}

func TestHostKeyCallbackKnownHostsDistinguishesUnknownFromMismatch(t *testing.T) {
	clearHostKeyEnv(t)
	_, pinned := newTestHostKey(t)
	_, other := newTestHostKey(t)
	t.Setenv(envKnownHostsFile, writeKnownHosts(t, "10.0.0.5:22", pinned))

	cb, err := hostKeyCallback()
	if err != nil {
		t.Fatalf("hostKeyCallback: %v", err)
	}

	if err := cb("10.0.0.5:22", &net.TCPAddr{}, pinned); err != nil {
		t.Errorf("the pinned key was rejected: %v", err)
	}

	// Same host, different key: interception or a rebuild. Must be shouted.
	err = cb("10.0.0.5:22", &net.TCPAddr{}, other)
	if err == nil {
		t.Fatal("a key that is not the pinned one was accepted")
	}
	if !strings.Contains(err.Error(), "HOST KEY MISMATCH") {
		t.Errorf("err = %v, want a mismatch error", err)
	}

	// Different host entirely: a missing entry. Same refusal, different remedy — the
	// operator adds a line rather than launching an investigation, and the message has
	// to say which of the two they are in.
	err = cb("10.0.0.6:22", &net.TCPAddr{}, other)
	if err == nil {
		t.Fatal("an unknown host was accepted")
	}
	if strings.Contains(err.Error(), "MISMATCH") {
		t.Errorf("err = %v, want an unknown-host error, not a mismatch", err)
	}
	if !strings.Contains(err.Error(), "not in known_hosts") {
		t.Errorf("err = %v, want it to say the host is absent from the file", err)
	}
}

func TestHostKeyCallbackFixedKeyPinsExactlyOne(t *testing.T) {
	clearHostKeyEnv(t)
	_, pinned := newTestHostKey(t)
	_, other := newTestHostKey(t)
	t.Setenv(envHostKey, string(ssh.MarshalAuthorizedKey(pinned)))

	cb, err := hostKeyCallback()
	if err != nil {
		t.Fatalf("hostKeyCallback: %v", err)
	}
	if err := cb("10.0.0.5:22", &net.TCPAddr{}, pinned); err != nil {
		t.Errorf("the pinned key was rejected: %v", err)
	}
	err = cb("10.0.0.5:22", &net.TCPAddr{}, other)
	if err == nil {
		t.Fatal("an unpinned key was accepted")
	}
	if !strings.Contains(err.Error(), "HOST KEY MISMATCH") {
		t.Errorf("err = %v, want a mismatch error naming the host", err)
	}
}

func TestHostKeyCallbackRejectsUnparseableHostKey(t *testing.T) {
	clearHostKeyEnv(t)
	t.Setenv(envHostKey, "this is not a public key")

	if _, err := hostKeyCallback(); err == nil {
		t.Fatal("a malformed PROVISION_HOST_KEY was accepted")
	}
}

// newTestHostKey returns a throwaway host keypair. Wraps newHostKey (sshexec_test.go)
// to hand back the public half, which every assertion here needs.
func newTestHostKey(t *testing.T) (ssh.Signer, ssh.PublicKey) {
	t.Helper()
	s := newHostKey(t)
	return s, s.PublicKey()
}
