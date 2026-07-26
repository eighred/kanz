package tui

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// requirePTY skips a test that needs a real pseudo-terminal and a POSIX shell.
//
// Every test in this file drives /bin/sh through github.com/creack/pty, and pty has no
// Windows backend — its Windows build is a stub whose Start returns "unsupported", so
// these fail at the first line with `start /bin/sh in pty: unsupported`. That is not a
// finding about anything under test, but it turned `go test ./...` red on a Windows
// workstation, and with CI billing-halted since 2026-07-16 that local run is the only
// gate actually executing. A red suite nobody can get green is a gate everyone learns
// to ignore.
//
// A RUNTIME SKIP RATHER THAN A `//go:build !windows` TAG, DELIBERATELY. A build tag
// would drop this file from the Windows build entirely, so it would not be compiled,
// vetted or type-checked there — and Windows is currently the only place the suite runs
// at all. Skipping keeps every line of it under the compiler on both platforms and
// costs one call per test. The tests themselves are unchanged and still run in full on
// Linux CI, which is where they are the real proof.
func requirePTY(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a pty and /bin/sh: github.com/creack/pty has no Windows backend " +
			"(its Start returns \"unsupported\"). Runs in full on Linux.")
	}
}

// The driver is proven against a program whose behaviour is not in question, so a
// driver bug can never be mistaken for a TUI bug later.
func TestDriverSeesOutputAndSendsInput(t *testing.T) {
	requirePTY(t)
	s := Start(t, "/bin/sh", []string{"-c", `read x; echo "GOT:$x"`}, nil)
	defer s.Close()
	s.Send("hello\r")
	s.WaitFor(t, "GOT:hello", 5*time.Second)
}

func TestWaitForFailsWithTheLastFrameAttached(t *testing.T) {
	requirePTY(t)
	s := Start(t, "/bin/sh", []string{"-c", `echo actual-output; sleep 5`}, nil)
	defer s.Close()
	fake := &testing.T{}
	func() {
		defer func() { _ = recover() }()
		s.waitFor(fake, "never-appears", 500*time.Millisecond)
	}()
	if !fake.Failed() {
		t.Fatal("WaitFor must fail when the substring never appears — a harness that hangs " +
			"or passes silently cannot be trusted to prove anything")
	}
	if !strings.Contains(s.Frames(), "actual-output") {
		t.Error("the captured frames must be available for the failure message; without them a " +
			"red E2E test tells you nothing about what the program actually showed")
	}
}

// TestWaitForIsRelativeToTheLastSend proves WaitFor searches only output produced
// since the most recent Send/SendKey, not the whole cumulative capture. Under the
// old (cumulative) behaviour, waiting again for a token that already appeared before
// the last Send would match instantly against the stale frame — a vacuous wait that
// never actually waited for anything, and that let the caller's next keystroke race
// ahead of the program. This test fails under that behaviour and passes under the
// mark-relative one.
func TestWaitForIsRelativeToTheLastSend(t *testing.T) {
	requirePTY(t)
	// Print FIRST at startup, then echo a fresh, distinct token for every line of
	// input received. No TUI and no cluster needed to prove the driver's own
	// windowing logic.
	s := Start(t, "/bin/sh", []string{"-c",
		`echo FIRST; while read x; do echo "REPLY:$x"; done`}, nil)
	defer s.Close()

	// Genuine wait: FIRST has never appeared before this point.
	s.WaitFor(t, "FIRST", 5*time.Second)

	// Send, then wait for the response to that specific send.
	s.Send("one\r")
	s.WaitFor(t, "REPLY:one", 5*time.Second)

	// The key property: waiting for FIRST again, after a later Send, must time
	// out. FIRST is still in the cumulative capture, so a driver that searches
	// the whole buffer (the defect) would match instantly here instead of
	// waiting. A short timeout is safe because a correct driver can never
	// satisfy this — the text will not reappear.
	fake := &testing.T{}
	start := time.Now()
	func() {
		defer func() { _ = recover() }()
		s.waitFor(fake, "FIRST", 300*time.Millisecond)
	}()
	elapsed := time.Since(start)

	if !fake.Failed() {
		t.Fatal("WaitFor matched a token from before the last Send — it searched the " +
			"whole cumulative capture instead of only output since the last keystroke, " +
			"which is the exact defect that lets a caller's next Send race the program")
	}
	if elapsed < 300*time.Millisecond {
		t.Errorf("WaitFor for a stale token returned in %s, far under its %s timeout — "+
			"it must have matched immediately against an earlier frame instead of actually "+
			"polling the post-Send window", elapsed, 300*time.Millisecond)
	}
}
