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
const (
	fixedCols = 120
	fixedRows = 40
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
		time.Sleep(50 * time.Millisecond) // polling the CAPTURE, not the workflow
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
	}
	_ = s.tty.Close()
}
