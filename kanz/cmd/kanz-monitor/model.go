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
	SPIFFESocket string        // SPIFFE Workload API socket for mesh mTLS; read only when !Plaintext
}

// maxEvents caps the rolling lifecycle feed so a long monitor session cannot
// grow m.events unbounded. The oldest entries are dropped, newest last.
const maxEvents = 200

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

	// busCh is the channel the bus reader's background goroutine (busreader.go)
	// writes tea.Msg values onto. It is a reference type, so every copy of
	// model Update hands back shares the same channel — the goroutine started
	// from Init's startBusReader call is the only writer; Update is the only
	// reader, via the looping waitForBusMsg tea.Cmd.
	busCh chan tea.Msg
}

func newModel(cfg Config) model {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	return model{
		cfg:    cfg,
		book:   map[string]position{},
		health: map[string]bool{},
		busCh:  make(chan tea.Msg, 64),
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
	UnverifiedVenueAccount                                                int
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.startBusReader(), m.pollTick())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height

	case busEventMsg:
		// Append, capped at maxEvents with the oldest dropped — a rolling feed,
		// newest last.
		m.events = append(m.events, lifecycleEvent(msg))
		if len(m.events) > maxEvents {
			m.events = m.events[len(m.events)-maxEvents:]
		}
		return m, waitForBusMsg(m.busCh)

	case busPositionMsg:
		// Upsert, last-write-wins — matches the compacted POSITION stream's own
		// semantics: the latest message per key IS the current state.
		key := msg.Portfolio + "/" + msg.Instrument
		m.book[key] = position(msg)
		return m, waitForBusMsg(m.busCh)

	case busErrMsg:
		// Non-fatal: recorded for the status line, never a crash. The reader
		// goroutine keeps running (or has already returned after this one
		// terminal error); either way Update must keep draining the channel.
		m.err = msg.err
		return m, waitForBusMsg(m.busCh)

	case pollMsg:
		// Full replace: parseCounters always returns every field (0 for a
		// counter absent from the scrape), so the snapshot is always
		// complete — never a partial merge over the previous tick.
		m.counters = msg.counters
		// health is merged, not replaced: a poll cycle that failed the
		// /metrics leg but succeeded /readyz (or vice versa) must not erase
		// the service statuses the OTHER leg just reported.
		for svc, ok := range msg.health {
			m.health[svc] = ok
		}
		if msg.err != nil {
			m.err = msg.err
		}
		// Re-arm: every pollMsg fires the next tick, so the ticker never
		// stops for the life of the program.
		return m, m.pollTick()
	}
	return m, nil
}

func (m model) View() string {
	// Task 5 replaces this with the real layout. A minimal non-panicking stub so
	// Bubble Tea's pre-data View call is safe.
	return "kanz-monitor — connecting…  (q to quit)\n"
}
