package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// nodeRow is one row of the Nodes pane — every display field is a string
// (Age is computed in the poller off the clock, keeping render pure).
// schedulable/evictablePods are the raw signals nodeStateLabel derives the
// status column from, and that the drain confirm prompt reports pod counts from.
type nodeRow struct {
	Name, Status, Roles, Region, Version, Age string
	schedulable                               bool
	evictablePods                             int
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

	// selected is the highlighted row in the Nodes pane; c/u/d act on
	// nodes[selected]. confirmingDrain gates the destructive drain action
	// behind an explicit y/n; actionErr surfaces a failed cordon/uncordon/drain
	// call without disturbing nodes/clusters (the next poll reflects reality).
	selected        int
	confirmingDrain bool
	actionErr       error

	// movingRegion/moveInput drive the Move Node (region relabel) prompt (S3b).
	// Unlike drain it fires directly on enter with no y/n confirm — a relabel
	// is non-destructive.
	movingRegion bool
	moveInput    string

	// showForm/form drive the Add Node form (S2a). provisions is the last
	// polled provisioning strip; formErr surfaces a failed submit without
	// leaving the form (the operator stays reachable to retry).
	showForm   bool
	form       addForm
	provisions []provisionRow
	formErr    error
	testResult string

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
		if m.active == paneNodes && m.confirmingDrain {
			return m.updateDrainConfirm(msg)
		}
		if m.active == paneNodes && m.movingRegion {
			return m.updateMoveInput(msg)
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
				m.testResult = ""
			}
		case "up":
			if m.active == paneNodes && m.selected > 0 {
				m.selected--
			}
		case "down":
			if m.active == paneNodes && m.selected < len(m.nodes)-1 {
				m.selected++
			}
		case "c":
			if m.active == paneNodes {
				if name, ok := m.selectedNodeName(); ok {
					return m, m.nodeActionCmd(m.src.cordon, name)
				}
			}
		case "u":
			if m.active == paneNodes {
				if name, ok := m.selectedNodeName(); ok {
					return m, m.nodeActionCmd(m.src.uncordon, name)
				}
			}
		case "d":
			if m.active == paneNodes {
				if _, ok := m.selectedNodeName(); ok {
					m.confirmingDrain = true
				}
			}
		case "m":
			if m.active == paneNodes {
				if _, ok := m.selectedNodeName(); ok {
					m.movingRegion = true
					m.moveInput = ""
				}
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
			m.selected = clampSelected(m.selected, len(m.nodes))
		}
		return m, m.pollTick()
	case nodeActionMsg:
		m.actionErr = msg.err
		// The node's new status arrives on the next poll — no optimistic
		// mutation of m.nodes here, so a failed action never lies about state.
		return m, nil
	case addNodeResultMsg:
		if msg.err != nil {
			m.formErr = msg.err
		} else {
			m.showForm = false
			m.formErr = nil
		}
	case testConnResultMsg:
		switch {
		case msg.err != nil:
			m.testResult = "✗ test failed: " + msg.err.Error()
		case msg.res.reachable:
			m.testResult = fmt.Sprintf("✓ reachable (%dms)", msg.res.latencyMs)
		default:
			m.testResult = "✗ unreachable: " + msg.res.message
		}
		return m, nil
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
	case tea.KeyCtrlT:
		return m, m.testConnCmd()
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

// testConnCmd probes the form's ip:port off the UI thread.
func (m model) testConnCmd() tea.Cmd {
	ip := m.form.value("ip")
	port := atoi32(m.form.value("ssh_port"))
	src := m.src
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
		defer cancel()
		res, err := src.testConnection(ctx, ip, port)
		return testConnResultMsg{res: res, err: err}
	}
}

