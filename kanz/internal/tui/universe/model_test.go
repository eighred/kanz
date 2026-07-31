package universe

import (
	"context"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// setRegionCall records one setRegion invocation; stubSource holds a pointer
// to a slice so calls survive the stub's value-receiver methods.
type setRegionCall struct{ name, region string }

// setVenueKeysCall records one setVenueKeys invocation.
type setVenueKeysCall struct {
	venue string
	keys  venueKeys
}

type stubSource struct {
	msg              fetchMsg
	setRegionCalls   *[]setRegionCall
	setVenueKeysCall *[]setVenueKeysCall
	venues           []venueRow

	// setVenueKeysAccountID/setVenueKeysErr let a test script the outcome of
	// the next setVenueKeys call — a proved submit, a FailedPrecondition
	// rejection, or any other error.
	setVenueKeysAccountID string
	setVenueKeysErr       error

	// budgets records the deadline each call arrived with, keyed by call name. The
	// context is the only observable the TUI has for WHICH bound a command applied,
	// and the two bounds must not be interchangeable. Nil records nothing.
	budgets *map[string]time.Duration
}

// record stores the remaining budget on ctx, rounded to whole seconds: the deadline is
// set microseconds before the call sees it, and the assertion is about which bound was
// chosen, not about scheduling noise.
func (s stubSource) record(call string, ctx context.Context) {
	if s.budgets == nil {
		return
	}
	var d time.Duration
	if dl, ok := ctx.Deadline(); ok {
		d = time.Until(dl).Round(time.Second)
	}
	(*s.budgets)[call] = d
}

func (s stubSource) fetch(context.Context) (fetchMsg, error) { return s.msg, nil }

func (s stubSource) addNode(context.Context, addNodeInput) (string, error)  { return "p-1", nil }
func (s stubSource) listProvisions(context.Context) ([]provisionRow, error) { return nil, nil }

func (s stubSource) testConnection(ctx context.Context, _ string, _ int32) (testConnResult, error) {
	s.record("testConnection", ctx)
	return testConnResult{reachable: true, latencyMs: 7}, nil
}

func (s stubSource) cordon(ctx context.Context, _ string) error {
	s.record("cordon", ctx)
	return nil
}

func (s stubSource) uncordon(context.Context, string) error { return nil }
func (s stubSource) drain(context.Context, string) error    { return nil }

func (s stubSource) setRegion(_ context.Context, name, region string) error {
	if s.setRegionCalls != nil {
		*s.setRegionCalls = append(*s.setRegionCalls, setRegionCall{name: name, region: region})
	}
	return nil
}

func (s stubSource) setVenueKeys(_ context.Context, venue string, keys venueKeys) (string, error) {
	if s.setVenueKeysCall != nil {
		*s.setVenueKeysCall = append(*s.setVenueKeysCall, setVenueKeysCall{venue: venue, keys: keys})
	}
	return s.setVenueKeysAccountID, s.setVenueKeysErr
}

func (s stubSource) listVenueKeys(context.Context) ([]venueRow, error) { return s.venues, nil }

// --timeout is sized for the ordinary reads, and it must be UNABLE to reach into Test
// Connection. Test Connection waits on the operator running an ephemeral probe Job
// (60s of its own), so a 1s bound applied there would cancel a healthy probe and report
// a client-side deadline — the exact defect the deadline-nesting work removed, arriving
// this time through a flag rather than a constant. The ordinary actions, meanwhile, must
// honour the flag or it does nothing at all.
func TestTighteningTheCallTimeoutCannotShortenTestConnection(t *testing.T) {
	budgets := map[string]time.Duration{}
	m := NewModel(Config{CallTimeout: time.Second}, stubSource{budgets: &budgets})

	m.testConnCmd()()
	m.nodeActionCmd(m.src.cordon, "london")()

	if got := budgets["testConnection"]; got != testConnTimeout {
		t.Errorf("Test Connection ran with a %s budget, want %s: --timeout must not be able to "+
			"lower it beneath the operator's own probe wait", got, testConnTimeout)
	}
	if got := budgets["cordon"]; got != time.Second {
		t.Errorf("cordon ran with a %s budget, want the configured 1s — an ordinary action that "+
			"ignores --timeout leaves the flag doing nothing", got)
	}
}

// A zero CallTimeout must NOT become an already-expired deadline: every call would then
// fail instantly with a deadline error indistinguishable from an unreachable gateway.
func TestZeroCallTimeoutFallsBackToTheDefault(t *testing.T) {
	budgets := map[string]time.Duration{}
	m := NewModel(Config{}, stubSource{budgets: &budgets})

	m.nodeActionCmd(m.src.cordon, "london")()

	if got := budgets["cordon"]; got != DefaultCallTimeout {
		t.Errorf("an unset CallTimeout produced a %s budget, want %s", got, DefaultCallTimeout)
	}
}

func TestFetchMsgPopulatesModel(t *testing.T) {
	m := NewModel(Config{}, stubSource{})
	updated, _ := m.Update(fetchMsg{
		nodes:    []nodeRow{{Name: "london", Status: "Ready", Region: "europe"}},
		clusters: []clusterRow{{Region: "europe", Online: 1}},
	})
	got := updated.(Model)
	if len(got.nodes) != 1 || got.nodes[0].Name != "london" {
		t.Fatalf("nodes = %+v", got.nodes)
	}
	if len(got.clusters) != 1 || got.clusters[0].Region != "europe" {
		t.Fatalf("clusters = %+v", got.clusters)
	}
}

func TestTabSwitchesPane(t *testing.T) {
	m := NewModel(Config{}, stubSource{})
	if m.active != ScreenNodes {
		t.Fatalf("initial pane = %v, want ScreenNodes", m.active)
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if updated.(Model).active != ScreenClusters {
		t.Fatalf("after tab, pane = %v, want ScreenClusters", updated.(Model).active)
	}
}

func TestTabCyclesThroughAPIPaneAndBack(t *testing.T) {
	m := NewModel(Config{}, stubSource{})
	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = u.(Model)
	if m.active != ScreenClusters {
		t.Fatalf("after 1 tab, pane = %v, want ScreenClusters", m.active)
	}
	u, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = u.(Model)
	if m.active != ScreenAPI {
		t.Fatalf("after 2 tabs, pane = %v, want ScreenAPI", m.active)
	}
	u, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = u.(Model)
	if m.active != ScreenNodes {
		t.Fatalf("after 3 tabs, pane = %v, want ScreenNodes (cycle back)", m.active)
	}
}

func TestFetchMsgPopulatesVenuesAndClampsAPISelected(t *testing.T) {
	m := NewModel(Config{}, stubSource{})
	m.apiSelected = 5
	updated, _ := m.Update(fetchMsg{
		venues: []venueRow{{venue: "binance", configured: true}, {venue: "coinbase", configured: false}},
	})
	got := updated.(Model)
	if len(got.venues) != 2 || got.venues[0].venue != "binance" {
		t.Fatalf("venues = %+v", got.venues)
	}
	if got.apiSelected != 1 {
		t.Fatalf("apiSelected = %d, want clamped to 1", got.apiSelected)
	}
}

func TestAPIPaneSelectionMovesWithinBounds(t *testing.T) {
	m := NewModel(Config{}, stubSource{})
	m.active = ScreenAPI
	m.venues = []venueRow{{venue: "binance"}, {venue: "coinbase"}}

	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if u.(Model).apiSelected != 1 {
		t.Fatalf("down should select row 1, got %d", u.(Model).apiSelected)
	}
	u, _ = u.(Model).Update(tea.KeyMsg{Type: tea.KeyDown})
	if u.(Model).apiSelected != 1 {
		t.Fatalf("down at last row should stay at 1, got %d", u.(Model).apiSelected)
	}
	u, _ = u.(Model).Update(tea.KeyMsg{Type: tea.KeyUp})
	if u.(Model).apiSelected != 0 {
		t.Fatalf("up should select row 0, got %d", u.(Model).apiSelected)
	}
	u, _ = u.(Model).Update(tea.KeyMsg{Type: tea.KeyUp})
	if u.(Model).apiSelected != 0 {
		t.Fatalf("up at first row should stay at 0, got %d", u.(Model).apiSelected)
	}
}

func TestAPIPaneSelectionNoOpWithNoVenues(t *testing.T) {
	m := NewModel(Config{}, stubSource{})
	m.active = ScreenAPI
	m.venues = nil
	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if cmd != nil {
		t.Fatalf("down with no venues should not return a command")
	}
	if u.(Model).apiSelected != 0 {
		t.Fatalf("apiSelected should stay 0 with no venues, got %d", u.(Model).apiSelected)
	}
}

func TestFetchErrorGoesToStatusNotCrash(t *testing.T) {
	m := NewModel(Config{}, stubSource{})
	updated, _ := m.Update(fetchMsg{err: context.DeadlineExceeded})
	if updated.(Model).err == nil {
		t.Fatalf("expected err recorded on Model")
	}
}

func TestQuitKey(t *testing.T) {
	m := NewModel(Config{}, stubSource{})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatalf("expected tea.Quit command on q")
	}
}

func TestNodeSelectionMovesAndActionsFire(t *testing.T) {
	m := NewModel(Config{}, stubSource{})
	m.active = ScreenNodes
	m.nodes = []nodeRow{{Name: "a", schedulable: true}, {Name: "b", schedulable: true}}
	// down moves selection
	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if u.(Model).selected != 1 {
		t.Fatalf("down should select row 1, got %d", u.(Model).selected)
	}
	// 'c' on the selected node returns a command
	u2, cmd := u.(Model).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	if cmd == nil {
		t.Fatalf("c (cordon) should return a command")
	}
	_ = u2
}

func TestDrainAsksForConfirmation(t *testing.T) {
	m := NewModel(Config{}, stubSource{})
	m.active = ScreenNodes
	m.nodes = []nodeRow{{Name: "a", schedulable: true, evictablePods: 3}}
	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	if cmd != nil {
		t.Fatalf("d should NOT fire drain immediately — it opens a confirm")
	}
	if !u.(Model).confirmingDrain {
		t.Fatalf("d should enter the confirm state")
	}
	// 'y' confirms and fires
	_, cmd2 := u.(Model).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if cmd2 == nil {
		t.Fatalf("y should fire the drain command")
	}
}

func TestMKeyOpensRegionInput(t *testing.T) {
	m := NewModel(Config{}, stubSource{})
	m.active = ScreenNodes
	m.nodes = []nodeRow{{Name: "london", schedulable: true}}
	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	if cmd != nil {
		t.Fatalf("m should open the region input, not fire immediately")
	}
	got := u.(Model)
	if !got.movingRegion {
		t.Fatalf("m should enter the move-input state")
	}
	if got.moveInput != "" {
		t.Fatalf("moveInput should start empty, got %q", got.moveInput)
	}
}

func TestMKeyWithNoNodesIsNoOp(t *testing.T) {
	m := NewModel(Config{}, stubSource{})
	m.active = ScreenNodes
	m.nodes = nil
	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	if cmd != nil {
		t.Fatalf("m with no nodes should not return a command")
	}
	if u.(Model).movingRegion {
		t.Fatalf("m with no nodes should not enter the move-input state")
	}
}

func TestRegionInputTypesAndBackspaces(t *testing.T) {
	m := NewModel(Config{}, stubSource{})
	m.active = ScreenNodes
	m.nodes = []nodeRow{{Name: "london", schedulable: true}}
	m.movingRegion = true
	for _, r := range "asia" {
		u, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = u.(Model)
	}
	if m.moveInput != "asia" {
		t.Fatalf("move input = %q, want asia", m.moveInput)
	}
	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m = u.(Model)
	if m.moveInput != "asi" {
		t.Fatalf("backspace should trim one rune, got %q", m.moveInput)
	}
}

func TestRegionInputEnterSubmitsAndRecordsCall(t *testing.T) {
	var calls []setRegionCall
	m := NewModel(Config{}, stubSource{setRegionCalls: &calls})
	m.active = ScreenNodes
	m.nodes = []nodeRow{{Name: "london", schedulable: true}}
	m.movingRegion = true
	m.moveInput = "asia"

	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatalf("enter should fire the SetNodeRegion command")
	}
	if u.(Model).movingRegion {
		t.Fatalf("enter should close the move-input state")
	}
	msg := cmd()
	am, ok := msg.(nodeActionMsg)
	if !ok {
		t.Fatalf("expected nodeActionMsg, got %T", msg)
	}
	if am.err != nil {
		t.Fatalf("unexpected err: %v", am.err)
	}
	if len(calls) != 1 || calls[0].name != "london" || calls[0].region != "asia" {
		t.Fatalf("setRegion calls = %+v", calls)
	}
}

