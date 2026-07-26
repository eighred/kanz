package main

import (
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// openPort binds an ephemeral loopback port. Kept unaccepted on purpose: the probe
// only completes a TCP handshake, so a listener that never Accepts is enough — and
// closing it immediately is how the unreachable case gets a port nothing is on.
func openPort(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// probeEnv puts the binary in probe mode against addr and redirects the termination
// message to a temp file, returning that path. The redirect is why terminationLogPath
// is a var: /dev/termination-log only exists inside a Kubernetes container.
func probeEnv(t *testing.T, addr string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "termination-log")
	old := terminationLogPath
	terminationLogPath = path
	t.Cleanup(func() { terminationLogPath = old })

	t.Setenv("PROVISION_MODE", modeProbe)
	t.Setenv("PROVISION_TARGET_ADDR", addr)
	return path
}

func readProbeResult(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no termination message written: %v", err)
	}
	if n := strings.Count(strings.TrimSuffix(string(b), "\n"), "\n"); n != 0 {
		t.Errorf("termination message is %d lines, want exactly 1: %q", n+1, string(b))
	}
	return strings.TrimSpace(string(b))
}

var reachableLine = regexp.MustCompile(`^reachable [0-9]+$`)

// TestRunProbeReportsReachable dials a REAL listener (no seam, no fake) — the whole
// value of this mode is that the dial is real, so the test dials.
func TestRunProbeReportsReachable(t *testing.T) {
	ln := openPort(t)
	defer func() { _ = ln.Close() }()
	path := probeEnv(t, ln.Addr().String())

	fj := &fakeJoiner{}
	if err := run(fj); err != nil {
		t.Fatalf("run: %v", err)
	}
	if fj.called {
		t.Error("probe mode must never join — a probe that provisions is a destructive surprise")
	}
	if got := readProbeResult(t, path); !reachableLine.MatchString(got) {
		t.Errorf("termination message = %q, want `reachable <latency_ms>`", got)
	}
}

// TestRunProbeReportsUnreachable proves the three things the operator depends on for a
// closed port: a non-nil error (which main turns into exit 1), an `unreachable` line,
// and a reason trimmed to its last ": " segment rather than Go's full dial error.
func TestRunProbeReportsUnreachable(t *testing.T) {
	ln := openPort(t)
	addr := ln.Addr().String()
	_ = ln.Close() // bind then close, so the port is (almost certainly) closed
	path := probeEnv(t, addr)

	if err := run(&fakeJoiner{}); err == nil {
		t.Fatal("expected an error so main exits 1 for an unreachable target")
	}
	got := readProbeResult(t, path)
	if !strings.HasPrefix(got, "unreachable ") {
		t.Fatalf("termination message = %q, want an `unreachable <reason>` line", got)
	}
	reason := strings.TrimPrefix(got, "unreachable ")
	if strings.Contains(reason, ": ") {
		t.Errorf("reason %q is not trimmed to its last \": \" segment", reason)
	}
	if strings.Contains(reason, addr) {
		t.Errorf("reason %q echoes the dialled address back to the caller", reason)
	}
}

// TestRunProbeNeedsNoCredentials is the reason the probe is a separate mode rather
// than the join path with a flag: it must succeed with NO k3s coordinates, NO SSH user
// and NO readable key, so no caller is ever forced to mount a bootstrap key into a pod
// that has no use for one.
func TestRunProbeNeedsNoCredentials(t *testing.T) {
	ln := openPort(t)
	defer func() { _ = ln.Close() }()
	path := probeEnv(t, ln.Addr().String())
	for _, k := range []string{"PROVISION_SSH_USER", "K3S_SERVER_URL", "K3S_TOKEN"} {
		t.Setenv(k, "")
	}
	t.Setenv("PROVISION_SSH_KEY_FILE", filepath.Join(t.TempDir(), "does-not-exist"))

	if err := run(&fakeJoiner{}); err != nil {
		t.Fatalf("probe mode requires a credential it should not: %v", err)
	}
	if got := readProbeResult(t, path); !reachableLine.MatchString(got) {
		t.Errorf("termination message = %q, want `reachable <latency_ms>`", got)
	}
}

func TestRunProbeRequiresTargetAddr(t *testing.T) {
	probeEnv(t, "")
	if err := run(&fakeJoiner{}); err == nil {
		t.Fatal("expected an error when PROVISION_TARGET_ADDR is missing")
	}
}

// TestRunRejectsUnknownMode: every unrecognised value must error rather than silently
// joining. Case matters — the operator sets exactly "probe", so anything else is a
// misconfiguration to surface, not to guess at.
func TestRunRejectsUnknownMode(t *testing.T) {
	for _, mode := range []string{"join", "Probe", "PROBE", " probe", "probe ", "test"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("PROVISION_MODE", mode)
			t.Setenv("PROVISION_TARGET_ADDR", "127.0.0.1:22")
			fj := &fakeJoiner{}
			if err := run(fj); err == nil {
				t.Errorf("PROVISION_MODE=%q was accepted", mode)
			}
			if fj.called {
				t.Errorf("PROVISION_MODE=%q fell through to the join path", mode)
			}
		})
	}
}

func TestDialReasonTrimsToLastSegment(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"dial tcp 10.0.0.5:22: connect: connection refused", "connection refused"},
		{"dial tcp 10.0.0.5:22: i/o timeout", "i/o timeout"},
		{"no colon here", "no colon here"},
		{"trailing colon: ", "trailing colon: "},
	} {
		if got := dialReason(errString(tc.in)); got != tc.want {
			t.Errorf("dialReason(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
