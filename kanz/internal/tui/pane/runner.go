package pane

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"

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
func (p *ExecPane) Command() *exec.Cmd { return ToolCommand(p.bin, p.args...) }

// Binary is the command this pane launches. Exported for tests and for the arch
// guard that asserts bus tools are reached by exec rather than imported.
func (p *ExecPane) Binary() string { return p.bin }

// ToolCommand builds the command for a sibling tool, resolved SAFELY.
//
// THE BUG THIS EXISTS FOR, reported from a Windows TTY pass:
//
//	monitor (kanz-monitor.exe): exec: "kanz-monitor":
//	cannot run executable found relative to current directory
//
// Since Go 1.19 (CVE-2022-30580) exec.LookPath will not resolve a bare name
// through a RELATIVE PATH entry — including "." — and returns exec.ErrDot
// instead of running it. That is correct and must not be worked around: a
// binary named kanz-halt dropped into whatever directory the operator happened
// to `cd` into would otherwise take over the platform kill switch.
//
// So resolution is explicit, in this order:
//
//  1. NEXT TO THE RUNNING kanz BINARY. The tools ship together, and that
//     directory is exactly as trustworthy as the shell already executing from
//     it. This is the case that was broken — a developer running ./kanz from the
//     build output has the siblings right there and nothing on PATH.
//  2. An ABSOLUTE hit on PATH, for a deliberate install.
//  3. Nothing. The working directory is never used.
//
// A failure is carried on the returned command's Err rather than reported here,
// because Runner.Command has no error to return: exec.Cmd.Run surfaces Err
// immediately, so it reaches the shell's status bar through the same
// ExecFinished path a real launch failure does.
func ToolCommand(name string, args ...string) *exec.Cmd {
	path, err := resolveTool(name)
	if err != nil {
		// FULLY FORMED EVEN THOUGH IT WILL NEVER RUN. Args and stdio are set so a
		// caller inspecting the command — the halt pane renders its own argv —
		// sees the same shape whether resolution succeeded or not. An error
		// command that was a bare {Path, Err} made Args()[1:] a panic waiting for
		// the first machine without the tool installed.
		c := &exec.Cmd{Path: name, Args: append([]string{name}, args...), Err: err}
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		return c
	}
	c := exec.Command(path, args...) //nolint:gosec // path is resolved above, never from user input
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c
}

// resolveTool locates a sibling tool. See ToolCommand for the ordering and why
// the working directory is excluded.
func resolveTool(name string) (string, error) {
	exeName := name
	if runtime.GOOS == "windows" {
		exeName += ".exe"
	}

	selfDir := ""
	if self, err := os.Executable(); err == nil {
		selfDir = filepath.Dir(self)
		candidate := filepath.Join(selfDir, exeName)
		if fi, statErr := os.Stat(candidate); statErr == nil && !fi.IsDir() {
			return candidate, nil
		}
	}

	found, err := exec.LookPath(name)
	switch {
	case errors.Is(err, exec.ErrDot):
		// LookPath found it, but only by way of a relative PATH entry. Refusing
		// is the whole point — see ToolCommand.
		return "", fmt.Errorf("%s was found only in the current directory, which is refused for "+
			"safety (Go's exec.ErrDot): install it on PATH, or put it beside the kanz binary in %s",
			name, quoteDir(selfDir))
	case err != nil:
		return "", fmt.Errorf("%s not found: it is not beside the kanz binary in %s and not on PATH. "+
			"The estate tools ship together — `make build` puts them in the same directory",
			name, quoteDir(selfDir))
	case !filepath.IsAbs(found):
		return "", fmt.Errorf("%s resolved to the relative path %q, which is refused for safety: "+
			"install it on PATH, or put it beside the kanz binary in %s", name, found, quoteDir(selfDir))
	}
	return found, nil
}

func quoteDir(dir string) string {
	if dir == "" {
		return "the kanz binary's directory"
	}
	return strconv.Quote(dir)
}
