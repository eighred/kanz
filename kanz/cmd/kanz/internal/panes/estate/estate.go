// Package estate mounts the universe operator screens as kanz shell panes (#66).
//
// Universe standalone cycles its three screens with `tab`. Inside the shell that
// key is already global — it moves between the shell's panes — so universe's
// internal cycling would never see it and two of its three screens would be
// unreachable, which is exactly what #66 asks to fix. Mounting each screen as
// its own top-level tab resolves that instead of remapping around it, and avoids
// a second tab bar nested inside the first.
//
// ONE MODEL, THREE PANES. The screens share a single universe.Model through
// *Shared. Giving each pane its own model would run three pollers against the
// api-gateway for one operator looking at one estate, and the three would
// disagree with each other between ticks — an operator cordoning a node on the
// Nodes tab would not see it reflected on Clusters.
package estate

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/eighred/kanz/internal/tui/pane"
	"github.com/eighred/kanz/internal/tui/theme"
	"github.com/eighred/kanz/internal/tui/ui"
	"github.com/eighred/kanz/internal/tui/universe"
)

// Shared is the one universe.Model the estate panes render.
//
// universe.Model is a value type with value receivers (bubbletea's convention),
// so the shared mutable cell lives here rather than in each pane. Only Apply
// writes it, and only from the shell's Update goroutine — the same single-writer
// contract universe's own poller already respects.
//
// BUILT LAZILY, ON FIRST VISIT. universe.NewGatewaySource REFUSES an empty token
// ("the gateway authenticates a PERSON — this tool holds no authority of its
// own"), and the kanz shell opens before the operator has signed in. Building
// eagerly would mean a signed-out operator cannot open kanz at all — losing the
// Copilot pane, which is where /login lives. So the estate connects when it is
// first opened, with whatever token exists then, and says so if there is none.
type Shared struct {
	build func() (universe.Model, error)

	m     universe.Model
	ready bool
	// err is rendered in the pane rather than returned. A shell that refuses to
	// start because one pane cannot reach its backend is a worse tool than one
	// that shows which pane is unreachable and why.
	err error
}

// NewShared defers construction until a pane is first opened. build is called at
// most once per successful construction; a failure is retried on the next visit,
// so signing in on the Copilot pane and tabbing back here works without a
// restart.
func NewShared(build func() (universe.Model, error)) *Shared {
	return &Shared{build: build}
}

// Model returns the current model value. Only meaningful once ready.
func (s *Shared) Model() universe.Model { return s.m }

// Apply stores what universe's Update returned.
func (s *Shared) Apply(m universe.Model) { s.m = m }

// ensure builds the model if it is not built yet, returning the Init command for
// the poller on the transition. Retries after a failure — see NewShared.
func (s *Shared) ensure() tea.Cmd {
	if s.ready || s.build == nil {
		return nil
	}
	m, err := s.build()
	if err != nil {
		s.err = err
		return nil
	}
	s.m, s.ready, s.err = m, true, nil
	return s.m.Init()
}

// Err is the last construction failure, or nil.
func (s *Shared) Err() error { return s.err }

// Ready reports whether the estate model has been built.
func (s *Shared) Ready() bool { return s.ready }

// Pane is one universe screen presented as a shell pane.
type Pane struct {
	shared *Shared
	screen universe.Screen
	id     pane.ID
	title  string
}

// New mounts screen as a shell pane.
func New(shared *Shared, screen universe.Screen, id pane.ID, title string) *Pane {
	return &Pane{shared: shared, screen: screen, id: id, title: title}
}

// All mounts every universe screen, in the order an operator works through them:
// the estate first, then what it is made of, then the credentials that reach it.
//
// Returned as a slice so a screen added to universe cannot be silently left
// unmounted — the wiring names them all in one place, and a new one is a
// compile-visible addition here rather than a tab nobody notices is missing.
func All(shared *Shared) []pane.Pane {
	return []pane.Pane{
		New(shared, universe.ScreenNodes, "nodes", "Nodes"),
		New(shared, universe.ScreenClusters, "clusters", "Clusters"),
		New(shared, universe.ScreenAPI, "venue-keys", "Venue Keys"),
	}
}

func (p *Pane) ID() pane.ID   { return p.id }
func (p *Pane) Title() string { return p.title }

// Plane is Gateway: universe reaches the estate through the api-gateway with the
// operator's own bearer token and holds no cluster access of any kind (OPS-M2c).
// It carries no SPIFFE identity, so it is safe in this process — unlike the bus
// tools, which is the whole distinction pane.Plane exists to draw.
func (p *Pane) Plane() pane.Plane { return pane.Gateway }

// Init connects the estate on first visit and starts its poller once for all
// three panes. Three Init calls would otherwise run three poll loops against the
// gateway for one operator looking at one estate.
func (p *Pane) Init() tea.Cmd { return p.shared.ensure() }

// Update selects this pane's screen, then delegates.
//
// The screen is set on EVERY message, not just on entry. The three panes share
// one model, so whichever pane is active must assert its screen before universe
// renders or acts — otherwise a keypress on Clusters would be interpreted
// against whatever screen the last pane left selected.
func (p *Pane) Update(msg tea.Msg) (pane.Pane, tea.Cmd) {
	if !p.shared.Ready() {
		// Nothing to drive yet. Messages are dropped rather than queued: they are
		// keystrokes and poll ticks for a session that does not exist.
		return p, nil
	}
	m := p.shared.Model().WithScreen(p.screen)
	updated, cmd := m.Update(msg)
	um, ok := updated.(universe.Model)
	if !ok {
		// universe.Model.Update returns tea.Model. If it ever returns something
		// else the shared cell must not be poisoned with it.
		return p, cmd
	}
	p.shared.Apply(um)
	return p, cmd
}

// View renders universe's frame for this screen.
//
// universe renders at its own natural size and does not take width/height —
// clipping to the shell's body budget is ui.Frame's job, and duplicating it here
// would give two answers to one question.
func (p *Pane) View(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	// SAY WHY IT IS EMPTY. An unreachable estate rendering a blank pane is
	// indistinguishable from a hung one, and the fix (sign in on Copilot) is not
	// guessable from a blank screen.
	if err := p.shared.Err(); err != nil {
		return strings.Join([]string{
			theme.Error.Render("  the estate is unreachable"),
			"",
			"  " + ui.Clip(err.Error(), w-2),
			"",
			"  " + theme.StatusBar.Render("sign in on the Copilot pane, then return here."),
		}, "\n")
	}
	if !p.shared.Ready() {
		return theme.StatusBar.Render("  connecting to the estate…")
	}
	return p.shared.Model().WithScreen(p.screen).View()
}

// Screen is which universe screen this pane shows. Exported for the arch guard
// that asserts every screen is mounted.
func (p *Pane) Screen() universe.Screen { return p.screen }

// String aids test failure messages.
func (p *Pane) String() string { return fmt.Sprintf("estate.Pane(%s/%d)", p.id, p.screen) }
