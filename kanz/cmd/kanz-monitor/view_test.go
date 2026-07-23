package main

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
)

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
	if !strings.Contains(out, "3") {
		t.Errorf("render() missing quarantined count 3:\n%s", out)
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
		book:     map[string]position{},
		counters: counterSnapshot{}, // Filled/Rejected stay 0 here — must be derived
		health:   map[string]bool{},
		width:    80,
		height:   24,
	}

	out := m.render()

	if !strings.Contains(out, "2") {
		t.Errorf("render() missing derived filled count 2:\n%s", out)
	}
	if !strings.Contains(out, "1") {
		t.Errorf("render() missing derived rejected count 1:\n%s", out)
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
