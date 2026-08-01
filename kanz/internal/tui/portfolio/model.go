package portfolio

import (
	"context"
	"errors"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/eighred/kanz/internal/tui/pane"
)

// PaneID addresses this pane in the shell's registry and key bindings.
const PaneID pane.ID = "book"

// Config is what the pane needs beyond the shared gateway client.
type Config struct {
	// Account is the broker account whose positions are shown.
	Account string
	// Portfolio is the portfolio whose risk measures are shown. It is a SEPARATE
	// id from Account on purpose: the broker projection is keyed by account and
	// the risk engine by portfolio, and assuming they are the same string is the
	// kind of guess that silently shows one fund's risk beside another's book.
	Portfolio string
	// PollInterval is how often the two surfaces are re-read.
	PollInterval time.Duration
	// CallTimeout bounds ONE fetch. The shared client carries no timeout of its
	// own, so this is the only thing that stops an unresponsive gateway from
	// hanging the pane.
	CallTimeout time.Duration
}

// withDefaults fills the intervals but NOT the ids. An empty Account or
// Portfolio is a wiring mistake and is surfaced as one — defaulting them would
// silently read somebody else's book.
func (c Config) withDefaults() Config {
	if c.PollInterval <= 0 {
		c.PollInterval = 5 * time.Second
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = 10 * time.Second
	}
	return c
}

// fetchMsg carries one poll's result. The two halves are INDEPENDENT: each
// carries its own error, so a risk engine that is down leaves the positions
// on screen with a note beside the risk table, rather than blanking both.
type fetchMsg struct {
	positions []Position
	posErr    error
	measures  []Measure
	riskErr   error
	at        time.Time
}

// Model is the positions/PnL/risk pane.
type Model struct {
	cfg Config
	// build defers source construction until the pane is first used, and is
	// retried after a failure. THE SHELL OPENS BEFORE THE OPERATOR HAS SIGNED IN
	// (the token arrives on the Copilot pane's /login), so a source built at
	// startup would either refuse an empty token and take the whole shell down
	// with it, or capture a token that a mid-session re-login then invalidates.
	// This is the same lazy-connect stance the estate panes take, and for the same
	// reason: signing in on one pane and tabbing to another must work without a
	// restart.
	build func() (*Source, error)
	src   *Source

	positions []Position
	measures  []Measure
	posErr    error
	riskErr   error
	lastAt    time.Time
	loaded    bool
}

// New builds the pane. build is called at most once per successful construction.
func New(build func() (*Source, error), cfg Config) Model {
	return Model{cfg: cfg.withDefaults(), build: build}
}

func (m Model) ID() pane.ID       { return PaneID }
func (m Model) Title() string     { return "Book" }
func (m Model) Plane() pane.Plane { return pane.Gateway }

// startMsg asks Update to build the source and fetch.
//
// Init cannot return a Model, so construction cannot happen there — it would
// build a source nothing could store. Bouncing through a message puts it in
// Update, which can.
type startMsg struct{}

// Init fetches immediately rather than waiting a full interval, so the pane does
// not open empty for five seconds and read as "no positions".
func (m Model) Init() tea.Cmd {
	return func() tea.Msg { return startMsg{} }
}

func (m Model) Update(msg tea.Msg) (pane.Pane, tea.Cmd) {
	switch msg := msg.(type) {
	case startMsg:
		return m.start()
	case fetchMsg:
		m.positions, m.posErr = msg.positions, msg.posErr
		m.measures, m.riskErr = msg.measures, msg.riskErr
		m.lastAt, m.loaded = msg.at, true
		return m, m.pollTick()
	case tea.KeyMsg:
		// r re-reads on demand. An operator watching a book during an incident
		// should not have to wait out a poll interval to see whether something
		// changed. It also retries construction, so it doubles as "I have signed
		// in now, try again".
		if msg.String() == "r" {
			return m.start()
		}
	}
	return m, nil
}

// start ensures the source exists and issues an immediate fetch.
//
// Construction happens HERE, on the UI goroutine, rather than inside the fetch
// command: Model is a value type (bubbletea's convention), so a goroutine that
// built the source could not store it back on the model anyone reads.
func (m Model) start() (pane.Pane, tea.Cmd) {
	if m.src == nil {
		s, err := m.build()
		if err != nil {
			// Rendered in the pane rather than returned. A shell that refuses to
			// start because one pane cannot reach its backend is a worse tool than
			// one that shows which pane is unreachable and why.
			m.posErr, m.riskErr = err, err
			m.loaded, m.lastAt = true, time.Now()
			return m, m.pollTick() // retry on the next tick
		}
		m.src = s
	}
	return m, m.fetchNow()
}

// pollTick arms one tick; Update re-arms after each result, so the pane polls at
// a fixed cadence without stacking timers.
func (m Model) pollTick() tea.Cmd {
	return tea.Tick(m.cfg.PollInterval, func(time.Time) tea.Msg { return m.fetch() })
}

func (m Model) fetchNow() tea.Cmd {
	return func() tea.Msg { return m.fetch() }
}

// fetch reads both surfaces under one deadline and keeps their failures apart.
func (m Model) fetch() tea.Msg {
	if m.src == nil {
		// The build failed and this is the retry tick; report it as both halves
		// failing, because neither surface is reachable without a source.
		err := errNotConnected
		return fetchMsg{posErr: err, riskErr: err, at: time.Now()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.CallTimeout)
	defer cancel()

	out := fetchMsg{at: time.Now()}
	out.positions, out.posErr = m.src.Positions(ctx, m.cfg.Account)
	out.measures, out.riskErr = m.src.Measures(ctx, m.cfg.Portfolio)
	return out
}

// errNotConnected is what the pane shows before a token exists. It names the
// remedy, because "not connected" on its own sends an operator to check the
// network for a missing sign-in.
var errNotConnected = errors.New("not connected to the gateway yet — sign in on the Copilot pane " +
	"with /login, then press r here")
