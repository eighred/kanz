package halt

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/eighred/kanz/internal/tui/pane"
)

func typeText(p *Pane, s string) {
	for _, r := range s {
		p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func press(p *Pane, k tea.KeyType) tea.Cmd {
	_, cmd := p.Update(tea.KeyMsg{Type: k})
	return cmd
}

// fill drives the form to a complete, runnable state.
func fill(p *Pane, by, reason, tenant string) {
	typeText(p, by)
	press(p, tea.KeyDown)
	typeText(p, reason)
	press(p, tea.KeyDown)
	typeText(p, tenant)
}

// THE ASSERTION #65's TESTS WERE MISSING.
//
// The shell tests asserted an exec was ISSUED, never that the child could run.
// That is a correct unit test of routing and it is why the Halt tab shipped
// launching kanz-halt with no arguments, exiting instantly with "--by is
// required" (#171). This asserts the argv the child would actually receive.
func TestSubmittedArgvCarriesEveryRequiredFlag(t *testing.T) {
	p := New()
	fill(p, "operator:akif", "risk breach on fund-alpha", "load-test")

	if cmd := press(p, tea.KeyEnter); cmd == nil {
		t.Fatal("enter on a complete form produced no command")
	}

	got := strings.Join(p.Args(), " ")
	for _, want := range []string{
		"--by operator:akif",
		"--reason risk breach on fund-alpha",
		"--tenant load-test",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("argv %q is missing %q — kanz-halt would refuse it", got, want)
		}
	}
	if strings.Contains(got, "--resume") {
		t.Errorf("argv %q carries --resume for a halt", got)
	}
}

// A REASON IS FREE TEXT and reaches the child exactly as typed. argv is a slice,
// so nothing is interpreted on the way — this is the property that makes an
// apostrophe or a quote safe rather than a syntax error.
func TestReasonReachesTheChildVerbatim(t *testing.T) {
	reason := `it's down: "risk" breach, 50% margin`
	p := New()
	fill(p, "operator:akif", reason, "load-test")

	args := p.Args()
	found := false
	for i, a := range args {
		if a == "--reason" && i+1 < len(args) {
			if args[i+1] != reason {
				t.Errorf("--reason = %q, want %q verbatim", args[i+1], reason)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("no --reason in argv: %v", args)
	}
	// One argv element, not split on spaces — the whole point of not going
	// through a shell.
	if len(args) != 6 {
		t.Errorf("argv has %d elements (%v), want 6 — the reason was split", len(args), args)
	}
}

// NOTHING RUNS UNTIL ALL THREE ARE PRESENT, and the refusal names every missing
// field at once rather than one per attempt.
func TestAnIncompleteFormRefusesToRun(t *testing.T) {
	p := New()

	if cmd := press(p, tea.KeyEnter); cmd != nil {
		t.Fatal("enter on an empty form produced a command — kanz-halt would run with no attribution")
	}
	if p.err == nil {
		t.Fatal("an empty form was refused silently")
	}
	for _, want := range []string{"--by", "--reason", "--tenant"} {
		if !strings.Contains(p.err.Error(), want) {
			t.Errorf("refusal %q does not name %s — the operator must not have to guess which field",
				p.err.Error(), want)
		}
	}

	// Two of three is still refused, and the message narrows.
	typeText(p, "operator:akif")
	press(p, tea.KeyDown)
	typeText(p, "a reason")
	if cmd := press(p, tea.KeyEnter); cmd != nil {
		t.Error("enter with no --tenant produced a command")
	}
	if !strings.Contains(p.err.Error(), "--tenant") || strings.Contains(p.err.Error(), "--by ") {
		t.Errorf("refusal %q should name only the still-missing field", p.err.Error())
	}
}

// WHITESPACE IS NOT A VALUE. A reason of "   " would satisfy a naive non-empty
// check and record an unexplained transition, which is the thing lifecycle.v1
// refuses.
func TestWhitespaceDoesNotSatisfyARequiredField(t *testing.T) {
	p := New()
	fill(p, "operator:akif", "   ", "load-test")
	if cmd := press(p, tea.KeyEnter); cmd != nil {
		t.Error("a whitespace-only reason was accepted — the FACT would carry no explanation")
	}
}

// SELECTING THE TAB MUST NOT RUN ANYTHING. This is the safety interlock: before
// #171 the kill switch ran on arrival, one `tab` from the Copilot prompt.
func TestThePaneIsShownRatherThanLaunched(t *testing.T) {
	var p pane.Pane = New()
	if _, ok := p.(pane.Confirming); !ok {
		t.Fatal("the halt pane is not pane.Confirming — the shell would launch it on selection, " +
			"putting the kill switch one keystroke from the Copilot prompt")
	}
	if p.Plane() != pane.Bus {
		t.Errorf("Plane() = %v, want Bus — kanz-halt holds its own SVID and publishes the FACT itself",
			p.Plane())
	}
	if _, ok := p.(pane.Runner); !ok {
		t.Error("the halt pane is not pane.Runner — the registry rejects a bus pane with no command")
	}
	// Typed text must reach the fields rather than the global key table (#172).
	if _, ok := p.(pane.TextInput); !ok {
		t.Error("the halt pane does not accept typed text — typing a reason containing h, l or q " +
			"would navigate panes and quit the shell")
	}
}

// --resume goes through the SAME form. Reopening the gate is an attributable,
// recorded act too; only routing it through the halt path would leave one of the
// two mode changes unexplained.
func TestResumeIsTheSameFormWithAFlag(t *testing.T) {
	p := New()
	fill(p, "operator:akif", "initial deployment validation", "load-test")

	press(p, tea.KeyDown) // onto the mode field
	p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})

	if !p.resume {
		t.Fatal("space on the mode field did not toggle to resume")
	}
	got := strings.Join(p.Args(), " ")
	if !strings.Contains(got, "--resume") {
		t.Errorf("argv %q is missing --resume", got)
	}
	if !strings.Contains(got, "--by operator:akif") {
		t.Errorf("argv %q lost the attribution when toggling mode", got)
	}
}

// A refusal must clear as soon as the operator starts fixing it, or a stale
// complaint sits under a field that now has a value.
func TestTheRefusalClearsOnEdit(t *testing.T) {
	p := New()
	press(p, tea.KeyEnter)
	if p.err == nil {
		t.Fatal("no refusal to clear")
	}
	typeText(p, "o")
	if p.err != nil {
		t.Errorf("refusal %q survived an edit", p.err.Error())
	}
}

// Backspace removes a whole rune. A reason will contain non-ASCII, and trimming
// a byte at a time leaves invalid UTF-8 in an audit record.
func TestBackspaceRemovesAWholeRune(t *testing.T) {
	p := New()
	typeText(p, "aé")
	press(p, tea.KeyBackspace)
	if p.by != "a" {
		t.Errorf("by = %q after backspace over a multi-byte rune, want %q", p.by, "a")
	}
}