func TestRegionInputEnterOnEmptyDoesNothing(t *testing.T) {
	var calls []setRegionCall
	m := NewModel(Config{}, stubSource{setRegionCalls: &calls})
	m.active = ScreenNodes
	m.nodes = []nodeRow{{Name: "london", schedulable: true}}
	m.movingRegion = true
	m.moveInput = ""

	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatalf("enter on empty moveInput should not fire a command")
	}
	if !u.(Model).movingRegion {
		t.Fatalf("enter on empty moveInput should stay in the move-input state")
	}
	if len(calls) != 0 {
		t.Fatalf("setRegion should not have been called, got %+v", calls)
	}
}

func TestRegionInputEscCancels(t *testing.T) {
	var calls []setRegionCall
	m := NewModel(Config{}, stubSource{setRegionCalls: &calls})
	m.active = ScreenNodes
	m.nodes = []nodeRow{{Name: "london", schedulable: true}}
	m.movingRegion = true
	m.moveInput = "asia"
	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil {
		t.Fatalf("esc should not return a command")
	}
	got := u.(Model)
	if got.movingRegion {
		t.Fatalf("esc should cancel the move input")
	}
	if got.moveInput != "" {
		t.Fatalf("esc should reset moveInput, got %q", got.moveInput)
	}
	if len(calls) != 0 {
		t.Fatalf("setRegion should not have been called, got %+v", calls)
	}
}

