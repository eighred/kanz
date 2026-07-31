// Package halt is the shell's confirmation form for the platform kill switch
// (#171).
//
// kanz-halt requires --by, --reason and --tenant. A mode change must be
// attributable to a principal, and lifecycle.v1 refuses an unexplained
// transition. The Halt tab shipped in #65 launched it with none of them, so it
// exited immediately with "--by is required" — the tab had never once been able
// to do the thing it exists for.
//
// NONE OF THE THREE CAN BE DEFAULTED, and that is the design, not an obstacle.
// `--by operator:unknown` is worse than the error: it writes an
// attributable-looking record that attributes nothing, into the append-only log
// a halt exists in order to be provable in. So the shell asks.
//
// THE FORM IS ALSO THE SAFETY INTERLOCK. Before it, the kill switch sat one
// `tab` from the Copilot prompt and ran on arrival. A pane.Confirming pane
// cannot be triggered by navigation at all: tabbing here shows a form, and
// nothing happens until three fields are filled and enter is pressed.
//
// It holds NO bus code. It collects three strings and passes them as arguments;
// kanz-halt keeps its own SPIFFE identity and publishes the FACT itself. The
// shell still links no bus, and TestKanzShellDoesNotLinkTheBus still holds.
package halt

import (
	"fmt"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/eighred/kanz/internal/tui/pane"
	"github.com/eighred/kanz/internal/tui/theme"
	"github.com/eighred/kanz/internal/tui/ui"
)

// ID is this pane's stable address.
const ID pane.ID = "halt"

// binary is the tool this pane runs. Not configurable: the kill switch is one
// specific program, and a pane that could be pointed at another one is a
// different and much worse feature.
const binary = "kanz-halt"

// field indexes, in the order an operator fills them.
const (
	fieldBy = iota
	fieldReason
	fieldTenant
	fieldMode
	fieldCount
)

// Pane is the halt confirmation form.
type Pane struct {
	by, reason, tenant string
	// resume selects `--resume` (reopen the gate) instead of a halt. Both are
	// the same act — an attributable, recorded mode change — so both go through
	// this form rather than only the destructive one.
	resume bool

	focus int
	// focused mirrors the shell's Input mode — see SetFocused.
	focused bool
	// err is the last refusal, shown until the operator fixes it. Cleared on any
	// edit so a stale complaint does not sit under a field that now has a value.
	err error
}

// New builds the form. Empty: nothing is pre-filled, deliberately. A remembered
// --by would be the previous operator's name attached to this operator's act.
func New() *Pane { return &Pane{} }

func (p *Pane) ID() pane.ID   { return ID }
func (p *Pane) Title() string { return "Halt" }

// Plane is Bus: the work happens in kanz-halt, which holds its own SVID and
// publishes platform.mode.changed. This pane only gathers arguments.
func (p *Pane) Plane() pane.Plane { return pane.Bus }

// ConfirmBeforeRun marks this pane as one the shell shows rather than launches.
func (p *Pane) ConfirmBeforeRun() {}

// AcceptsTypedText: three of the four fields are free text, so the global key
// table must not take printable characters from this pane (#172). Without it,
// typing a reason containing "h", "l" or "q" would navigate panes and quit the
// shell mid-sentence.
func (p *Pane) AcceptsTypedText() {}

// SetFocused mirrors the shell's Input mode. The form shows it on the hint line
// rather than the fields: a half-lit field is harder to read than one sentence
// saying whether keys are landing.
func (p *Pane) SetFocused(v bool) { p.focused = v }

func (p *Pane) Init() tea.Cmd { return nil }

// Command builds the argv for kanz-halt.
//
// ARGV, NEVER A SHELL STRING. --reason is free text and will contain spaces,
// quotes and apostrophes; exec.Command takes a slice, so the argument arrives
// exactly as typed and nothing is interpreted on the way. Building a command
// line and handing it to a shell is how a reason like `it's down` becomes a
// syntax error, or worse.
func (p *Pane) Command() *exec.Cmd {
	args := []string{"--by", p.by, "--reason", p.reason, "--tenant", p.tenant}
	if p.resume {
		args = append(args, "--resume")
	}
	// Resolved through pane.ToolCommand, not exec.Command: on Windows a bare
	// name is refused when it is only reachable from the working directory
	// (exec.ErrDot), and for the KILL SWITCH specifically that refusal is not an
	// inconvenience — it is what stops a kanz-halt.exe dropped in whatever
	// directory the operator happened to be in from becoming the platform's
	// halt.
	return pane.ToolCommand(binary, args...)
}

