// Package app is the kanz shell's root bubbletea model: the tab router.
//
// It owns three things and deliberately nothing else — pane selection, the
// global key table, and the frame. Everything domain-shaped lives in a pane.
// The router is the file most likely to accumulate "just one more case", so the
// boundary is worth stating: if a change here needs to know what a pane DOES,
// it belongs in the pane.
package app

import (
	"fmt"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/eighred/kanz/internal/tui/keymap"
	"github.com/eighred/kanz/internal/tui/pane"
	"github.com/eighred/kanz/internal/tui/theme"
	"github.com/eighred/kanz/internal/tui/ui"
)

// Model is the shell.
type Model struct {
	panes  *pane.Registry
	active int

	width, height int
	showHelp      bool

	// lastErr is surfaced in the status bar rather than logged and lost. A TUI
	// has no stderr an operator can see — anything written there is painted over
	// by the next frame — so an error that is not rendered did not happen.
	lastErr error

	quitting bool
}

// New builds the shell over a registry. The first pane is active.
func New(reg *pane.Registry) (Model, error) {
	if reg == nil || reg.Len() == 0 {
		return Model{}, fmt.Errorf("app: shell needs at least one pane")
	}
	return Model{panes: reg}, nil
}

// Init starts the active pane only.
//
// Not every pane: Init is where a pane opens connections and starts polling, and
// starting all of them would have the shell talking to every backend the moment
// it opens, including panes the operator never visits.
func (m Model) Init() tea.Cmd {
	if p, ok := m.panes.At(m.active); ok {
		return p.Init()
	}
	return nil
}

// execFinishedMsg carries the result of a child process back into the shell.
type execFinishedMsg struct{ err error }

// Update routes. Global keys are consumed here; everything else goes to the
// active pane.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// Deliberately falls through to the pane too: a pane that lays itself
		// out needs the size, and swallowing the resize here is why a pane
		// renders at the wrong width until the next keystroke.

	case tea.KeyMsg:
		if cmd, handled := m.globalKey(msg.String()); handled {
			return m, cmd
		}

	case execFinishedMsg:
		// A child that failed must say so. Before this, a mistyped binary name
		// looked identical to a clean, instant exit.
		m.lastErr = msg.err
		return m, nil
	}

	p, ok := m.panes.At(m.active)
	if !ok {
		return m, nil
	}
	updated, cmd := p.Update(msg)
	if updated != nil {
		m.panes.Replace(m.active, updated)
	}
	return m, cmd
}

// globalKey handles the shell's own bindings. The bool says whether the key was
// consumed — an unconsumed key must reach the active pane untouched.
func (m *Model) globalKey(key string) (tea.Cmd, bool) {
	// THE HELP OVERLAY SWALLOWS THE NEXT KEY, and only that. It is a modal, so
	// leaving other bindings live would let an operator tab to another pane
	// while the overlay covers it.
	if m.showHelp {
		m.showHelp = false
		return nil, true
	}

	// ASK THE PANE FIRST. A pane taking typed text must receive the characters
	// somebody types; consuming them as bindings makes it unusable, which is what
	// `h`/`l`/`q`/`?` did to the Copilot prompt in #65.
	switch keymap.LookupFor(key, m.activeAcceptsText()) {
	case keymap.Quit:
		m.quitting = true
		return tea.Quit, true

	case keymap.Help:
		m.showHelp = true
		return nil, true

	case keymap.NextPane:
		return m.selectPane((m.active + 1) % m.panes.Len()), true

	case keymap.PrevPane:
		return m.selectPane((m.active - 1 + m.panes.Len()) % m.panes.Len()), true

	case keymap.SelectPane:
		if n, ok := keymap.PaneOrdinal(key); ok && n < m.panes.Len() {
			return m.selectPane(n), true
		}
		// A bound alt+N past the end is consumed and ignored rather than passed
		// through: alt+7 in a five-pane shell should do nothing, not land in
		// whatever the active pane makes of it.
		return nil, true
	}
	return nil, false
}