func TestKKeyOpensKeyFormScopedToSelectedVenue(t *testing.T) {
	m := NewModel(Config{}, stubSource{})
	m.active = ScreenAPI
	m.venues = []venueRow{{venue: "binance"}, {venue: "okx"}}
	m.apiSelected = 1

	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if cmd != nil {
		t.Fatalf("k should open the form, not fire immediately")
	}
	got := u.(Model)
	if !got.showKeyForm {
		t.Fatalf("k should enter the key-form state")
	}
	if got.keyForm.venue != "okx" {
		t.Fatalf("keyForm.venue = %q, want okx (scoped to selected row)", got.keyForm.venue)
	}
}

func TestKKeyWithNoVenuesIsNoOp(t *testing.T) {
	m := NewModel(Config{}, stubSource{})
	m.active = ScreenAPI
	m.venues = nil
	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if cmd != nil {
		t.Fatalf("k with no venues should not return a command")
	}
	if u.(Model).showKeyForm {
		t.Fatalf("k with no venues should not enter the key-form state")
	}
}

func TestKeyFormTypingAndBackspaceEditFocusedField(t *testing.T) {
	m := NewModel(Config{}, stubSource{})
	m.showKeyForm = true
	m.keyForm = newKeyForm("binance")

	for _, r := range "hé" {
		u, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = u.(Model)
	}
	if m.keyForm.value("api_key") != "hé" {
		t.Fatalf("api_key = %q, want hé", m.keyForm.value("api_key"))
	}
	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m = u.(Model)
	if m.keyForm.value("api_key") != "h" {
		t.Fatalf("api_key after backspace = %q, want h", m.keyForm.value("api_key"))
	}
}

