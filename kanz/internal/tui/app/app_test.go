package app

import (
	"os/exec"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/eighred/kanz/internal/tui/keymap"
	"github.com/eighred/kanz/internal/tui/pane"
)

type fake struct {
	id      pane.ID
	title   string
	plane   pane.Plane
	gotKeys []string
}

func (f *fake) ID() pane.ID          { return f.id }
func (f *fake) Title() string        { return f.title }
func (f *fake) Plane() pane.Plane    { return f.plane }
func (f *fake) Init() tea.Cmd        { return nil }
func (f *fake) View(int, int) string { return "body:" + f.title }

func (f *fake) Update(msg tea.Msg) (pane.Pane, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		f.gotKeys = append(f.gotKeys, k.String())
	}
	return f, nil
}

func key(s string) tea.KeyMsg {
	if len(s) == 1 {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
	switch s {
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func newModel(t *testing.T, panes ...pane.Pane) Model {
	t.Helper()
	reg, err := pane.NewRegistry(panes...)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	m, err := New(reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.width, m.height = 80, 24
	return m
}

func TestTabCyclesPanesAndWraps(t *testing.T) {
	m := newModel(t,
		&fake{id: "a", title: "A"},
		&fake{id: "b", title: "B"},
		&fake{id: "c", title: "C"},
	)
	for _, want := range []int{1, 2, 0} { // wraps back to the first
		got, _ := m.Update(key("tab"))
		m = got.(Model)
		if m.active != want {
			t.Fatalf("after tab, active = %d, want %d", m.active, want)
		}
	}
	got, _ := m.Update(key("shift+tab"))
	m = got.(Model)
	if m.active != 2 {
		t.Errorf("after shift+tab from 0, active = %d, want 2 (wraps backwards)", m.active)
	}
}

// A GLOBAL KEY MUST NOT REACH THE PANE, and everything else must.
//
// Both halves matter. A shell that forwards its own bindings makes panes handle
// keys they never see the effect of; one that swallows unbound keys steals the
// input an operator is typing.
func TestGlobalKeysAreConsumedAndOthersReachThePane(t *testing.T) {
	p := &fake{id: "a", title: "A"}
	m := newModel(t, p, &fake{id: "b", title: "B"})

	for _, k := range []string{"tab", "?"} {
		got, _ := m.Update(key(k))
		m = got.(Model)
	}
	for _, k := range p.gotKeys {
		if k == "tab" || k == "?" {
			t.Errorf("global key %q reached the pane", k)
		}
	}

	// "x" is unbound, so it belongs to the pane. (The help overlay opened above
	// swallows one key, so close it first.)
	got, _ := m.Update(key("z"))
	m = got.(Model)
	got, _ = m.Update(key("x"))
	m = got.(Model)

	active, _ := m.panes.At(m.active)
	fp := active.(*fake)
	found := false
	for _, k := range fp.gotKeys {
		if k == "x" {
			found = true
		}
	}
	if !found {
		t.Errorf("unbound key never reached the active pane; pane saw %v", fp.gotKeys)
	}
}

// SELECTING A BUS PANE MUST NOT MOVE THE ACTIVE TAB.
//
// A bus pane renders nothing — it runs as a child process. Leaving the tab on it
// would strand the operator on a blank screen when the child exits, instead of
// returning them to the pane they launched from.
func TestSelectingABusPaneDoesNotChangeTheActiveTab(t *testing.T) {
	m := newModel(t,
		&fake{id: "copilot", title: "Copilot"},
		pane.NewExecPane("halt", "Halt", "kanz-halt"),
	)
	before := m.active

	got, cmd := m.Update(key("tab")) // tab onto the bus pane
	m = got.(Model)

	if m.active != before {
		t.Errorf("active tab moved to a bus pane (%d -> %d) — it renders nothing, so the operator "+
			"would face a blank screen", before, m.active)
	}
	if cmd == nil {
		t.Error("selecting a bus pane produced no command — the tool would never launch")
	}
}

// A failed child must be surfaced. Before this the shell returned from a
// mistyped binary looking exactly like a clean, instant exit.
func TestChildProcessErrorIsSurfacedInTheStatusBar(t *testing.T) {
	m := newModel(t, &fake{id: "a", title: "A"})
	got, _ := m.Update(pane.ExecFinished{Err: errFake{}})
	m = got.(Model)

	if m.lastErr == nil {
		t.Fatal("a failed child process left no error on the model")
	}
	if !strings.Contains(m.status(), "boom") {
		t.Errorf("status = %q, want it to carry the child's error", m.status())
	}
}

type errFake struct{}

func (errFake) Error() string { return "boom" }

// The frame must fit the terminal exactly: a pane that returns more lines than
// the body budget must be clipped, not allowed to push the status bar off.
func TestViewNeverExceedsTheTerminalHeight(t *testing.T) {
	tall := &fake{id: "a", title: "A"}
	m := newModel(t, tall)
	m.height = 10

	lines := strings.Split(m.View(), "\n")
	if len(lines) > m.height {
		t.Errorf("View() rendered %d lines into a %d-row terminal — the status bar would be pushed "+
			"off screen", len(lines), m.height)
	}
}

// Before the first WindowSizeMsg there is no size to lay out against. Rendering
// a guessed 80x24 shows a frame that visibly jumps on the next redraw.
func TestViewIsEmptyBeforeTheFirstResize(t *testing.T) {
	reg, err := pane.NewRegistry(&fake{id: "a", title: "A"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	m, err := New(reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := m.View(); got != "" {
		t.Errorf("View() before any resize = %q, want empty", got)
	}
}

// The resize must reach the pane as well as the shell, or a pane that lays
// itself out renders at the wrong width until the next keystroke.
func TestResizeIsForwardedToTheActivePane(t *testing.T) {
	p := &sizePane{fake: fake{id: "a", title: "A"}}
	m := newModel(t, p)
	got, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = got.(Model)

	active, _ := m.panes.At(0)
	if !active.(*sizePane).sawResize {
		t.Error("WindowSizeMsg was consumed by the shell and never reached the pane")
	}
	if m.width != 120 || m.height != 40 {
		t.Errorf("shell size = %dx%d, want 120x40", m.width, m.height)
	}
}

type sizePane struct {
	fake
	sawResize bool
}

func (p *sizePane) Update(msg tea.Msg) (pane.Pane, tea.Cmd) {
	if _, ok := msg.(tea.WindowSizeMsg); ok {
		p.sawResize = true
	}
	return p, nil
}

// textPane accepts typed characters, like the Copilot pane.
type textPane struct {
	fake
	focused bool
}

func (t *textPane) AcceptsTypedText() {}
func (t *textPane) SetFocused(v bool) { t.focused = v }

// Update must return the textPane ITSELF, not the embedded fake. The promoted
// fake.Update returns *fake, and the registry would accept that swap — the ids
// match — leaving a pane that no longer implements TextInput after one
// keystroke. That is a real property of Registry.Replace worth knowing: it
// verifies the id, not the type.
func (t *textPane) Update(msg tea.Msg) (pane.Pane, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		t.gotKeys = append(t.gotKeys, k.String())
	}
	return t, nil
}

// A PANE TAKING TYPED TEXT MUST RECEIVE THE CHARACTERS SOMEBODY TYPES.
//
// This is the defect #65 shipped and this test did not catch: the routing test
// above probes with "x" and "z", which happen to be unbound, so it proved the
// mechanism and said nothing about the table. Meanwhile `h`, `l`, `q` and `?`
// were global — typing "help" into the Copilot prompt sent h to prev-pane, l to
// next-pane, and q quit the shell mid-sentence.
//
// The letters asserted here are the ACTUAL bindings, not arbitrary ones.
func TestATextPaneReceivesTheLettersThatAreAlsoBindings(t *testing.T) {
	p := &textPane{fake: fake{id: "copilot", title: "Copilot"}}
	m := newModel(t, p, &fake{id: "b", title: "B"})

	for _, k := range []string{"h", "l", "q", "?"} {
		got, _ := m.Update(key(k))
		m = got.(Model)
	}

	active, _ := m.panes.At(m.active)
	tp := active.(*textPane)
	for _, want := range []string{"h", "l", "q", "?"} {
		found := false
		for _, got := range tp.gotKeys {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%q never reached the text pane — the global table swallowed a character "+
				"somebody was typing; pane saw %v", want, tp.gotKeys)
		}
	}
	if m.active != 0 {
		t.Errorf("typing moved the active pane to %d — h/l navigated instead of being typed", m.active)
	}
	if m.quitting {
		t.Error("typing \"q\" quit the shell")
	}
}

// The keys a text field never produces must still work, or a text pane becomes a
// trap with no way out.
func TestATextPaneStillHonoursModifierAndTabBindings(t *testing.T) {
	m := newModel(t,
		&textPane{fake: fake{id: "copilot", title: "Copilot"}},
		&fake{id: "b", title: "B"},
	)
	got, _ := m.Update(key("tab"))
	m = got.(Model)
	if m.active != 1 {
		t.Errorf("tab did not move panes from a text pane (active=%d) — the pane would be a trap", m.active)
	}
}

// A read-only pane keeps the full table, including the PRINTABLE global. The
// modal fix must not cost every pane its bindings.
//
// The exemplar used to be `l` (vim-style next-pane). Navigation moved to arrows
// and tab, and `h` became help, so `h` is now the printable global this asserts
// with — the property is unchanged, only the key that carries it.
func TestANonTextPaneKeepsThePrintableGlobal(t *testing.T) {
	m := newModel(t, &fake{id: "a", title: "A"}, &fake{id: "b", title: "B"})
	got, _ := m.Update(key("h"))
	m = got.(Model)
	if !m.showHelp {
		t.Error("h did not open help on a read-only pane — the printable global was swallowed")
	}
}

// confirmingPane is a Bus pane that must gather input first, like the kill
// switch (#171).
type confirmingPane struct {
	fake
	launched bool
}

func (c *confirmingPane) Plane() pane.Plane { return pane.Bus }
func (c *confirmingPane) ConfirmBeforeRun() {}
func (c *confirmingPane) Command() *exec.Cmd {
	c.launched = true
	return exec.Command("kanz-halt")
}

func (c *confirmingPane) Update(msg tea.Msg) (pane.Pane, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		c.gotKeys = append(c.gotKeys, k.String())
	}
	return c, nil
}

// A CONFIRMING BUS PANE IS SHOWN, NEVER LAUNCHED ON SELECTION.
//
// This is the safety interlock, and without this test it was not enforced:
// making the router ignore pane.Confirming left every existing test green while
// the platform kill switch ran on arrival, one `tab` from the Copilot prompt.
//
// TestSelectingABusPaneDoesNotChangeTheActiveTab covers the OTHER bus shape (a
// plain ExecPane, which SHOULD launch immediately), so it cannot cover this one.
func TestAConfirmingBusPaneIsShownRatherThanLaunched(t *testing.T) {
	halt := &confirmingPane{fake: fake{id: "halt", title: "Halt"}}
	m := newModel(t, &fake{id: "copilot", title: "Copilot"}, halt)

	got, cmd := m.Update(key("tab")) // tab onto the kill switch
	m = got.(Model)

	if cmd != nil {
		t.Error("selecting a confirming pane produced a command — the kill switch would run on a " +
			"keystroke, with none of the arguments it requires")
	}
	if halt.launched {
		t.Error("Command() was called on selection — kanz-halt would have been executed by navigation")
	}
	if m.active != 1 {
		t.Errorf("active = %d, want 1 — a confirming pane must become visible so it can collect input",
			m.active)
	}
}

// THE SHELL OPENS READY TO TYPE. The first pane is the Copilot prompt, and
// asking for a keystroke before the product's primary action is friction with
// nothing bought by it.
func TestTheShellOpensInInputModeOnATextPane(t *testing.T) {
	p := &textPane{fake: fake{id: "copilot", title: "Copilot"}}
	m := newModel(t, p, &fake{id: "nodes", title: "Nodes"})

	if m.mode != keymap.Input {
		t.Errorf("mode = %v on open, want Input — the Copilot prompt would need a keystroke first", m.mode)
	}
	if !p.focused {
		t.Error("the pane was not told it has focus, so its prompt cannot say whether keys land")
	}
}

// A READ-ONLY PANE IS NEVER IN INPUT MODE, so the whole binding table is live
// there — including the vim keys it was written for.
func TestSwitchingToAReadOnlyPaneReturnsToNavigate(t *testing.T) {
	m := newModel(t,
		&textPane{fake: fake{id: "copilot", title: "Copilot"}},
		&fake{id: "nodes", title: "Nodes"},
	)
	got, _ := m.Update(key("tab"))
	m = got.(Model)

	if m.mode != keymap.Navigate {
		t.Errorf("mode = %v on a read-only pane, want Navigate", m.mode)
	}
	// And the printable global acts again rather than being swallowed.
	got, _ = m.Update(key("h"))
	m = got.(Model)
	if !m.showHelp {
		t.Error("h did not open help in Navigate mode — the printable global was swallowed")
	}
}

// ONE KEY, ONE MEANING PER MODE. This is the property the whole modal change
// exists for: the same printable character types in Input and acts globally in
// Navigate, and which one is in force is a mode the operator can see — not a
// function of which tab is open.
//
// Asserted with `h` since navigation moved to arrows and tab (#65's `l` is no
// longer bound at all). `h` is now the only printable global besides `q`, which
// makes it the sharpest test of this property rather than merely a surviving one.
func TestTheSamePrintableKeyTypesInInputAndActsInNavigate(t *testing.T) {
	p := &textPane{fake: fake{id: "copilot", title: "Copilot"}}
	m := newModel(t, p, &fake{id: "nodes", title: "Nodes"})

	// Input mode (the shell opens here): the letter reaches the pane.
	got, _ := m.Update(key("h"))
	m = got.(Model)
	if m.showHelp {
		t.Fatal("h opened help while in Input mode — the prompt would lose the character")
	}
	if len(p.gotKeys) == 0 || p.gotKeys[len(p.gotKeys)-1] != "h" {
		t.Fatalf("the pane did not receive h; saw %v", p.gotKeys)
	}

	// esc to Navigate: the same letter now acts globally.
	got, _ = m.Update(key("esc"))
	m = got.(Model)
	if m.mode != keymap.Navigate {
		t.Fatalf("esc did not leave Input mode (mode=%v)", m.mode)
	}
	got, _ = m.Update(key("h"))
	m = got.(Model)
	if !m.showHelp {
		t.Error("h did not open help after esc")
	}
}

// i returns to Input, and the pane is told so it can render the difference.
func TestIReturnsToInputModeAndFocusesThePane(t *testing.T) {
	p := &textPane{fake: fake{id: "copilot", title: "Copilot"}}
	m := newModel(t, p, &fake{id: "nodes", title: "Nodes"})

	got, _ := m.Update(key("esc"))
	m = got.(Model)
	if p.focused {
		t.Error("the pane still reports focus after esc")
	}
	got, _ = m.Update(key("i"))
	m = got.(Model)
	if m.mode != keymap.Input {
		t.Fatalf("i did not enter Input mode (mode=%v)", m.mode)
	}
	if !p.focused {
		t.Error("the pane was not refocused, so its prompt would still say 'press i to type'")
	}
}

// THE MODE MUST BE VISIBLE. A shell where one key does two things has to say
// which posture it is in, or it has swapped one unlearnable rule for another.
func TestTheStatusBarNamesTheMode(t *testing.T) {
	m := newModel(t, &textPane{fake: fake{id: "copilot", title: "Copilot"}})
	if !strings.Contains(m.status(), "INPUT") {
		t.Errorf("status = %q, want it to name INPUT mode", m.status())
	}
	got, _ := m.Update(key("esc"))
	m = got.(Model)
	if !strings.Contains(m.status(), "NAV") {
		t.Errorf("status = %q, want it to name NAV mode", m.status())
	}
}
