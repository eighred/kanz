// Package pane is the unit the kanz shell routes between: one screen, one
// purpose, addressable by a stable id.
//
// It carries the one distinction the shell cannot get wrong. kanz replaces four
// terminal binaries that span TWO AUTH PLANES (#65):
//
//	Gateway  kanz, universe          an SSO device-flow token — a HUMAN identity
//	Bus      kanz-halt, kanz-monitor one SPIFFE SVID PER TOOL, each with its own
//	                                 NetworkPolicy and NATS publish grant
//
// A gateway pane can live in this process: it reuses the token the shell already
// holds, so nothing is granted that the operator did not already have. A bus
// pane cannot. Running kanz-monitor's code in the same process as kanz-halt's
// would need one identity that is simultaneously a read-only observer and the
// kill switch — five least-privilege identities collapsed into one, and
// cmd/kanz-halt/main.go's stated reason for existing ("the one tool that must
// work while the system is on fire cannot be coupled to the system that is on
// fire") discarded.
//
// So Plane is not a label. It selects the execution strategy, and the shell
// refuses to run a Bus pane in-process.
package pane

import (
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"
)

// ID addresses a pane. Stable: it appears in key bindings and tests, and it is
// what a pane is looked up by, so renaming one is a deliberate change.
type ID string

// Plane is the authentication boundary a pane runs on. See the package doc —
// this is a security property, not a display hint.
type Plane int

const (
	// Gateway panes talk to the api-gateway with the shell's own SSO token and
	// run inside this process.
	Gateway Plane = iota
	// Bus panes hold their own SPIFFE SVID and MUST run as a child process.
	Bus
)

func (p Plane) String() string {
	switch p {
	case Gateway:
		return "gateway"
	case Bus:
		return "bus"
	default:
		return "unknown"
	}
}

// Pane is one screen of the shell.
//
// Update returns a Pane rather than mutating through a pointer receiver so a
// pane can be a value type, which is bubbletea's own convention and keeps a
// pane's state changes explicit at the call site.
type Pane interface {
	ID() ID
	// Title is what the tab bar shows.
	Title() string
	// Plane decides in-process vs child process. See the package doc.
	Plane() Plane
	Init() tea.Cmd
	Update(msg tea.Msg) (Pane, tea.Cmd)
	// View renders into exactly w columns and h rows. A pane that returns more
	// will be clipped by the shell rather than allowed to corrupt the layout.
	View(w, h int) string
}

// Runner is implemented by a Bus pane: it hands back the command to execute.
//
// The pane does not run it. The shell does, through tea.ExecProcess, which
// suspends the program and gives the child the real terminal — the child is
// itself interactive, so sharing a terminal rather than piping is the point.
type Runner interface {
	Pane
	// Command is built fresh per invocation. Returning a stored *exec.Cmd would
	// break the second launch: an exec.Cmd cannot be reused after Run.
	Command() *exec.Cmd
}
