package pane

import (
	"fmt"
	"os"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"
)

// ExecPane is a Bus-plane pane: a tab that launches an existing binary.
//
// ONE IMPLEMENTATION FOR EVERY BUS TOOL. kanz-monitor and kanz-halt differ only
// in which binary they run and what they are called, so they share this rather
// than getting a type each — two near-identical pane types is how the second one
// misses a fix the first one got.
//
// It renders nothing. Selecting the tab hands the terminal to the child, so
// there is no in-shell view to draw; View exists only to satisfy Pane and is
// reached solely if a bus pane is somehow made active, which the router
// prevents.
type ExecPane struct {
	id    ID
	title string
	bin   string
	args  []string
}

// NewExecPane describes a bus-plane tool.
//
// bin is resolved through PATH at LAUNCH time, not here. Resolving at
// construction would make the shell refuse to start when a sibling tool is
// merely absent — the operator would lose every other pane because one binary
// was not installed. A missing binary should fail when it is asked for, and say
// which one.
func NewExecPane(id ID, title, bin string, args ...string) *ExecPane {
	return &ExecPane{id: id, title: title, bin: bin, args: args}
}

func (p *ExecPane) ID() ID        { return p.id }
func (p *ExecPane) Title() string { return p.title }
func (p *ExecPane) Plane() Plane  { return Bus }
func (p *ExecPane) Init() tea.Cmd { return nil }

func (p *ExecPane) Update(tea.Msg) (Pane, tea.Cmd) { return p, nil }

func (p *ExecPane) View(w, _ int) string {
	if w <= 0 {
		return ""
	}
	return fmt.Sprintf("  %s runs as its own process.\n  Select this tab to launch it.", p.bin)
}

// Command builds a fresh *exec.Cmd per launch.
//
// FRESH EVERY TIME, because an exec.Cmd cannot be reused: the second Run on the
// same value returns "exec: already started", so a stored command would work
// once and then fail for the rest of the session.
//
// Stdio is wired to the real terminal. These children are interactive — a
// monitor that paints a live feed and a kill switch that prompts — so piping
// their output into the shell would break both. tea.ExecProcess suspends the
// program first, which is what makes sharing the terminal safe.
func (p *ExecPane) Command() *exec.Cmd {
	c := exec.Command(p.bin, p.args...) //nolint:gosec // bin is a fixed literal from the wiring, never user input
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c
}

// Binary is the command this pane launches. Exported for tests and for the arch
// guard that asserts bus tools are reached by exec rather than imported.
func (p *ExecPane) Binary() string { return p.bin }
