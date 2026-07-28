package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// venueRow is one row of the API Manager pane — presence only, never key
// material.
type venueRow struct {
	venue      string
	configured bool
}

// pane selects which read-only view is shown.
type pane int

const (
	paneNodes pane = iota
	paneClusters
	paneAPI
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

	// probing is true from the Test Connection keypress until its result arrives.
	// It exists because the probe stopped being an in-process dial and became a
	// Kubernetes Job, so the form now sits for seconds with nothing to show. It does
	// two jobs: the render draws it where the result will appear, so the TUI is
	// visibly working rather than apparently hung, and updateForm refuses a second
	// probe while it is set — otherwise each keypress of an operator who thinks the
	// TUI is wedged spawns another Job holding :22 egress. It deliberately survives
	// closing and reopening the form, because the Job does too: clearing it on open
	// would hand back exactly the second Job it exists to prevent.
	probing bool

	// venues is the last polled API-Manager presence strip; apiSelected is the
	// highlighted row in the API Manager pane.
	venues      []venueRow
	apiSelected int

	// showKeyForm/keyForm drive the Set API Keys form (S4a write side), scoped
	// to the venue selected in the API Manager pane when 'k' is pressed.
	// keyFormErr surfaces the sanitized rejection reason when the exchange
	// declines the submitted credentials (FailedPrecondition, S4b's pre-write
	// proof): the form is reopened with every field cleared and keyFormErr set,
	// so the operator sees why without the typed secret ever surviving on the
	// model. Any other failed submit surfaces via actionErr instead (see
	// keyFormResultMsg), which leaves keyFormErr nil.
	showKeyForm bool
	keyForm     keyForm
	keyFormErr  error

	// verifiedAccounts records, per venue, the exchange_account_id returned by
	// the last proved SetVenueKeys submit (S4b's pre-write proof) — session
	// state only, never sourced from ListVenueKeys (presence only). Only a
	// non-empty exchange_account_id is ever recorded here: an unproven
	// deployment (empty id) must not start claiming verification.
	//
	// This is a submit OUTCOME, not a standing claim about current state: the
	// keys backing an entry can be overwritten later by a path that did no
	// proof at all (proof disabled, another operator session, a direct store
	// write), and ListVenueKeys can never tell the difference. So every
	// fetchMsg poll refresh wipes the map — the badge reads "verified" only
	// until the next poll, then falls back to the value-blind "configured"
	// wording until another submit proves it again.
	verifiedAccounts map[string]string

	width, height int
	err           error
}

