package tui

import (
	"strings"
	"testing"
	"time"
)

// The driver is proven against a program whose behaviour is not in question, so a
// driver bug can never be mistaken for a TUI bug later.
func TestDriverSeesOutputAndSendsInput(t *testing.T) {
	s := Start(t, "/bin/sh", []string{"-c", `read x; echo "GOT:$x"`}, nil)
	defer s.Close()
	s.Send("hello\r")
	s.WaitFor(t, "GOT:hello", 5*time.Second)
}

func TestWaitForFailsWithTheLastFrameAttached(t *testing.T) {
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
