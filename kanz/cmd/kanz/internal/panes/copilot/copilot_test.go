package copilot

import (
	"context"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// fakeREPL records the lines it was asked to dispatch and can block, so the
// pane's in-flight gating is testable without a gateway.
type fakeREPL struct {
	mu      sync.Mutex
	lines   []string
	release chan struct{}
	stop    bool
}

func (f *fakeREPL) Dispatch(_ context.Context, line string) bool {
	f.mu.Lock()
	f.lines = append(f.lines, line)
	f.mu.Unlock()
	if f.release != nil {
		<-f.release
	}
	return f.stop
}

func (f *fakeREPL) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lines...)
}

func typeLine(p *Pane, s string) *Pane {
	for _, r := range s {
		next, _ := p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		p = next.(*Pane)
	}
	return p
}

func press(p *Pane, t tea.KeyType) (*Pane, tea.Cmd) {
	next, cmd := p.Update(tea.KeyMsg{Type: t})
	return next.(*Pane), cmd
}

// The pane collects a line and hands it to the REPL — it does not reimplement
// any command. /help, /login and the rest exist once, in repl.Dispatch.
func TestEnterDispatchesTheTypedLineToTheREPL(t *testing.T) {
	f := &fakeREPL{}
	p := New(f, NewBuffer())

	p = typeLine(p, "/whoami")
	p, cmd := press(p, tea.KeyEnter)
	if cmd == nil {
		t.Fatal("enter produced no command — the line was never dispatched")
	}
	cmd() // run the tea.Cmd the shell would have run

	got := f.got()
	if len(got) != 1 || got[0] != "/whoami" {
		t.Fatalf("REPL saw %v, want [/whoami]", got)
	}
	if p.input != "" {
		t.Errorf("input = %q after enter, want it cleared", p.input)
	}
}

// AN EMPTY LINE MUST NOT REACH THE REPL. Enter on an empty prompt is how an
// operator clears their thinking, not a command.
func TestEnterOnAnEmptyLineDispatchesNothing(t *testing.T) {
	f := &fakeREPL{}
	p := New(f, NewBuffer())

	p, cmd := press(p, tea.KeyEnter)
	if cmd != nil {
		t.Error("enter on an empty prompt produced a command")
	}
	if len(f.got()) != 0 {
		t.Errorf("REPL saw %v, want nothing", f.got())
	}
	_ = p
}

// INPUT IS DROPPED WHILE A REQUEST IS IN FLIGHT, not queued.
//
// The REPL holds a session token and a gateway client and is not safe for
// concurrent use. Queuing would also let an operator stack commands against a
// session whose token the in-flight /login is about to replace.
func TestInputIsIgnoredWhileARequestIsInFlight(t *testing.T) {
	f := &fakeREPL{release: make(chan struct{})}
	p := New(f, NewBuffer())

	p = typeLine(p, "/exposure PF1")
	p, cmd := press(p, tea.KeyEnter)

	done := make(chan struct{})
	go func() { cmd(); close(done) }() // blocks in Dispatch until released

	// Type while busy: none of it may register.
	p = typeLine(p, "/logout")
	p, second := press(p, tea.KeyEnter)
	if second != nil {
		t.Error("a second command was dispatched while one was in flight")
	}
	if p.input != "" {
		t.Errorf("keystrokes during a request landed in the input buffer: %q", p.input)
	}

	close(f.release)
	<-done

	if got := f.got(); len(got) != 1 {
		t.Errorf("REPL saw %v, want exactly one dispatch", got)
	}
}

// /quit inside the pane must quit the shell, not just the pane. The REPL's stop
// signal is the only thing that knows the operator asked to leave.
func TestREPLStopQuitsTheShell(t *testing.T) {
	f := &fakeREPL{stop: true}
	p := New(f, NewBuffer())

	next, _ := p.Update(doneMsg{stop: true})
	p = next.(*Pane)
	if !p.done {
		t.Error("a stop from the REPL did not mark the pane done")
	}
}

// Backspace must remove a whole rune. Trimming a byte at a time leaves invalid
// UTF-8 on screen the moment anyone types a non-ASCII character.
func TestBackspaceRemovesAWholeRune(t *testing.T) {
	p := New(&fakeREPL{}, NewBuffer())
	p = typeLine(p, "né")

	p, _ = press(p, tea.KeyBackspace)
	if p.input != "n" {
		t.Errorf("input = %q after backspace over a multi-byte rune, want %q", p.input, "n")
	}
	p, _ = press(p, tea.KeyBackspace)
	p, _ = press(p, tea.KeyBackspace) // past empty must not panic
	if p.input != "" {
		t.Errorf("input = %q, want empty", p.input)
	}
}

// The prompt is the last line and must survive a short pane: scrollback is what
// gets dropped, never the thing being typed.
func TestViewAlwaysKeepsThePromptVisible(t *testing.T) {
	out := NewBuffer()
	for i := 0; i < 200; i++ {
		_, _ = out.Write([]byte("scrollback line\n"))
	}
	p := New(&fakeREPL{}, out)
	// The shell focuses a text pane on entry; without it the prompt correctly
	// renders "press i to type" instead of the input, which is a different
	// assertion from the one this test makes.
	p.SetFocused(true)
	p = typeLine(p, "/whoami")

	v := p.View(40, 5)
	lines := strings.Split(v, "\n")
	if len(lines) > 5 {
		t.Errorf("View rendered %d lines into 5 rows", len(lines))
	}
	if !strings.Contains(v, "/whoami") {
		t.Error("the prompt was scrolled off by history — the operator cannot see what they are typing")
	}
}

// AN UNFOCUSED PROMPT SAYS SO. A prompt that looks identical whether or not it
// will receive keys is the reason the mode has to be visible at all.
func TestAnUnfocusedPromptSaysHowToType(t *testing.T) {
	p := New(&fakeREPL{}, NewBuffer())

	if v := p.View(40, 5); !strings.Contains(v, "press i to type") {
		t.Errorf("View = %q, want it to say how to start typing", v)
	}
	p.SetFocused(true)
	if v := p.View(40, 5); strings.Contains(v, "press i to type") {
		t.Errorf("View = %q, still says 'press i' while focused", v)
	}
}

// THE PROMPT IS PINNED TO THE BOTTOM. ui.Frame pads AFTER a pane's lines, so a
// short scrollback used to leave the prompt at the top of an empty pane,
// walking downwards as output arrived.
func TestThePromptSitsAtTheBottomWithLittleScrollback(t *testing.T) {
	out := NewBuffer()
	_, _ = out.Write([]byte("one line\n"))
	p := New(&fakeREPL{}, out)
	p.SetFocused(true)

	lines := strings.Split(p.View(40, 6), "\n")
	last := lines[len(lines)-1]
	if !strings.Contains(last, "kanz›") {
		t.Errorf("last rendered line is %q, want the prompt — it floats up when scrollback is short", last)
	}
}
