package main

import (
	"strings"
	"testing"
)

func TestRenderNodesPaneShowsRows(t *testing.T) {
	m := model{
		active: paneNodes,
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
	m := model{
		active: paneClusters,
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
	m := model{active: paneNodes, width: 80, height: 24, err: errStub{}}
	if !strings.Contains(m.render(), "boom") {
		t.Errorf("render should surface the error text\n%s", m.render())
	}
}

func TestRenderAPIPaneShowsPresenceNeverKeyMaterial(t *testing.T) {
	m := model{
		active: paneAPI,
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
		t.Errorf("render is not pure for paneAPI: two calls produced different output\nfirst:\n%s\nsecond:\n%s", out, out2)
	}
}

func TestRenderAPIPaneShowsSetKeysHint(t *testing.T) {
	m := model{active: paneAPI, width: 80, height: 24}
	if !strings.Contains(m.render(), "set keys") {
		t.Errorf("render should show the [k] set keys hint for paneAPI\n%s", m.render())
	}
}

type errStub struct{}

func (errStub) Error() string { return "boom" }

func TestRenderMoveRegionPromptIsPureAndStable(t *testing.T) {
	m := model{
		active: paneNodes,
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