// Args is the argv this pane would run, for tests and for the status line. It
// deliberately returns what Command builds rather than re-deriving it — two
// derivations is how the displayed command stops matching the run one.
func (p *Pane) Args() []string { return p.Command().Args[1:] }

// validate reports what is missing, naming every empty field at once rather
// than one per attempt.
func (p *Pane) validate() error {
	var missing []string
	if strings.TrimSpace(p.by) == "" {
		missing = append(missing, "--by (who is doing this, e.g. operator:akif)")
	}
	if strings.TrimSpace(p.reason) == "" {
		missing = append(missing, "--reason (why; it is recorded in the FACT)")
	}
	if strings.TrimSpace(p.tenant) == "" {
		missing = append(missing, "--tenant (the envelope tenant_id)")
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("cannot run: %s", strings.Join(missing, "; "))
}

func (p *Pane) Update(msg tea.Msg) (pane.Pane, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return p, nil
	}

	switch key.String() {
	case "up":
		p.focus = (p.focus - 1 + fieldCount) % fieldCount
		return p, nil
	case "down":
		p.focus = (p.focus + 1) % fieldCount
		return p, nil

	case "left", "right", " ":
		if p.focus == fieldMode {
			p.resume = !p.resume
			return p, nil
		}
		// On a text field these are ordinary input positions the form does not
		// support yet; swallowing them is better than inserting a literal space
		// where the operator expected a cursor move.
		if key.String() == " " {
			p.edit(func(s string) string { return s + " " })
		}
		return p, nil

	case "enter":
		if err := p.validate(); err != nil {
			p.err = err
			return p, nil
		}
		p.err = nil
		return p, p.run()

	case "backspace":
		p.err = nil
		p.edit(func(s string) string {
			r := []rune(s)
			if len(r) == 0 {
				return s
			}
			return string(r[:len(r)-1])
		})
		return p, nil
	}

	if k := key.String(); len([]rune(k)) == 1 && !strings.Contains(k, "+") {
		p.err = nil
		p.edit(func(s string) string { return s + k })
	}
	return p, nil
}

// edit applies fn to the focused text field. The mode field holds no text.
func (p *Pane) edit(fn func(string) string) {
	switch p.focus {
	case fieldBy:
		p.by = fn(p.by)
	case fieldReason:
		p.reason = fn(p.reason)
	case fieldTenant:
		p.tenant = fn(p.tenant)
	}
}

// run launches kanz-halt and reports the outcome to the shell.
func (p *Pane) run() tea.Cmd {
	cmd := p.Command()
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		if err != nil {
			err = fmt.Errorf("%s: %w", binary, err)
		}
		return pane.ExecFinished{ID: ID, Err: err}
	})
}

func (p *Pane) View(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	action := "HALT — stops execution platform-wide"
	if p.resume {
		action = "RESUME — reopens the gate"
	}

	lines := []string{
		"  " + theme.Error.Render(action),
		"",
		p.field("by      ", p.by, fieldBy, "operator:akif"),
		p.field("reason  ", p.reason, fieldReason, "why this is happening"),
		p.field("tenant  ", p.tenant, fieldTenant, "envelope tenant_id"),
		p.modeField(),
		"",
	}
	if p.err != nil {
		lines = append(lines, "  "+theme.Error.Render(p.err.Error()), "")
	}
	lines = append(lines,
		"  "+theme.StatusBar.Render(p.hint()))

	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, ui.Clip(l, w))
	}
	return strings.Join(out, "\n")
}

// hint says what the keys do right now, which differs by mode: in Navigate the
// form is inert and the shell's bindings are live.
func (p *Pane) hint() string {
	if !p.focused {
		return "press i to fill this in · nothing runs until then"
	}
	return "↑/↓ field · ←/→ or space toggles mode · enter runs · esc to navigate"
}

func (p *Pane) field(label, value string, idx int, placeholder string) string {
	marker := "  "
	if p.focus == idx {
		marker = theme.Prompt.Render("▸ ")
	}
	shown := value
	if shown == "" {
		shown = theme.StatusBar.Render(placeholder)
	}
	return marker + theme.Body.Render(label) + shown
}

func (p *Pane) modeField() string {
	marker := "  "
	if p.focus == fieldMode {
		marker = theme.Prompt.Render("▸ ")
	}
	mode := "halt"
	if p.resume {
		mode = "resume"
	}
	return marker + theme.Body.Render("mode    ") + mode
}
