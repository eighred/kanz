// Package tui drives the operator TUI through a pseudo-terminal.
//
// It exists because the TUI is a full-screen program: the only way to prove a
// keypress reaches Kubernetes is to send a real keypress to a real terminal and
// then ask Kubernetes. Model-level tests cannot do that, and API-level tests prove
// the server rather than the client.
package tui

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// fixedCols/fixedRows pin the terminal geometry. A test whose result depends on
// terminal size is a test that fails on somebody else's machine.
//
// pollInterval trades responsiveness against wasted wakeups: it only polls the
// in-memory capture buffer (not the workflow under test), so shortening it just
// burns CPU on the mutex/Contains check while the child is still producing output.
const (
	fixedCols    = 120
	fixedRows    = 40
	pollInterval = 50 * time.Millisecond
)

// Session is one running program attached to a pty.
type Session struct {
	cmd  *exec.Cmd
	tty  *os.File
	mu   sync.Mutex
	buf  bytes.Buffer
	done chan struct{}
}

// Start launches bin in a pty and begins draining its output.
func Start(t *testing.T, bin string, args []string, env []string) *Session {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: fixedCols, Rows: fixedRows})
	if err != nil {
		t.Fatalf("start %s in pty: %v", bin, err)
	}
	s := &Session{cmd: cmd, tty: f, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		chunk := make([]byte, 4096)
		for {
			n, err := f.Read(chunk)
			if n > 0 {
				s.mu.Lock()
				s.buf.Write(chunk[:n])
				s.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return s
}

// Send writes keystrokes. "\r" is Enter; use SendKey for control bytes.
func (s *Session) Send(keys string) { _, _ = s.tty.WriteString(keys) }

// SendKey writes one raw byte, for control sequences such as ctrl+t (0x14).
func (s *Session) SendKey(b byte) { _, _ = s.tty.Write([]byte{b}) }

// WaitFor blocks until the captured output contains sub, or fails the test with the
// full capture attached. This is the ONLY synchronisation primitive in the harness:
// a sleep would pass on a fast machine and flake on a slow one, and would hide
// exactly the missing-feedback defects this slice exists to find.
func (s *Session) WaitFor(t *testing.T, sub string, timeout time.Duration) {
	t.Helper()
	s.waitFor(t, sub, timeout)
}

func (s *Session) waitFor(t *testing.T, sub string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(s.Frames(), sub) {
			return
		}
		time.Sleep(pollInterval)
	}
	t.Errorf("timed out after %s waiting for %q.\n--- captured output ---\n%s\n--- end ---",
		timeout, sub, s.Frames())
}

// Frames returns everything the program has written so far, ANSI included.
func (s *Session) Frames() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// Close asks the program to quit, then kills it if it will not.
func (s *Session) Close() {
	s.Send("q")
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
		_ = s.cmd.Process.Kill()
		// Kill only requests termination; it does not guarantee the drain
		// goroutine's blocked Read has returned yet. Re-wait on s.done, bounded,
		// so we never close the fd out from under a still-running reader and never
		// hang forever if a platform's poller is slow to notice the fd died.
		select {
		case <-s.done:
		case <-time.After(3 * time.Second):
		}
	}
	_ = s.tty.Close()
	// Reap the child so it doesn't sit as a zombie for the rest of the test binary's
	// life: this harness starts a fresh session per proof, and zombies would
	// accumulate across the suite. The process is confirmed dead or killed above, so
	// Wait cannot block.
	_ = s.cmd.Wait()
}
