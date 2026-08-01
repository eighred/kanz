package app

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	zone "github.com/lrstanley/bubblezone"

	"github.com/eighred/kanz/internal/tui/keymap"
	"github.com/eighred/kanz/internal/tui/ui"
)

func click(x, y int) tea.MouseMsg {
	return tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}
}

// ONLY A PRESS ACTS.
//
// bubbletea delivers motion and release alongside the press. Treating every
// mouse event as a click makes a drag across the tab bar select every tab it
// passes over, and makes the release re-fire whatever the press already did.
func TestOnlyALeftPressIsTreatedAsAClick(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  tea.MouseMsg
	}{
		{"motion", tea.MouseMsg{X: 1, Y: 0, Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft}},
		{"release", tea.MouseMsg{X: 1, Y: 0, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft}},
		{"right press", tea.MouseMsg{X: 1, Y: 0, Action: tea.MouseActionPress, Button: tea.MouseButtonRight}},
		{"wheel", tea.MouseMsg{X: 1, Y: 0, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newModel(t, &fake{id: "a", title: "A"}, &fake{id: "b", title: "B"})
			m.showHelp = true
			got, _ := m.Update(tc.msg)
			if !got.(Model).showHelp {
				t.Error("a non-click mouse event was acted on — a drag would select every tab it crosses")
			}
		})
	}
}

// The overlay is modal for the mouse exactly as it is for the keyboard: a click
// dismisses it and does nothing else, rather than acting on whatever is drawn
// underneath it.
func TestAClickDismissesTheHelpOverlayAndNothingElse(t *testing.T) {
	m := newModel(t, &fake{id: "a", title: "A"}, &fake{id: "b", title: "B"})
	m.showHelp = true

	got, _ := m.Update(click(40, 10))
	m2 := got.(Model)
	if m2.showHelp {
		t.Fatal("a click did not dismiss the overlay")
	}
	if m2.active != 0 {
		t.Errorf("the dismissing click also changed pane (active=%d) — the overlay is modal", m2.active)
	}
}

// CLICKING THE BODY OF A TEXT PANE FOCUSES IT.
//
// This is the mouse spelling of `i`, and it goes through the same setMode, so the
// component's focus and the shell's mode cannot end up disagreeing — the split
// that made the prompt render "press i to type" while already receiving keys.
func TestClickingATextPaneBodyEntersInputMode(t *testing.T) {
	p := &textPane{fake: fake{id: "copilot", title: "Copilot"}}
	m := newModel(t, p, &fake{id: "nodes", title: "Nodes"})

	// esc first: the shell opens in Input on a text pane, so start from Navigate.
	got, _ := m.Update(key("esc"))
	m = got.(Model)
	if m.mode != keymap.Navigate {
		t.Fatalf("setup: mode = %v, want Navigate", m.mode)
	}
	if p.focused {
		t.Fatal("setup: the pane is still focused after esc")
	}

	got, _ = m.Update(click(10, 5))
	m = got.(Model)
	if m.mode != keymap.Input {
		t.Errorf("mode = %v after clicking the body, want Input", m.mode)
	}
	if !p.focused {
		t.Error("the pane was not told it is focused — its prompt would still say 'press i to type'")
	}
}

// A read-only pane has nothing to focus, so a body click must fall through to
// the pane rather than being consumed. Consuming it would make the shell eat
// mouse events a pane might want.
func TestClickingAReadOnlyPaneBodyIsNotConsumed(t *testing.T) {
	m := newModel(t, &fake{id: "a", title: "A"}, &fake{id: "b", title: "B"})
	if m.mode != keymap.Navigate {
		t.Fatalf("setup: mode = %v, want Navigate", m.mode)
	}
	got, _ := m.Update(click(10, 5))
	if got.(Model).mode != keymap.Navigate {
		t.Error("clicking a read-only pane changed the mode")
	}
}

// CLICKING A TAB SELECTS IT.
//
// This is the one mouse path that needs real zone coordinates, so it renders
// first and waits for the manager's worker to record them — zone.Scan hands the
// view to a goroutine, so the positions are not readable the instant it returns.
// The wait is bounded and the test fails loudly rather than skipping, because a
// tab bar whose zones never resolve is a tab bar the mouse cannot use at all.
func TestClickingATabSelectsThatPane(t *testing.T) {
	m := newModel(t, &fake{id: "a", title: "A"}, &fake{id: "b", title: "B"}, &fake{id: "c", title: "C"})
	m.width, m.height = 80, 24
	_ = m.View() // marks and scans the tab zones

	target := ui.TabZoneID(2)
	var info *zone.ZoneInfo
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if info = zone.Get(target); !info.IsZero() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if info.IsZero() {
		t.Fatalf("zone %q never resolved after rendering the tab bar — clicks on tabs would do nothing", target)
	}

	got, _ := m.Update(click(info.StartX, info.StartY))
	if active := got.(Model).active; active != 2 {
		t.Errorf("active = %d after clicking tab 2, want 2", active)
	}
}

// THE HELP TEXT MUST NOT LIE.
//
// helpView is hand-written rather than generated from keymap.Global, because an
// operator reads by task ("how do I change pane?") while the table groups by
// action, and the mouse gestures have no binding at all. The cost of writing it
// out is that it can drift, and a help overlay naming a key that does nothing is
// worse than no overlay. This is the check that keeps it honest.
func TestHelpTextOnlyNamesKeysThatAreBound(t *testing.T) {
	m := newModel(t, &fake{id: "a", title: "A"})
	help := m.helpView()

	// Every key the overlay names, in the spelling keymap uses.
	for _, k := range []string{"tab", "left", "right", "i", "enter", "esc", "h", "q"} {
		if keymap.Lookup(k) == keymap.None {
			t.Errorf("the help overlay describes %q but nothing is bound to it", k)
		}
	}
	// And the two printable globals must both appear, since those are the ones an
	// operator cannot discover by pressing arrows.
	for _, want := range []string{"h", "q", "Esc", "Tab"} {
		if !strings.Contains(help, want) {
			t.Errorf("the help overlay does not mention %q", want)
		}
	}
	// The pre-#199 wording named a colour. The shell is monochrome; saying "red"
	// again would send an operator looking for something that cannot be there.
	if strings.Contains(strings.ToLower(help), "red") {
		t.Error("the help overlay still describes bus panes by colour — the shell is black and white")
	}

	// MOUSE REPORTING TOOK DRAG-TO-SELECT AWAY (#200), SO THE OVERLAY SAYS SO
	// (#201). Without it an operator reads the missing selection as "I cannot
	// copy from kanz", and the help overlay is the only place they will look.
	// Asserted here rather than trusted to survive an edit of the copy.
	if !strings.Contains(help, "Shift") {
		t.Error("the help overlay does not say how to select text — mouse reporting means the " +
			"terminal no longer handles drag-to-select, and nothing else tells the operator")
	}
}

// h and l are NOT bound any more: they are printable, and a printable global is
// a character the pane taking text never receives. Navigation is arrows and tab.
func TestVimNavigationKeysAreNoLongerGlobal(t *testing.T) {
	if act := keymap.Lookup("l"); act != keymap.None {
		t.Errorf("l is bound to %v — it was removed so the prompt can receive it", act)
	}
	if act := keymap.Lookup("h"); act != keymap.Help {
		t.Errorf("h is bound to %v, want Help", act)
	}
}
