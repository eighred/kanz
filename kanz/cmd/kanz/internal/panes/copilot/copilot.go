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

	"github.com/charmbracelet/bubbles/textinput"
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

	// input is bubbles/textinput rather than a hand-rolled string.
	//
	// The hand-rolled version handled exactly two keys — enter and backspace —
	// and appended anything else that was a single rune. That is fine until an
	// operator uses a cursor: left/right, home/end, ctrl+w, ctrl+u and paste all
	// did nothing, and a mistyped URL in a /login had to be deleted one character
	// at a time. It also trimmed runes correctly only because someone remembered
	// to; the component does it by construction.
	//
	// It is Blur()red by default and Focus()ed only through SetFocused, so the
	// shell's mode stays the single source of truth for whether this pane is
	// taking text (see SetFocused).
	input textinput.Model
	// busy gates a second Dispatch while one is in flight. The REPL holds a
	// session token and a gateway client and is not safe for concurrent use, and
	// an operator pressing enter twice is not an error to report — it is a
	// keystroke to ignore.
	busy bool
	done bool

	// focused mirrors the shell's Input mode for THIS pane, so the prompt can say
	// whether it will receive what you type. The shell owns the mode; this is a
	// render hint, not a second source of truth.
	focused bool
}

// doneMsg reports that a dispatch finished; stop carries the REPL's /quit.
type doneMsg struct{ stop bool }

// New builds the pane. out is the writer the REPL was constructed with, so this
// pane renders exactly what the REPL printed.
func New(repl Dispatcher, out *Buffer) *Pane {
	ti := textinput.New()
	ti.Prompt = "" // the pane draws its own "kanz› ", so the component adds none
	// No Placeholder: the unfocused state already says "press i to type", and two
	// hints in one line is how a prompt starts looking like output.
	ti.Blur()
	return &Pane{repl: repl, out: out, input: ti}
}

// NewBuffer returns the sink to hand to repl.New and then to New. It exists so
// the composition root cannot accidentally give the REPL one writer and the
// pane another, which would render an always-empty screen.
func NewBuffer() *Buffer { return &Buffer{} }

func (p *Pane) ID() pane.ID       { return ID }
func (p *Pane) Title() string     { return "Copilot" }
func (p *Pane) Plane() pane.Plane { return pane.Gateway }
func (p *Pane) Init() tea.Cmd     { return nil }

// AcceptsTypedText marks this pane as taking typed input: it hosts the REPL
// prompt, so the global key table must not take printable characters from it.
func (p *Pane) AcceptsTypedText() {}

// SetFocused is how the shell tells this pane whether it is taking input, and it
// is the ONLY thing that focuses or blurs the component.
//
// The shell owns the mode — through i/enter, esc, a tab change, or a click
// resolved by bubblezone — and routing every one of those through setMode means
// the component cannot end up focused while the shell believes it is in
// Navigate. That split is what made the prompt render "press i to type" while
// already receiving keys (#65).
func (p *Pane) SetFocused(v bool) {
	p.focused = v
	if v {
		p.input.Focus()
		return
	}
	p.input.Blur()
}

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
		if msg.String() == "enter" {
			line := strings.TrimSpace(p.input.Value())
			p.input.SetValue("")
			if line == "" {
				return p, nil
			}
			p.busy = true
			return p, p.dispatch(line)
		}
		// Everything else is the component's: cursor movement, word delete, paste.
		// Nothing is filtered here — the shell already decided this keystroke is
		// text by being in Input mode, and a second opinion at this layer is how
		// the two disagree.
		var cmd tea.Cmd
		p.input, cmd = p.input.Update(msg)
		return p, cmd
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

	// THE PROMPT IS PINNED TO THE BOTTOM. ui.Frame pads the body AFTER a pane's
	// lines, so a short scrollback left the prompt floating at the top of an
	// empty pane and walking downwards as output arrived — the one element whose
	// position should never move.
	//
	// Padded here rather than in Frame because it is this pane's choice: a form
	// is top-aligned and would be wrong pinned to the bottom.
	if pad := visible - len(lines); pad > 0 {
		lines = append(make([]string, pad), lines...)
	}

	prompt := theme.Prompt.Render("kanz› ") + p.input.View()
	switch {
	case p.busy:
		prompt = theme.StatusBar.Render("kanz› working…")
	case !p.focused:
		// Say why typing does nothing. A prompt that looks identical whether or
		// not it will receive keys is the whole reason the mode has to be visible.
		prompt = theme.StatusBar.Render("kanz› press i to type")
	}

	body := strings.Join(lines, "\n")
	if body != "" {
		body += "\n"
	}
	return theme.Body.Render(body) + ui.Clip(prompt, w)
}
