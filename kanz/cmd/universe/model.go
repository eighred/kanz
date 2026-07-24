package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
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

	// showForm/form drive the Add Node form (S2a). provisions is the last
	// polled provisioning strip; formErr surfaces a failed submit without
	// leaving the form (the operator stays reachable to retry).
	showForm   bool
	form       addForm
	provisions []provisionRow
	formErr    error

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
		if m.showForm {
			return m.updateForm(msg)
		}
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "tab":
			if m.active == paneNodes {
				m.active = paneClusters
			} else {
				m.active = paneNodes
			}
		case "a":
			if m.active == paneNodes {
				m.showForm = true
				m.form = newAddForm()
				m.formErr = nil
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
			m.provisions = msg.provisions
			m.err = nil
		}
		return m, m.pollTick()
	case addNodeResultMsg:
		if msg.err != nil {
			m.formErr = msg.err
		} else {
			m.showForm = false
			m.formErr = nil
		}
	}
	return m, nil
}

// updateForm handles key input while the Add Node form is active. It never
// touches m.nodes/m.clusters — only the form's own state and, on submit, a
// tea.Cmd that reads the key file and calls addNode off the UI thread.
func (m model) updateForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.showForm = false
		m.formErr = nil
		return m, nil
	case tea.KeyTab:
		m.form = m.form.next()
		return m, nil
	case tea.KeyShiftTab:
		m.form = m.form.prev()
		return m, nil
	case tea.KeyBackspace:
		m.form = m.form.backspace()
		return m, nil
	case tea.KeyEnter:
		return m, m.submitAddForm()
	case tea.KeyRunes:
		m.form = m.form.key(msg)
		return m, nil
	}
	return m, nil
}

// submitAddForm reads the key file named in the form and fires AddNode off the
// UI thread, returning an addNodeResultMsg. The key bytes never touch the model.
func (m model) submitAddForm() tea.Cmd {
	in := addNodeInput{
		hostname: m.form.value("hostname"),
		ip:       m.form.value("ip"),
		sshUser:  m.form.value("ssh_user"),
		sshPort:  atoi32(m.form.value("ssh_port")),
	}
	keyPath := m.form.value("key_path")
	src := m.src
	return func() tea.Msg {
		key, err := os.ReadFile(keyPath)
		if err != nil {
			return addNodeResultMsg{err: fmt.Errorf("read key %s: %w", keyPath, err)}
		}
		in.sshKey = key
		ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
		defer cancel()
		id, err := src.addNode(ctx, in)
		return addNodeResultMsg{id: id, err: err}
	}
}

// addNodeResultMsg carries the outcome of an AddNode call back into Update.
type addNodeResultMsg struct {
	id  string
	err error
}

// atoi32 parses an int32 from a form field; a malformed SSH port falls back to
// 22 rather than failing the whole submit on a stray keystroke.
func atoi32(s string) int32 {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 22
	}
	return int32(n)
}

func (m model) View() string { return m.render() }
