package main

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Config is the monitor's read configuration. Every field is a READ coordinate —
// there is deliberately no producer, no signing key beyond a bearer token, and no
// write path anywhere in this struct. A monitor cannot move capital.
type Config struct {
	NATSURL      string        // the spine to subscribe to (read-only broadcast)
	GatewayURL   string        // api-gateway base URL for /v1 read routes + /metrics
	Token        string        // HS256 bearer for the gateway (kanz-devtoken locally)
	Tenant       string        // the tenant whose book to show; poller scopes to it
	PollInterval time.Duration // how often the poller scrapes; <=0 ⇒ 2s
	Plaintext    bool          // true ⇒ dial NATS without TLS (dev rig); false ⇒ mesh mTLS
}

// model is the whole UI state. It is mutated ONLY by Update, only in response to
// messages from the bus reader and the poller — never by those goroutines directly,
// which is Bubble Tea's concurrency contract and the reason the UI needs no locks.
type model struct {
	cfg Config

	// events is the rolling order-lifecycle feed (newest last), capped so a long
	// session cannot grow unbounded.
	events []lifecycleEvent

	// book is the current position per instrument, folded from the compacted
	// POSITION stream. Keyed "portfolio/instrument".
	book map[string]position

	// counters and health are the latest poll snapshot.
	counters counterSnapshot
	health   map[string]bool // service name → ready

	width, height int
	err           error // last non-fatal error, shown in a status line
}

func newModel(cfg Config) model {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	return model{
		cfg:    cfg,
		book:   map[string]position{},
		health: map[string]bool{},
	}
}

// The message and row types the later tasks fill in. Declared here so the model
// compiles and View can range over empty slices/maps without a nil panic.
type lifecycleEvent struct {
	At      time.Time
	Type    string // the order.order.* event_type, trimmed for display
	OrderID string
	Detail  string // venue, price×qty, reason — whatever the payload carries
}

type position struct {
	Portfolio, Instrument string
	Quantity, AvgPrice    string // exact RatStrings off the wire; never float
}

type counterSnapshot struct {
	Filled, Rejected, Quarantined, Ungoverned, Unpriced, SharedCollateral int
}

func (m model) Init() tea.Cmd { return nil } // Task 3/4 return the real startup cmds

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	}
	return m, nil
}

func (m model) View() string {
	// Task 5 replaces this with the real layout. A minimal non-panicking stub so
	// Bubble Tea's pre-data View call is safe.
	return "kanz-monitor — connecting…  (q to quit)\n"
}
