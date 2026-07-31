package pane

import (
	"os/exec"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// fake is a Gateway pane for tests.
type fake struct {
	id    ID
	title string
	plane Plane
}

func (f fake) ID() ID                         { return f.id }
func (f fake) Title() string                  { return f.title }
func (f fake) Plane() Plane                   { return f.plane }
func (f fake) Init() tea.Cmd                  { return nil }
func (f fake) Update(tea.Msg) (Pane, tea.Cmd) { return f, nil }
func (f fake) View(int, int) string           { return f.title }

// busNoRunner is the shape the registry must refuse: it claims the bus plane but
// supplies no command, so the shell would have nothing to launch.
type busNoRunner struct{ fake }

func (b busNoRunner) Plane() Plane { return Bus }

func TestRegistryKeepsTabOrder(t *testing.T) {
	r, err := NewRegistry(
		fake{id: "a", title: "A"},
		fake{id: "b", title: "B"},
		fake{id: "c", title: "C"},
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	// Order is what an operator's fingers learn. Built from a map it would vary
	// between runs, which is the failure this asserts against.
	got := r.IDs()
	want := []ID{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("IDs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("IDs()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if r.IndexOf("b") != 1 {
		t.Errorf("IndexOf(b) = %d, want 1", r.IndexOf("b"))
	}
	if r.IndexOf("missing") != -1 {
		t.Errorf("IndexOf(missing) = %d, want -1", r.IndexOf("missing"))
	}
}

// A BUS PANE THAT CANNOT SAY WHAT TO RUN MUST BE REFUSED AT WIRING TIME.
//
// The alternative is discovering it on the first keypress, at which point the
// only options are to do nothing (a dead tab) or to fall back to running the
// tool in-process — which is precisely the identity collapse the plane split
// exists to prevent.
func TestRegistryRefusesABusPaneWithNoCommand(t *testing.T) {
	_, err := NewRegistry(busNoRunner{fake{id: "halt", title: "Halt"}})
	if err == nil {
		t.Fatal("NewRegistry accepted a Bus pane with no Runner — the shell would have no way to launch it")
	}
	if !strings.Contains(err.Error(), "Runner") {
		t.Errorf("error = %q, want it to name Runner so the fix is obvious", err)
	}
}

func TestRegistryRefusesDuplicateAndEmptyIDs(t *testing.T) {
	if _, err := NewRegistry(fake{id: "a", title: "A"}, fake{id: "a", title: "A2"}); err == nil {
		t.Error("NewRegistry accepted a duplicate id — ids address panes, so two panes cannot share one")
	}
	if _, err := NewRegistry(fake{id: "", title: "A"}); err == nil {
		t.Error("NewRegistry accepted an empty id — the pane could not be addressed or bound to a key")
	}
	if _, err := NewRegistry(); err == nil {
		t.Error("NewRegistry accepted an empty registry — a shell with no panes has nothing to route to")
	}
}

// Replace stores what a pane's Update returned. It must refuse a pane whose id
// changed, which would silently detach that tab from its key binding.
func TestRegistryReplaceRejectsAnIDChange(t *testing.T) {
	r, err := NewRegistry(fake{id: "a", title: "A"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if !r.Replace(0, fake{id: "a", title: "A renamed"}) {
		t.Error("Replace rejected a same-id pane")
	}
	if r.Replace(0, fake{id: "different", title: "X"}) {
		t.Error("Replace accepted a pane whose id changed — the tab would detach from its binding")
	}
	if r.Replace(9, fake{id: "a"}) {
		t.Error("Replace accepted an out-of-range index")
	}
}

// EVERY BUS PANE MUST BUILD A FRESH COMMAND. An exec.Cmd cannot be reused —
// Run on a started command returns "exec: already started" — so a stored one
// would launch correctly the first time and fail for the rest of the session.
func TestExecPaneBuildsAFreshCommandEachTime(t *testing.T) {
	p := NewExecPane("monitor", "Monitor", "kanz-monitor")

	first, second := p.Command(), p.Command()
	if first == nil || second == nil {
		t.Fatal("Command() returned nil")
	}
	if first == second {
		t.Error("Command() returned the same *exec.Cmd twice — the second launch would fail with " +
			"\"exec: already started\"")
	}
	if p.Plane() != Bus {
		t.Errorf("Plane() = %v, want Bus", p.Plane())
	}
	var _ Runner = p // must satisfy Runner, or the registry rejects it
	if p.Binary() != "kanz-monitor" {
		t.Errorf("Binary() = %q, want kanz-monitor", p.Binary())
	}
}

// The child is interactive, so it must get the real terminal rather than pipes.
func TestExecPaneInheritsTheTerminal(t *testing.T) {
	c := NewExecPane("halt", "Halt", "kanz-halt").Command()
	if c.Stdin == nil || c.Stdout == nil || c.Stderr == nil {
		t.Fatal("Command() left stdio unset — a piped kill switch cannot prompt, and a piped monitor " +
			"cannot paint")
	}
	var _ *exec.Cmd = c
}