func TestKeyFormEnterSubmitsCallsSetVenueKeysClosesAndClearsSecret(t *testing.T) {
	var calls []setVenueKeysCall
	m := NewModel(Config{}, stubSource{setVenueKeysCall: &calls})
	m.showKeyForm = true
	m.keyForm = newKeyForm("okx")
	m.keyForm.fields[0].value = "key-123"
	m.keyForm.fields[1].value = "secret-abc"
	m.keyForm.fields[2].value = "pass-xyz"

	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatalf("enter should fire the SetVenueKeys command")
	}
	// The form must not stay open with the typed secret while the request is
	// in flight is not required by this test — what matters is that the
	// eventual result clears it, asserted below.
	msg := cmd()
	rm, ok := msg.(keyFormResultMsg)
	if !ok {
		t.Fatalf("expected keyFormResultMsg, got %T", msg)
	}
	if rm.err != nil {
		t.Fatalf("unexpected err: %v", rm.err)
	}
	if len(calls) != 1 || calls[0].venue != "okx" {
		t.Fatalf("setVenueKeys calls = %+v", calls)
	}
	if calls[0].keys.apiKey != "key-123" || calls[0].keys.apiSecret != "secret-abc" || calls[0].keys.passphrase != "pass-xyz" {
		t.Fatalf("setVenueKeys keys = %+v", calls[0].keys)
	}

	u2, _ := u.(Model).Update(rm)
	final := u2.(Model)
	if final.showKeyForm {
		t.Fatalf("keyFormResultMsg (success) should close the form")
	}
	if final.keyForm.value("api_secret") != "" {
		t.Fatalf("keyFormResultMsg (success) should leave no typed secret on the Model, got %q", final.keyForm.value("api_secret"))
	}
}

