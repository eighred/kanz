package universe

import (
	"strings"
	"testing"
)

func TestRenderNodesPaneShowsRows(t *testing.T) {
	m := Model{
		active: ScreenNodes,
		width:  100, height: 30,
		nodes: []nodeRow{
			{Name: "london", Status: "Ready", Roles: "control-plane", Region: "europe", Version: "v1.31.3", Age: "3d", schedulable: true},
			{Name: "tokyo", Roles: "-", Region: "asia", Version: "v1.31.3", Age: "3d", schedulable: false, evictablePods: 2},
		},
	}
	out := m.render()
	for _, want := range []string{"london", "Ready", "europe", "tokyo", "Draining (2)"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q\n%s", want, out)
		}
	}
}

func TestRenderClustersPaneShowsCounts(t *testing.T) {
	m := Model{
		active: ScreenClusters,
		width:  100, height: 30,
		clusters: []clusterRow{{Region: "usa", Online: 2, Offline: 1}},
	}
	out := m.render()
	for _, want := range []string{"usa", "2", "1"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q\n%s", want, out)
		}
	}
}

func TestRenderErrorShownInStatus(t *testing.T) {
	m := Model{active: ScreenNodes, width: 80, height: 24, err: errStub{}}
	if !strings.Contains(m.render(), "boom") {
		t.Errorf("render should surface the error text\n%s", m.render())
	}
}

func TestRenderAPIPaneShowsPresenceNeverKeyMaterial(t *testing.T) {
	m := Model{
		active: ScreenAPI,
		width:  100, height: 30,
		venues: []venueRow{
			{venue: "binance", configured: true},
			{venue: "coinbase", configured: false},
		},
	}
	out := m.render()
	for _, want := range []string{"binance", "configured", "coinbase", "not set"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q\n%s", want, out)
		}
	}
	for _, forbidden := range []string{"sk-", "secret", "passphrase"} {
		if strings.Contains(strings.ToLower(out), strings.ToLower(forbidden)) {
			t.Errorf("render must never show key material, found %q\n%s", forbidden, out)
		}
	}
	out2 := m.render()
	if out != out2 {
		t.Errorf("render is not pure for ScreenAPI: two calls produced different output\nfirst:\n%s\nsecond:\n%s", out, out2)
	}
}

func TestRenderAPIPaneShowsVerifiedAccountId(t *testing.T) {
	m := Model{
		active: ScreenAPI,
		width:  100, height: 30,
		venues: []venueRow{
			{venue: "okx", configured: true},
			{venue: "binance", configured: true},
		},
		verifiedAccounts: map[string]string{"okx": "acct-789"},
	}
	out := m.render()
	if !strings.Contains(out, "✓ verified · account acct-789") {
		t.Errorf("proved venue should render the verified wording with its account id\n%s", out)
	}
	if !strings.Contains(out, "✓ configured") {
		t.Errorf("unproven venue should keep the existing configured wording\n%s", out)
	}
	out2 := m.render()
	if out != out2 {
		t.Errorf("render is not pure: two calls produced different output\nfirst:\n%s\nsecond:\n%s", out, out2)
	}
}

func TestRenderAPIPaneAfterPollRefreshFallsBackToConfigured(t *testing.T) {
	// A proved submit renders "verified", but that badge must not survive a
	// poll refresh — the poll is the point at which the keys behind it could
	// already have been overwritten by an unproven path (see Model.go's
	// fetchMsg handler and the verifiedAccounts field comment).
	m := Model{
		active: ScreenAPI,
		width:  100, height: 30,
		venues:           []venueRow{{venue: "okx", configured: true}},
		verifiedAccounts: map[string]string{"okx": "acct-789"},
	}
	out := m.render()
	if !strings.Contains(out, "✓ verified · account acct-789") {
		t.Fatalf("precondition: proved venue should render verified before the poll\n%s", out)
	}

	u, _ := m.Update(fetchMsg{venues: []venueRow{{venue: "okx", configured: true}}})
	got := u.(Model)
	if id, ok := got.verifiedAccounts["okx"]; ok {
		t.Fatalf("a poll refresh must clear verifiedAccounts, got okx=%q", id)
	}

	out2 := got.render()
	if !strings.Contains(out2, "✓ configured") {
		t.Errorf("after a poll refresh, a proved-then-refreshed venue must fall back to the plain configured wording\n%s", out2)
	}
	if strings.Contains(out2, "verified") {
		t.Errorf("after a poll refresh, render must not claim verification\n%s", out2)
	}
	if strings.Contains(out2, "acct-789") {
		t.Errorf("after a poll refresh, render must not still show the stale account id\n%s", out2)
	}
}