// testConnResultMsg carries the outcome of a testConnection probe back into Update.
type testConnResultMsg struct {
	res testConnResult
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

// updateDrainConfirm handles key input while the drain confirm prompt is
// showing. Any key other than y/n/esc is swallowed — the prompt blocks all
// other nodes-pane input until answered.
func (m model) updateDrainConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y":
		m.confirmingDrain = false
		if name, ok := m.selectedNodeName(); ok {
			return m, m.nodeActionCmd(m.src.drain, name)
		}
		return m, nil
	case "n", "esc":
		m.confirmingDrain = false
		return m, nil
	}
	return m, nil
}

// updateMoveInput handles key input while the Move Node (region relabel)
// prompt is showing. Unlike drain, enter fires setRegion directly with no
// y/n confirm — a relabel is non-destructive. An empty moveInput is rejected
// client-side on enter (stays in the input) rather than round-tripping a
// value the handler would reject anyway. Any other key is swallowed.
func (m model) updateMoveInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.movingRegion = false
		m.moveInput = ""
		return m, nil
	case tea.KeyEnter:
		if m.moveInput == "" {
			return m, nil
		}
		if name, ok := m.selectedNodeName(); ok {
			region := m.moveInput
			m.movingRegion = false
			m.moveInput = ""
			return m, m.moveNodeCmd(name, region)
		}
		return m, nil
	case tea.KeyBackspace:
		if r := []rune(m.moveInput); len(r) > 0 {
			m.moveInput = string(r[:len(r)-1])
		}
		return m, nil
	case tea.KeyRunes:
		m.moveInput += string(msg.Runes)
		return m, nil
	}
	return m, nil
}

// moveNodeCmd runs a setRegion call off the UI thread and reports the outcome
// as a nodeActionMsg — the same result message cordon/drain use, so a failed
// relabel surfaces via actionErr with no new plumbing. The node's new Region
// arrives on the next poll rather than being applied optimistically here.
func (m model) moveNodeCmd(name, region string) tea.Cmd {
	src := m.src
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
		defer cancel()
		return nodeActionMsg{err: src.setRegion(ctx, name, region)}
	}
}

// selectedNodeName returns the name of the highlighted node, guarding against
// an empty (or since-shrunk) nodes slice so an action key is a safe no-op
// rather than an index panic.
func (m model) selectedNodeName() (string, bool) {
	if m.selected < 0 || m.selected >= len(m.nodes) {
		return "", false
	}
	return m.nodes[m.selected].Name, true
}

// clampSelected keeps selected in [0, n-1] (or 0 when n==0) after a fetch
// replaces m.nodes — a poll that shrinks the estate must never leave a stale
// index pointing past the end of the new slice.
func clampSelected(selected, n int) int {
	if n == 0 {
		return 0
	}
	if selected >= n {
		return n - 1
	}
	if selected < 0 {
		return 0
	}
	return selected
}

// nodeActionCmd runs a cordon/uncordon/drain call off the UI thread and
// reports the outcome as a nodeActionMsg; the node's new status arrives on
// the next poll rather than being applied optimistically here.
func (m model) nodeActionCmd(action func(context.Context, string) error, name string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
		defer cancel()
		return nodeActionMsg{err: action(ctx, name)}
	}
}

// nodeActionMsg carries the outcome of a cordon/uncordon/drain call back into Update.
type nodeActionMsg struct{ err error }

// nodeStateLabel derives the Nodes-pane status column from the raw
// schedulable/evictablePods/Status signals: a cordoned node still running pods
// reads as actively Draining; once its pods are gone it reads as Drained. A
// schedulable node falls through to its actual readiness (Status) — a
// schedulable-but-NotReady/Unknown node (e.g. a healthy uncordoned node whose
// kubelet died) must never render as "Ready".
func nodeStateLabel(n nodeRow) string {
	if !n.schedulable {
		if n.evictablePods > 0 {
			return fmt.Sprintf("Draining (%d)", n.evictablePods)
		}
		return "Drained"
	}
	return n.Status // "Ready" / "NotReady" / "Unknown" — readiness for a schedulable node
}

func (m model) View() string { return m.render() }