// activeAcceptsText reports whether the active pane takes typed characters.
func (m *Model) activeAcceptsText() bool {
	p, ok := m.panes.At(m.active)
	if !ok {
		return false
	}
	_, isText := p.(pane.TextInput)
	return isText
}

// selectPane switches tabs, and is where the two planes diverge.
//
// A gateway pane is shown in-process. A BUS pane is never shown: selecting it
// runs its binary as a child process through tea.ExecProcess, which suspends
// this program and hands over the real terminal. The child keeps its own SPIFFE
// identity, its own NetworkPolicy and its own NATS grant, which is the whole
// reason the shell does not absorb it (see pane's package doc).
//
// The active tab does NOT move to a bus pane. It would be left pointing at a
// screen that cannot render, and on return the operator would face an empty
// pane instead of the one they launched from.
func (m *Model) selectPane(i int) tea.Cmd {
	p, ok := m.panes.At(i)
	if !ok {
		return nil
	}
	if p.Plane() == pane.Bus {
		return m.runBusPane(p)
	}
	m.active = i
	m.lastErr = nil
	return p.Init()
}

// runBusPane suspends the shell and executes the pane's command.
func (m *Model) runBusPane(p pane.Pane) tea.Cmd {
	r, ok := p.(pane.Runner)
	if !ok {
		// Unreachable via NewRegistry, which rejects this at wiring time. Kept
		// because "unreachable" is a claim about today's callers, and the
		// failure it guards is running a bus tool in-process.
		m.lastErr = fmt.Errorf("pane %q is on the bus plane but supplies no command", p.ID())
		return nil
	}
	cmd := r.Command()
	if cmd == nil {
		m.lastErr = fmt.Errorf("pane %q supplied a nil command", p.ID())
		return nil
	}
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return execFinishedMsg{err: wrapExecErr(p.ID(), cmd, err)}
	})
}

// wrapExecErr names the pane and the binary. "exec: not found" alone does not
// say which of several child tools failed, and the operator is looking at a
// shell that just came back with no explanation.
func wrapExecErr(id pane.ID, cmd *exec.Cmd, err error) error {
	if err == nil {
		return nil
	}
	name := ""
	if cmd != nil {
		name = cmd.Path
	}
	return fmt.Errorf("%s (%s): %w", id, name, err)
}

// View renders the frame.
func (m Model) View() string {
	if m.quitting {
		// Leave the terminal on a clean line rather than mid-frame.
		return ""
	}
	if m.width <= 0 || m.height <= 0 {
		// Before the first WindowSizeMsg there is no size to lay out against.
		// Rendering a guessed 80x24 here shows a frame that visibly jumps.
		return ""
	}

	tabs := make([]ui.Tab, 0, m.panes.Len())
	for i := 0; i < m.panes.Len(); i++ {
		p, ok := m.panes.At(i)
		if !ok {
			continue
		}
		tabs = append(tabs, ui.Tab{Title: p.Title(), Plane: p.Plane(), Active: i == m.active})
	}

	body := m.body()
	return ui.Frame(ui.TabBar(tabs, m.width), body, ui.StatusBar(m.status(), m.width), m.width, m.height)
}

func (m Model) body() string {
	if m.showHelp {
		return m.helpView()
	}
	p, ok := m.panes.At(m.active)
	if !ok {
		return ""
	}
	// Body height mirrors ui.Frame's reservation (tab bar, rule, status bar).
	return p.View(m.width, m.height-3)
}

func (m Model) helpView() string {
	out := "  keys\n\n"
	for _, b := range keymap.Global {
		keys := ""
		for i, k := range b.Keys {
			if i > 0 {
				keys += ", "
			}
			keys += k
		}
		out += fmt.Sprintf("  %-28s %s\n", keys, b.Help)
	}
	out += "\n  bus panes (shown in red) leave this shell and run as their own\n"
	out += "  process, keeping their own identity. press any key to close.\n"
	return out
}

func (m Model) status() string {
	if m.lastErr != nil {
		return theme.Error.Render("error: " + m.lastErr.Error())
	}
	p, ok := m.panes.At(m.active)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s  [%s]", p.Title(), p.Plane())
}