func newModel(cfg Config, src nodeSource) model {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 3 * time.Second
	}
	// Normalized HERE rather than at each call site: a zero would make every context
	// deadline already expired, so every call would fail instantly with a deadline error
	// that looks like an unreachable gateway.
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = defaultCallTimeout
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
		if m.showKeyForm {
			return m.updateKeyForm(msg)
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
			switch m.active {
			case paneNodes:
				m.active = paneClusters
			case paneClusters:
				m.active = paneAPI
			default:
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
			if m.active == paneAPI && m.apiSelected > 0 {
				m.apiSelected--
			}
		case "down":
			if m.active == paneNodes && m.selected < len(m.nodes)-1 {
				m.selected++
			}
			if m.active == paneAPI && m.apiSelected < len(m.venues)-1 {
				m.apiSelected++
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
		case "k":
			if m.active == paneAPI {
				if m.apiSelected >= 0 && m.apiSelected < len(m.venues) {
					m.showKeyForm = true
					m.keyForm = newKeyForm(m.venues[m.apiSelected].venue)
					m.keyFormErr = nil
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
			m.venues = msg.venues
			m.err = nil
			m.selected = clampSelected(m.selected, len(m.nodes))
			m.apiSelected = clampSelected(m.apiSelected, len(m.venues))
			// A poll refresh invalidates every proved badge: ListVenueKeys is
			// value-blind and cannot confirm the keys it just reported present
			// are the same ones a prior submit proved. Wiping the whole map
			// makes "verified" mean "the last submit was proved," never a
			// standing claim about what the store holds right now.
			m.verifiedAccounts = nil
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
	case keyFormResultMsg:
		if msg.err != nil && status.Code(msg.err) == codes.FailedPrecondition {
			// The exchange rejected these credentials (S4b's pre-write proof).
			// The form stays OPEN so the operator can retype, but every field
			// is cleared first — a rejected key set is exactly the one that
			// must not be retained. The server's message is already sanitized
			// (never key material, never the exchange's own words), so it is
			// shown verbatim via keyFormErr.
			m.keyForm = newKeyForm(m.keyForm.venue)
			m.keyFormErr = errors.New(status.Convert(msg.err).Message())
			return m, nil
		}
		// Any other result closes the form — the typed secret must never
		// linger on the model. A failed submit surfaces through actionErr (the
		// same status-line error cordon/uncordon/drain/setRegion use) rather
		// than keeping the form open with the secret still resident, so the
		// operator can see what failed and reopen with 'k' to retry.
		m.showKeyForm = false
		m.keyForm = keyForm{}
		m.keyFormErr = nil
		m.actionErr = msg.err
		if msg.err == nil && msg.accountID != "" {
			m.verifiedAccounts = withVerifiedAccount(m.verifiedAccounts, msg.venue, msg.accountID)
		}
		return m, nil
	case testConnResultMsg:
		// Cleared before the switch so EVERY outcome — including a failed probe —
		// releases the in-flight lock. Clearing it per-branch is how a form ends up
		// permanently refusing to probe again after one error.
		m.probing = false
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
		return m.submitAddForm()
	case tea.KeyCtrlT:
		// One probe at a time. A probe costs a Job in the operator's namespace, so a
		// repeated keypress must be ignored rather than queued behind the first.
		if m.probing {
			return m, nil
		}
		m.probing = true
		m.testResult = "" // the previous verdict is not this probe's answer
		return m, m.testConnCmd()
	case tea.KeyRunes:
		m.form = m.form.key(msg)
		return m, nil
	}
	return m, nil
}

// submitAddForm validates the form, then reads the key file it names and fires
// AddNode off the UI thread, returning an addNodeResultMsg. The key bytes never
// touch the model.
//
// It returns a model as well as a Cmd because validation has to happen on the UI
// thread: a tea.Cmd can only speak by returning a message, so it cannot set
// formErr, and routing a rejection through a message would mean issuing the
// request the rejection exists to prevent. An empty required field therefore
// returns no command at all — no RPC, and no key-file read either. That ordering
// is the point: the read used to come first, so a blank Key Path surfaced as
// "read key : no such file or directory", an error about a path the operator
// never typed rather than a field they can fix.
func (m model) submitAddForm() (model, tea.Cmd) {
	if missing := m.form.missingRequired(); len(missing) > 0 {
		// EVERY empty field is named, not just the first. The whole value of
		// validating here is that the operator sees what is wrong without a round
		// trip; reporting one field at a time would just move the round trip
		// in-process, one submit per blank field. Listing them costs a Join.
		verb := "is"
		if len(missing) > 1 {
			verb = "are"
		}
		m.formErr = fmt.Errorf("%s %s required", strings.Join(missing, ", "), verb)
		// showForm stays true: the operator fixes the named field in place with
		// everything else still typed, rather than reopening and retyping.
		return m, nil
	}
	// A prior rejection is not this submit's verdict — clear it, or a form that was
	// fixed and resubmitted keeps showing the error it was fixed for.
	m.formErr = nil

	port, err := parsePort(m.form.value("ssh_port"))
	if err != nil {
		// Same shape as the missingRequired rejection above, and for the same
		// reason: no key-file read, no RPC, form stays open on the row the
		// operator has to fix. A bad port is caught here rather than sent, so it
		// can never reach the estate as a truncated one (see parsePort).
		m.formErr = err
		return m, nil
	}

	in := addNodeInput{
		hostname: m.form.value("hostname"),
		ip:       m.form.value("ip"),
		sshUser:  m.form.value("ssh_user"),
		sshPort:  port,
	}
	keyPath := m.form.value("key_path")
	src := m.src
	timeout := m.cfg.CallTimeout
	return m, func() tea.Msg {
		key, err := os.ReadFile(keyPath)
		if err != nil {
			return addNodeResultMsg{err: fmt.Errorf("read key %s: %w", keyPath, err)}
		}
		in.sshKey = key
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
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

// updateKeyForm handles key input while the Set API Keys form is active. It
// never touches m.venues — only the form's own state and, on submit, a
// tea.Cmd that calls setVenueKeys off the UI thread.
func (m model) updateKeyForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.showKeyForm = false
		m.keyForm = keyForm{}
		m.keyFormErr = nil
		return m, nil
	case tea.KeyTab:
		m.keyForm = m.keyForm.next()
		return m, nil
	case tea.KeyShiftTab:
		m.keyForm = m.keyForm.prev()
		return m, nil
	case tea.KeyBackspace:
		m.keyForm = m.keyForm.backspace()
		return m, nil
	case tea.KeyEnter:
		return m, m.submitKeyForm()
	case tea.KeyRunes:
		m.keyForm = m.keyForm.key(msg)
		return m, nil
	}
	return m, nil
}

// submitKeyForm fires SetVenueKeys off the UI thread, then the typed secret leaves the
// model — the form is reset regardless of outcome-in-flight, and the result closes it.
func (m model) submitKeyForm() tea.Cmd {
	venue := m.keyForm.venue
	keys := venueKeys{
		apiKey:     m.keyForm.value("api_key"),
		apiSecret:  m.keyForm.value("api_secret"),
		passphrase: m.keyForm.value("passphrase"),
	}
	src := m.src
	timeout := m.cfg.CallTimeout
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		accountID, err := src.setVenueKeys(ctx, venue, keys)
		return keyFormResultMsg{venue: venue, accountID: accountID, err: err}
	}
}

// keyFormResultMsg carries the outcome of a setVenueKeys call back into
// Update — venue identifies which pane row a proved accountID belongs to.
type keyFormResultMsg struct {
	venue     string
	accountID string
	err       error
}

// withVerifiedAccount returns a copy of accounts with venue set to id. Model
// fields are never mutated in place — Update's value receiver copies the map
// header, not its contents, so mutating a shared map here would leak a write
// across model copies (e.g. into the pre-Update model a caller still holds).
func withVerifiedAccount(accounts map[string]string, venue, id string) map[string]string {
	out := make(map[string]string, len(accounts)+1)
	for k, v := range accounts {
		out[k] = v
	}
	out[venue] = id
	return out
}

// testConnCmd probes the form's ip:port off the UI thread. It is the ONLY call site
// of testConnTimeout: every other command here is a fast read on Config.CallTimeout,
// while this one waits on the operator running an ephemeral probe Job.
//
// THE BOUND IS max(testConnTimeout, configured), never the configured value alone.
// --timeout is sized for the reads, so honouring it here would let an operator who
// tightened it put the TUI's bound back underneath the operator's 60s probe wait and
// re-break the deadline chain from the outside — every probe failing client-side with a
// generic deadline, including against a healthy host. Raising --timeout past 100s is
// respected, because that only ever gives an inner layer more room to answer.
func (m model) testConnCmd() tea.Cmd {
	ip := m.form.value("ip")
	port, err := parsePort(m.form.value("ssh_port"))
	if err != nil {
		// The probe is refused without spawning the Job, but it still ANSWERS with a
		// testConnResultMsg: updateForm sets probing before calling this, and only
		// that message clears it. Returning nil here would leave the form stuck
		// in-flight, refusing every further ctrl+t for the rest of the session.
		return func() tea.Msg { return testConnResultMsg{err: err} }
	}
	src := m.src
	timeout := testConnTimeout
	if m.cfg.CallTimeout > timeout {
		timeout = m.cfg.CallTimeout
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
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

// defaultSSHPort is what an omitted SSH Port field means. The field is prefilled
// with it and 22 is the only port this estate probes or provisions over, so a
// cleared field is an omission rather than a choice (see newAddForm).
const defaultSSHPort int32 = 22

// parsePort reads the SSH Port field. An empty or whitespace-only field is the
// documented omission and yields 22; anything else must be a real port, or the
// action that called this is refused with an error naming the field.
//
// It parses with ParseInt(_, 10, 32) rather than Atoi followed by an int32 cast
// because Atoi returns a platform-width int, and on a 64-bit build that cast
// TRUNCATES instead of failing: "4294967318" (2³²+22) parses cleanly and arrives
// at addNodeInput.sshPort / TestConnectionRequest.SshPort as 22, and "2147483670"
// as -2147483626. Neither is anything the operator sees.
//
// The truncation to a still-valid port is the dangerous one, and it is why this
// is not a cosmetic parse: Test Connection would report "✓ reachable" for a port
// nobody typed, and the AddNode that follows would provision a trading node over
// that same unintended port — so the estate's record of how the box is reached is
// wrong from the moment it joins, and it looks confirmed. ParseInt refuses both
// values at the boundary instead.
//
// The 1..65535 check is here rather than left to the server for the reason the
// required-field check is: a port no kernel can dial is worth saying next to the
// field, not after a round trip.
func parsePort(s string) (int32, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return defaultSSHPort, nil
	}
	n, err := strconv.ParseInt(t, 10, 32)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("SSH Port %q is not a port number (1-65535)", s)
	}
	return int32(n), nil
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
	timeout := m.cfg.CallTimeout
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
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
	timeout := m.cfg.CallTimeout
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
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