func TestKeyFormResultProvedRecordsAccountIdAndClosesForm(t *testing.T) {
	m := NewModel(Config{}, stubSource{})
	m.showKeyForm = true
	m.keyForm = newKeyForm("okx")
	m.keyForm.fields[0].value = "key-123"

	u, _ := m.Update(keyFormResultMsg{venue: "okx", accountID: "acct-789"})
	got := u.(Model)
	if got.showKeyForm {
		t.Fatalf("a proved submit should close the form")
	}
	if got.verifiedAccounts["okx"] != "acct-789" {
		t.Fatalf("verifiedAccounts[okx] = %q, want acct-789", got.verifiedAccounts["okx"])
	}
	if got.keyForm.value("api_key") != "" {
		t.Fatalf("a proved submit must not leave the typed secret on the Model, got %q", got.keyForm.value("api_key"))
	}
}

func TestPollRefreshClearsVerifiedAccounts(t *testing.T) {
	// A proved submit records verifiedAccounts["okx"], but the badge it drives
	// must not outlive the fact it asserts: once the keys behind it could have
	// been overwritten by an unproven path, the next poll must wipe it. See
	// the Model.verifiedAccounts field comment.
	m := NewModel(Config{}, stubSource{})
	m.showKeyForm = true
	m.keyForm = newKeyForm("okx")

	u, _ := m.Update(keyFormResultMsg{venue: "okx", accountID: "acct-789"})
	afterSubmit := u.(Model)
	if afterSubmit.verifiedAccounts["okx"] != "acct-789" {
		t.Fatalf("verifiedAccounts[okx] = %q, want acct-789 before the poll", afterSubmit.verifiedAccounts["okx"])
	}

	u2, _ := afterSubmit.Update(fetchMsg{
		venues: []venueRow{{venue: "okx", configured: true}},
	})
	afterPoll := u2.(Model)
	if id, ok := afterPoll.verifiedAccounts["okx"]; ok {
		t.Fatalf("a poll refresh must clear verifiedAccounts, got okx=%q", id)
	}
}