func TestRenderAPIPaneUnprovenSuccessStaysConfigured(t *testing.T) {
	// Empty exchange_account_id must render the existing "configured" wording,
	// byte-unchanged — an unproven deployment must not start claiming
	// verification.
	m := Model{
		active: ScreenAPI,
		width:  100, height: 30,
		venues:           []venueRow{{venue: "binance", configured: true}},
		verifiedAccounts: map[string]string{"binance": ""},
	}
	out := m.render()
	if !strings.Contains(out, "✓ configured") {
		t.Errorf("empty account id must not render as verified\n%s", out)
	}
	if strings.Contains(out, "verified") {
		t.Errorf("empty account id must never render the verified wording\n%s", out)
	}
}

func TestRenderAPIPaneShowsSetKeysHint(t *testing.T) {
	m := Model{active: ScreenAPI, width: 80, height: 24}
	if !strings.Contains(m.render(), "set keys") {
		t.Errorf("render should show the [k] set keys hint for ScreenAPI\n%s", m.render())
	}
}

func TestRenderKeyFormShowsMaskedFieldsAndIsPure(t *testing.T) {
	f := newKeyForm("okx")
	f.fields[0].value = "key-123"
	f.fields[1].value = "supersecretvalue"
	f.fields[2].value = "pass-xyz"
	m := Model{
		active: ScreenAPI,
		width:  100, height: 30,
		showKeyForm: true,
		keyForm:     f,
	}
	out1 := m.render()
	if strings.Contains(out1, "supersecretvalue") {
		t.Errorf("render must never show the raw secret\n%s", out1)
	}
	if !strings.Contains(out1, "key-123") {
		t.Errorf("render should show the plain api_key\n%s", out1)
	}
	out2 := m.render()
	if out1 != out2 {
		t.Errorf("render is not pure for the key form: two calls produced different output\nfirst:\n%s\nsecond:\n%s", out1, out2)
	}
}

func TestRenderKeyFormShowsRejectionReasonInline(t *testing.T) {
	m := Model{
		active: ScreenAPI,
		width:  100, height: 30,
		showKeyForm: true,
		keyForm:     newKeyForm("okx"),
		keyFormErr:  errStub{},
	}
	out := m.render()
	if !strings.Contains(out, "boom") {
		t.Errorf("render should show the rejection reason inline on the form\n%s", out)
	}
}

type errStub struct{}

func (errStub) Error() string { return "boom" }

func TestRenderMoveRegionPromptIsPureAndStable(t *testing.T) {
	m := Model{
		active: ScreenNodes,
		width:  100, height: 30,
		nodes:        []nodeRow{{Name: "london", Status: "Ready", Region: "europe", schedulable: true}},
		movingRegion: true,
		moveInput:    "asia",
	}
	out1 := m.render()
	for _, want := range []string{"london", "asia"} {
		if !strings.Contains(out1, want) {
			t.Errorf("render missing %q\n%s", want, out1)
		}
	}
	out2 := m.render()
	if out1 != out2 {
		t.Errorf("render is not pure: two calls produced different output\nfirst:\n%s\nsecond:\n%s", out1, out2)
	}
}
