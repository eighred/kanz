package main

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// nodeRow is one row of the Nodes pane — every field is a display string
// (Age is computed in the poller off the clock, keeping render pure).
type nodeRow struct {
	Name, Status, Roles, Region, Version, Age string
}

// clusterRow is one row of the Clusters pane.
type clusterRow struct {
	Region          string
	Online, Offline int
}

// pane selects which read-only view is shown.
type pane int

const (
	paneNodes pane = iota
	paneClusters
)

// model is the whole UI state, mutated ONLY by Update in response to messages —
// never by the poller goroutine directly (Bubble Tea's concurrency contract).
type model struct {
	cfg    Config
	src    nodeSource
	active pane

	nodes    []nodeRow
	clusters []clusterRow

	width, height int
	err           error
}

func newModel(cfg Config, src nodeSource) model {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 3 * time.Second
	}
	return model{cfg: cfg, src: src, active: paneNodes}
}

func (m model) Init() tea.Cmd { return m.pollTick() }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "tab":
			if m.active == paneNodes {
				m.active = paneClusters
			} else {
				m.active = paneNodes
			}
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case fetchMsg:
		// A failed fetch degrades the tick (err shown in the status line),
		// leaving the prior nodes/clusters intact — never a crash, never a
		// blank screen on one bad poll.
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.nodes = msg.nodes
			m.clusters = msg.clusters
			m.err = nil
		}
		return m, m.pollTick()
	}
	return m, nil
}

func (m model) View() string { return m.render() }
