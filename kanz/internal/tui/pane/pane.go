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

// TextInput is implemented by a pane that takes typed characters.
//
// The shell asks before consuming a keystroke as a global binding: a pane that
// is taking text must receive the letters somebody types, or the binding table
// silently steals them. That is not hypothetical — `h`, `l`, `q` and `?` were
// global in #65, which made typing "help" into the Copilot prompt navigate two
// panes and quit the shell (#171's sibling defect).
//
// It is a MARKER, with no behaviour, because the question is about the pane's
// nature rather than its state. A pane that reported "I am taking text right
// now" would move the decision into a mutable flag, and the shell would consume
// or forward the same key differently depending on when it arrived.
type TextInput interface {
	Pane
	// AcceptsTypedText marks this pane as one the global key table must not
	// take printable characters from.
	AcceptsTypedText()
	// SetFocused tells the pane whether the shell is currently in Input mode on
	// it, so it can render the difference. A prompt that looks the same whether
	// or not it will receive keys is the reason a mode has to be visible.
	SetFocused(bool)
}

// Confirming is a Bus pane the shell must SHOW rather than launch on selection.
//
// THE KILL SWITCH IS WHY THIS EXISTS (#171). kanz-halt requires --by, --reason
// and --tenant: a mode change must be attributable to a principal, and
// lifecycle.v1 refuses an unexplained transition. Launched on a keystroke with
// no arguments it could only ever fail, which is exactly what the Halt tab did
// from #65 until now.
//
// Those flags cannot be defaulted. `--by operator:unknown` is WORSE than the
// error: it produces an attributable-looking record that attributes nothing, in
// the append-only log a halt exists to be provable in.
//
// It is the right shape for a second, independent reason. A pane that runs on
// selection puts the platform kill switch one `tab` from the Copilot prompt,
// where a stray keystroke reaches it. A Confirming pane cannot be triggered by
// navigation at all — it is shown, and it acts only once it has what it needs.
type Confirming interface {
	Pane
	// ConfirmBeforeRun marks this pane as one that gathers input first. A
	// marker, not a predicate: whether a tool needs confirmation is a property
	// of the tool, and a runtime flag would invite "temporarily" clearing it.
	ConfirmBeforeRun()
}

// ExecFinished reports a child process's outcome back to the shell.
//
// It lives here rather than in the router because BOTH produce it: the router
// when it launches a plain ExecPane, and a Confirming pane when it launches
// itself. A second copy in the pane package would mean the shell recognised one
// and silently ignored the other — the child would fail and the operator would
// see a shell that simply came back.
type ExecFinished struct {
	ID  ID
	Err error
}