func TestKeyFormResultUnprovenSuccessRecordsNoAccountId(t *testing.T) {
	// Empty ExchangeAccountId (no proof configured for this deployment) must
	// never be treated as verification — see TestRenderAPIPaneUnprovenSuccessStaysConfigured.
	m := NewModel(Config{}, stubSource{})
	m.showKeyForm = true
	m.keyForm = newKeyForm("binance")

	u, _ := m.Update(keyFormResultMsg{venue: "binance", accountID: ""})
	got := u.(Model)
	if got.showKeyForm {
		t.Fatalf("a successful submit should close the form")
	}
	if id, ok := got.verifiedAccounts["binance"]; ok {
		t.Fatalf("empty exchange_account_id must not record a verified account, got %q", id)
	}
}

func TestKeyFormResultFailedPreconditionKeepsFormOpenAndClearsSecrets(t *testing.T) {
	m := NewModel(Config{}, stubSource{})
	m.showKeyForm = true
	m.keyForm = newKeyForm("okx")
	m.keyForm.fields[0].value = "key-123"    // api_key
	m.keyForm.fields[1].value = "secret-abc" // api_secret
	m.keyForm.fields[2].value = "pass-xyz"   // passphrase

	rejectErr := status.Error(codes.FailedPrecondition, "the exchange rejected these credentials")
	u, cmd := m.Update(keyFormResultMsg{venue: "okx", err: rejectErr})
	if cmd != nil {
		t.Fatalf("a rejection result should not itself return a command")
	}
	got := u.(Model)

	if !got.showKeyForm {
		t.Fatalf("a FailedPrecondition rejection must keep the form open")
	}
	if got.keyForm.venue != "okx" {
		t.Fatalf("the reopened form should stay scoped to the same venue, got %q", got.keyForm.venue)
	}
	// The MODEL's fields, not the render, must hold no typed secret — a
	// rejected key set is precisely the one that must not be retained.
	for _, key := range []string{"api_key", "api_secret", "passphrase"} {
		if v := got.keyForm.value(key); v != "" {
			t.Fatalf("rejected submit must clear every field; %s = %q", key, v)
		}
	}
	if got.keyFormErr == nil || got.keyFormErr.Error() == "" {
		t.Fatalf("rejection should surface the sanitized server reason via keyFormErr")
	}
	if got.verifiedAccounts["okx"] != "" {
		t.Fatalf("a rejected submit must not record a verified account")
	}
}

func TestKeyFormEscCancelsAndClears(t *testing.T) {
	m := NewModel(Config{}, stubSource{})
	m.showKeyForm = true
	m.keyForm = newKeyForm("okx")
	m.keyForm.fields[1].value = "secret-abc"

	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil {
		t.Fatalf("esc should not return a command")
	}
	got := u.(Model)
	if got.showKeyForm {
		t.Fatalf("esc should close the key form")
	}
	if got.keyForm.value("api_secret") != "" {
		t.Fatalf("esc should clear the typed secret, got %q", got.keyForm.value("api_secret"))
	}
}

func TestNodeStatusLabels(t *testing.T) {
	for _, tc := range []struct {
		row  nodeRow
		want string
	}{
		{nodeRow{schedulable: true, Status: "Ready"}, "Ready"},
		{nodeRow{schedulable: true, Status: "NotReady"}, "NotReady"},
		{nodeRow{schedulable: false, evictablePods: 3}, "Draining (3)"},
		{nodeRow{schedulable: false, evictablePods: 0}, "Drained"},
	} {
		if got := nodeStateLabel(tc.row); got != tc.want {
			t.Errorf("nodeStateLabel(%+v) = %q, want %q", tc.row, got, tc.want)
		}
	}
}
