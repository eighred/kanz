package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// counterRow builds the exact substring renderCounters emits for one row
// (label + its formatted value), using the identical format string
// ("%-20s %d") that view.go's renderCounters uses. Asserting on this
// label-and-value substring — rather than a bare digit — is what makes the
// counter tests isolating: a bare "2" or "3" can coincidentally match order
// IDs (ord-2) or book quantities (1.23456789) regardless of whether the
// counter itself was computed correctly.
func counterRow(label string, value int) string {
	return fmt.Sprintf("%-20s %d", label, value)
}

func TestRenderPopulatedModel(t *testing.T) {
	m := model{
		cfg: Config{
			Tenant:     "acme",
			NATSURL:    "nats://localhost:4222",
			GatewayURL: "http://localhost:8080",
		},
		events: []lifecycleEvent{
			{
				At:      time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC),
				Type:    eventTypeRouted,
				OrderID: "ord-routed-1",
				Detail:  "routed to SIM (venue order v-1)",
			},
			{
				At:      time.Date(2026, 7, 23, 10, 0, 1, 0, time.UTC),
				Type:    eventTypeFilled,
				OrderID: "ord-filled-1",
				Detail:  "10 @ 100.50 (venue SIM), filled 10/10",
			},
			{
				At:      time.Date(2026, 7, 23, 10, 0, 2, 0, time.UTC),
				Type:    eventTypeRejected,
				OrderID: "ord-rejected-1",
				Detail:  "ERR_X: bad thing happened over and over in exhaustive verbose venue-facing detail",
			},
		},
		book: map[string]position{
			"acme/BTC-USD": {
				Portfolio:  "acme",
				Instrument: "BTC-USD",
				Quantity:   "1.23456789",
				AvgPrice:   "65000.125",
			},
		},
		counters: counterSnapshot{
			Quarantined:            3,
			Ungoverned:             0,
			Unpriced:               0,
			SharedCollateral:       0,
			UnverifiedVenueAccount: 0,
		},
		health: map[string]bool{
			"gateway": true,
		},
		width:  100,
		height: 30,
	}

	out := m.render()

	if !strings.Contains(out, "ord-filled-1") {
		t.Errorf("render() missing order id ord-filled-1:\n%s", out)
	}
	if !strings.Contains(out, "BTC-USD") {
		t.Errorf("render() missing instrument BTC-USD:\n%s", out)
	}
	// Assert the formatted Quarantined row (label+value), not a bare "3" —
	// the book's Quantity fixture "1.23456789" also contains a "3" and would
	// make a bare-digit assertion pass regardless of whether the counter
	// tile rendered correctly.
	if !strings.Contains(out, counterRow("Quarantined", 3)) {
		t.Errorf("render() missing quarantined counter row %q:\n%s", counterRow("Quarantined", 3), out)
	}
	if !strings.Contains(out, "✓") { // ✓
		t.Errorf("render() missing a ✓ health mark:\n%s", out)
	}

	for i, line := range strings.Split(out, "\n") {
		if w := lipgloss.Width(line); w > m.width {
			t.Errorf("line %d width %d exceeds m.width %d: %q", i, w, m.width, line)
		}
	}
}

func TestRenderDerivesFilledAndRejectedFromEvents(t *testing.T) {
	m := model{
		cfg: Config{Tenant: "acme"},
		events: []lifecycleEvent{
			{Type: eventTypeFilled, OrderID: "ord-1"},
			{Type: eventTypeFilled, OrderID: "ord-2"},
			{Type: eventTypeRejected, OrderID: "ord-3"},
			{Type: eventTypeRouted, OrderID: "ord-4"},
		},
		book: map[string]position{},
		// Deliberately DIFFERENT from the event-derived truth (2 filled, 1
		// rejected): if renderCounters ever regresses to reading these
		// fields instead of deriving from m.events, the rendered tile will
		// show 99/88 and the assertions below will fail.
		counters: counterSnapshot{Filled: 99, Rejected: 88},
		health:   map[string]bool{},
		width:    80,
		height:   24,
	}

	out := m.render()

	// Assert the formatted counter row (label+value) rather than a bare
	// digit — this test's own order IDs (ord-1, ord-2, ...) contain the
	// digits "1" and "2" and would satisfy a bare strings.Contains check
	// regardless of whether the counters were derived correctly.
	wantFilled := counterRow("Filled", 2)
	wantRejected := counterRow("Rejected", 1)
	if !strings.Contains(out, wantFilled) {
		t.Errorf("render() missing derived filled row %q:\n%s", wantFilled, out)
	}
	if !strings.Contains(out, wantRejected) {
		t.Errorf("render() missing derived rejected row %q:\n%s", wantRejected, out)
	}

	// And the regression form must NOT appear: if renderCounters read
	// m.counters.Filled/.Rejected directly, these rows would show 99/88.
	regressedFilled := counterRow("Filled", 99)
	regressedRejected := counterRow("Rejected", 88)
	if strings.Contains(out, regressedFilled) {
		t.Errorf("render() shows m.counters.Filled (99) instead of the event-derived count:\n%s", out)
	}
	if strings.Contains(out, regressedRejected) {
		t.Errorf("render() shows m.counters.Rejected (88) instead of the event-derived count:\n%s", out)
	}
}

func TestRenderNarrowTerminalNoOverflow(t *testing.T) {
	m := model{
		cfg: Config{Tenant: "acme"},
		events: []lifecycleEvent{
			{
				At:      time.Now(),
				Type:    eventTypeFilled,
				OrderID: "a-very-long-order-id-that-could-overflow-a-narrow-terminal-0001",
				Detail:  "10 @ 100.50 (venue SIM), filled 10/10, this detail is also quite long indeed",
			},
		},
		book: map[string]position{
			"acme/SOME-VERY-LONG-INSTRUMENT-NAME": {
				Portfolio:  "acme",
				Instrument: "SOME-VERY-LONG-INSTRUMENT-NAME",
				Quantity:   "1.234567891234567",
				AvgPrice:   "65000.123456789",
			},
		},
		counters: counterSnapshot{Quarantined: 1},
		health:   map[string]bool{"gateway": false},
		width:    40,
		height:   20,
	}

	out := m.render()
	for i, line := range strings.Split(out, "\n") {
		if w := lipgloss.Width(line); w > m.width {
			t.Errorf("line %d width %d exceeds m.width %d: %q", i, w, m.width, line)
		}
	}
}

func TestRenderEmptyHealthIsNotAFailure(t *testing.T) {
	m := model{
		cfg:      Config{Tenant: "acme"},
		events:   nil,
		book:     map[string]position{},
		counters: counterSnapshot{},
		health:   map[string]bool{},
		width:    80,
		height:   24,
	}

	out := m.render()
	if strings.Contains(out, "✗") { // ✗ — no key should ever be hardcoded/shown
		t.Errorf("render() shows a ✗ for a service that was never polled:\n%s", out)
	}
}
