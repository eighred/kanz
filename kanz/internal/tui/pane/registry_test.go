package pane

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// A SIBLING OF THE RUNNING BINARY IS FOUND AND RUNS.
//
// This is the case that was broken on Windows: a developer running ./kanz from
// the build output has kanz-monitor right beside it and nothing on PATH, and
// exec.Command with a bare name refused it (exec.ErrDot).
//
// Proven by actually EXECUTING it, not by inspecting a path — the report was a
// launch failure, so an assertion that stops short of launching would not have
// caught it.
func TestToolCommandFindsAndRunsASiblingOfTheRunningBinary(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary: %v", err)
	}
	dir := filepath.Dir(self)

	// Build a tiny tool INTO the test binary's own directory, which is what
	// resolveTool searches first.
	name := "kanz-tooltest"
	src := filepath.Join(t.TempDir(), "main.go")
	if werr := os.WriteFile(src, []byte(
		"package main\nimport \"fmt\"\nfunc main(){ fmt.Print(\"ran\") }\n"), 0o600); werr != nil {
		t.Fatalf("write source: %v", werr)
	}
	out := filepath.Join(dir, name)
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	build := exec.Command("go", "build", "-o", out, src)
	if bout, berr := build.CombinedOutput(); berr != nil {
		t.Skipf("cannot build the probe tool (no toolchain here?): %v: %s", berr, bout)
	}
	defer func() { _ = os.Remove(out) }()

	cmd := ToolCommand(name)
	if cmd.Err != nil {
		t.Fatalf("ToolCommand(%q) failed to resolve a sibling of the running binary: %v", name, cmd.Err)
	}
	if !filepath.IsAbs(cmd.Path) {
		t.Errorf("resolved to a relative path %q — this is exactly what Go refuses to run", cmd.Path)
	}
	// Run it. Stdout is redirected because ToolCommand wires the real terminal.
	cmd.Stdout, cmd.Stderr, cmd.Stdin = nil, nil, nil
	got, rerr := cmd.Output()
	if rerr != nil {
		t.Fatalf("running the resolved sibling failed: %v", rerr)
	}
	if string(got) != "ran" {
		t.Errorf("sibling produced %q, want %q", got, "ran")
	}
}

// A TOOL THAT IS NOWHERE MUST FAIL WITH A MESSAGE THAT SAYS WHERE IT LOOKED.
//
// The command is still fully formed — argv and stdio — because the halt pane
// renders its own argv, and a bare {Path, Err} made Args()[1:] a panic waiting
// for the first machine without the tool installed.
func TestToolCommandCarriesAUsableErrorWhenTheToolIsMissing(t *testing.T) {
	cmd := ToolCommand("kanz-definitely-not-installed", "--by", "operator:akif")

	if cmd.Err == nil {
		t.Fatal("a missing tool produced no error — the shell would report a clean, instant exit")
	}
	msg := cmd.Err.Error()
	for _, want := range []string{"kanz-definitely-not-installed", "PATH"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q — the operator cannot tell where it looked", msg, want)
		}
	}
	if len(cmd.Args) != 3 {
		t.Errorf("Args = %v, want the argv preserved so callers can still render it", cmd.Args)
	}
	if cmd.Stdin == nil || cmd.Stdout == nil || cmd.Stderr == nil {
		t.Error("stdio left unset on the error command — callers see a different shape depending on " +
			"whether the tool happened to be installed")
	}
	// Run must surface the error rather than doing anything.
	if err := cmd.Run(); err == nil {
		t.Error("Run() on an unresolvable command returned nil")
	}
}
