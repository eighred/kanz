package theme

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// styles is every exported style, so this file cannot silently miss one added
// later. A new style that is not listed here is not guarded — and the whole
// point of a single palette is that the visual surface is reviewable in one
// place.
var styles = map[string]lipgloss.Style{
	"ActiveTab":   ActiveTab,
	"InactiveTab": InactiveTab,
	"BusTab":      BusTab,
	"StatusBar":   StatusBar,
	"Rule":        Rule,
	"Body":        Body,
	"Prompt":      Prompt,
	"Error":       Error,
}

// THE SHELL IS BLACK AND WHITE (owner decision, 2026-08-01).
//
// A guard rather than a comment, because "no colour" is exactly the kind of rule
// that decays one convenient exception at a time — someone adds a red for an
// error they care about, and the next person matches it.
//
// It also removes a class of bug: a hardcoded foreground has a light-terminal
// and a dark-terminal form to get wrong, and getting it wrong is invisible to
// whoever wrote it. Attributes have no such form.
func TestNoStyleSetsAColour(t *testing.T) {
	for name, s := range styles {
		if fg := s.GetForeground(); fg != (lipgloss.NoColor{}) {
			t.Errorf("%s sets a foreground colour (%v) — the shell is black and white; "+
				"distinguish with bold/faint/underline/reverse instead", name, fg)
		}
		if bg := s.GetBackground(); bg != (lipgloss.NoColor{}) {
			t.Errorf("%s sets a background colour (%v) — the shell is black and white", name, bg)
		}
	}
}

// THE BUS TAB MUST STAY DISTINGUISHABLE WITHOUT READING.
//
// This is a security property, not decoration. Selecting a Bus tab suspends the
// shell and hands the terminal to another program, and one of those programs is
// the platform kill switch. It used to be red; dropping colour could have
// quietly dropped the guarantee with it, which is the failure this test exists
// to prevent.
//
// Reverse is asserted specifically, and asserted to be UNIQUE, so the signal
// cannot be diluted by a second style adopting it.
func TestBusTabIsUnmistakableAndUniquelySo(t *testing.T) {
	if !BusTab.GetReverse() {
		t.Fatal("BusTab is no longer reverse video — the panes that leave this process, one of " +
			"which is the kill switch, must be visible as different WITHOUT READING")
	}
	for name, s := range styles {
		if name == "BusTab" {
			continue
		}
		if s.GetReverse() {
			t.Errorf("%s also uses reverse video — that attribute is BusTab's signal, and a "+
				"second user of it makes the kill-switch tab ambiguous", name)
		}
	}
}

// attrs is the set of attributes a style carries. Compared instead of rendered
// output because lipgloss strips every attribute when it detects no TTY — as in
// a test run — so comparing Render() would assert something about the test
// environment rather than about the theme. That is not hypothetical: the first
// version of the two tests below did exactly that and reported every style as
// identical to Body.
type attrs struct{ bold, faint, underline, reverse, italic bool }

func attrsOf(s lipgloss.Style) attrs {
	return attrs{
		bold:      s.GetBold(),
		faint:     s.GetFaint(),
		underline: s.GetUnderline(),
		reverse:   s.GetReverse(),
		italic:    s.GetItalic(),
	}
}

// NON-VACUITY: removing colour must not have flattened everything into the same
// rendering. Each style still has to carry SOMETHING, or the shell has no visual
// structure at all and the tests above are satisfied by a theme that draws
// nothing.
func TestEveryStyleStillDiffersFromPlainBody(t *testing.T) {
	plain := attrsOf(Body)
	if plain != (attrs{}) {
		t.Fatalf("Body carries attributes %+v — it is meant to be the terminal's plain foreground", plain)
	}
	for name, s := range styles {
		if name == "Body" {
			continue
		}
		if attrsOf(s) == plain {
			t.Errorf("%s carries no attributes at all — dropping colour flattened it, so this "+
				"distinction now exists only in the source", name)
		}
	}
}

// And the styles that are read AGAINST each other must actually differ.
func TestStylesReadAgainstEachOtherDiffer(t *testing.T) {
	for _, pair := range []struct {
		a, b   string
		reason string
	}{
		{"ActiveTab", "InactiveTab", "nothing would show which pane is in front"},
		{"Error", "Prompt", "a failure would look like a prompt"},
		{"BusTab", "InactiveTab", "the kill-switch tab would look like an ordinary one"},
	} {
		if attrsOf(styles[pair.a]) == attrsOf(styles[pair.b]) {
			t.Errorf("%s and %s carry identical attributes — %s", pair.a, pair.b, pair.reason)
		}
	}
}
