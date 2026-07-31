// Package copilot is the shell's default pane: the kanz REPL, hosted inside the
// TUI instead of owning the terminal itself.
//
// IT REUSES repl.Dispatch RATHER THAN REIMPLEMENTING THE COMMANDS. The REPL's
// Run method owns a blocking scanner on stdin, which cannot live inside
// bubbletea — bubbletea owns the terminal and delivers keys as messages. But
// Run's body is a read loop around one call, and that call takes a string and
// writes to an io.Writer. So this pane supplies the loop and the REPL supplies
// the behaviour; /login, /ask, /whoami and the rest have exactly one
// implementation, which is the point.
package copilot

import (
	"context"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/eighred/kanz/internal/tui/pane"
	"github.com/eighred/kanz/internal/tui/theme"
	"github.com/eighred/kanz/internal/tui/ui"
)

// ID is this pane's stable address.
const ID pane.ID = "copilot"

// Dispatcher is the half of the REPL this pane drives. Narrowed to one method
// so the pane can be tested without a gateway, a token store or an SSO client.
type Dispatcher interface {
	Dispatch(ctx context.Context, line string) (stop bool)
}

// Buffer is the REPL's output sink.
//
// SYNCHRONISED BECAUSE Dispatch RUNS OFF THE UPDATE GOROUTINE. It blocks on the
// gateway, so it runs inside a tea.Cmd; meanwhile View renders from the same
// bytes. Without the mutex that is a data race — one the race detector cannot
// prove here (no cgo on the usual Windows box), which is exactly why it is
// written correctly rather than observed to be fine.
type Buffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *Buffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *Buffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// Pane is the copilot screen.
type Pane struct {
	repl Dispatcher
	out  *Buffer

	input string
	// busy gates a second Dispatch while one is in flight. The REPL holds a
	// session token and a gateway client and is not safe for concurrent use, and
	// an operator pressing enter twice is not an error to report — it is a
	// keystroke to ignore.
	busy bool
	done bool
}

// doneMsg reports that a dispatch finished; stop carries the REPL's /quit.
type doneMsg struct{ stop bool }

// New builds the pane. out is the writer the REPL was constructed with, so this
// pane renders exactly what the REPL printed.
func New(repl Dispatcher, out *Buffer) *Pane {
	return &Pane{repl: repl, out: out}
}

// NewBuffer returns the sink to hand to repl.New and then to New. It exists so
// the composition root cannot accidentally give the REPL one writer and the
// pane another, which would render an always-empty screen.
func NewBuffer() *Buffer { return &Buffer{} }

func (p *Pane) ID() pane.ID       { return ID }
func (p *Pane) Title() string     { return "Copilot" }
func (p *Pane) Plane() pane.Plane { return pane.Gateway }
func (p *Pane) Init() tea.Cmd     { return nil }

func (p *Pane) Update(msg tea.Msg) (pane.Pane, tea.Cmd) {
	switch msg := msg.(type) {
	case doneMsg:
		p.busy = false
		if msg.stop {
			p.done = true
			return p, tea.Quit
		}
		return p, nil

	case tea.KeyMsg:
		if p.busy {
			// Input during a request is dropped rather than queued. Queuing would
			// let an operator stack commands against a session whose token may be
			// replaced by the /login they just typed.
			return p, nil
		}
		switch msg.String() {
		case "enter":
			line := strings.TrimSpace(p.input)
			p.input = ""
			if line == "" {
				return p, nil
			}
			p.busy = true
			return p, p.dispatch(line)
		case "backspace":
			if n := len(p.input); n > 0 {
				// Trim a whole rune, not a byte: a multi-byte character deleted
				// one byte at a time leaves invalid UTF-8 on screen.
				r := []rune(p.input)
				p.input = string(r[:len(r)-1])
			}
			return p, nil
		default:
			if k := msg.String(); len(k) > 0 && !strings.Contains(k, "+") && len([]rune(k)) == 1 {
				p.input += k
			}
			return p, nil
		}
	}
	return p, nil
}

// dispatch runs one line off the Update goroutine. See Dispatcher — it blocks on
// the network, and blocking in Update freezes every pane, not just this one.
func (p *Pane) dispatch(line string) tea.Cmd {
	return func() tea.Msg {
		stop := p.repl.Dispatch(context.Background(), line)
		return doneMsg{stop: stop}
	}
}

func (p *Pane) View(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	lines := ui.Wrap(p.out.String(), w)

	// The prompt is the last line and must always be visible, so the scrollback
	// is what gets dropped when the pane is short — not the thing being typed.
	promptRows := 1
	visible := h - promptRows
	if visible < 0 {
		visible = 0
	}
	if len(lines) > visible {
		lines = lines[len(lines)-visible:]
	}

	body := strings.Join(lines, "\n")
	prompt := theme.Prompt.Render("kanz› ") + p.input
	if p.busy {
		prompt = theme.StatusBar.Render("kanz› working…")
	}
	if body != "" {
		body += "\n"
	}
	return theme.Body.Render(body) + ui.Clip(prompt, w)
}
