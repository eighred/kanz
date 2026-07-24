package main

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

type stubSource struct{ msg fetchMsg }

func (s stubSource) fetch(context.Context) (fetchMsg, error) { return s.msg, nil }

func (s stubSource) addNode(context.Context, addNodeInput) (string, error)  { return "p-1", nil }
func (s stubSource) listProvisions(context.Context) ([]provisionRow, error) { return nil, nil }

func (s stubSource) testConnection(context.Context, string, int32) (testConnResult, error) {
	return testConnResult{reachable: true, latencyMs: 7}, nil
}

func (s stubSource) cordon(context.Context, string) error   { return nil }
func (s stubSource) uncordon(context.Context, string) error { return nil }
func (s stubSource) drain(context.Context, string) error    { return nil }

func TestFetchMsgPopulatesModel(t *testing.T) {
	m := newModel(Config{}, stubSource{})
	updated, _ := m.Update(fetchMsg{
		nodes:    []nodeRow{{Name: "london", Status: "Ready", Region: "europe"}},
		clusters: []clusterRow{{Region: "europe", Online: 1}},
	})
	got := updated.(model)
	if len(got.nodes) != 1 || got.nodes[0].Name != "london" {
		t.Fatalf("nodes = %+v", got.nodes)
	}
	if len(got.clusters) != 1 || got.clusters[0].Region != "europe" {
		t.Fatalf("clusters = %+v", got.clusters)
	}
}

func TestTabSwitchesPane(t *testing.T) {
	m := newModel(Config{}, stubSource{})
	if m.active != paneNodes {
		t.Fatalf("initial pane = %v, want paneNodes", m.active)
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if updated.(model).active != paneClusters {
		t.Fatalf("after tab, pane = %v, want paneClusters", updated.(model).active)
	}
}

func TestFetchErrorGoesToStatusNotCrash(t *testing.T) {
	m := newModel(Config{}, stubSource{})
	updated, _ := m.Update(fetchMsg{err: context.DeadlineExceeded})
	if updated.(model).err == nil {
		t.Fatalf("expected err recorded on model")
	}
}

func TestQuitKey(t *testing.T) {
	m := newModel(Config{}, stubSource{})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatalf("expected tea.Quit command on q")
	}
}

func TestNodeSelectionMovesAndActionsFire(t *testing.T) {
	m := newModel(Config{}, stubSource{})
	m.active = paneNodes
	m.nodes = []nodeRow{{Name: "a", schedulable: true}, {Name: "b", schedulable: true}}
	// down moves selection
	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if u.(model).selected != 1 {
		t.Fatalf("down should select row 1, got %d", u.(model).selected)
	}
	// 'c' on the selected node returns a command
	u2, cmd := u.(model).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	if cmd == nil {
		t.Fatalf("c (cordon) should return a command")
	}
	_ = u2
}

func TestDrainAsksForConfirmation(t *testing.T) {
	m := newModel(Config{}, stubSource{})
	m.active = paneNodes
	m.nodes = []nodeRow{{Name: "a", schedulable: true, evictablePods: 3}}
	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	if cmd != nil {
		t.Fatalf("d should NOT fire drain immediately — it opens a confirm")
	}
	if !u.(model).confirmingDrain {
		t.Fatalf("d should enter the confirm state")
	}
	// 'y' confirms and fires
	_, cmd2 := u.(model).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if cmd2 == nil {
		t.Fatalf("y should fire the drain command")
	}
}

func TestNodeStatusLabels(t *testing.T) {
	for _, tc := range []struct {
		row  nodeRow
		want string
	}{
		{nodeRow{schedulable: true}, "Ready"},
		{nodeRow{schedulable: false, evictablePods: 3}, "Draining (3)"},
		{nodeRow{schedulable: false, evictablePods: 0}, "Drained"},
	} {
		if got := nodeStateLabel(tc.row); got != tc.want {
			t.Errorf("nodeStateLabel(%+v) = %q, want %q", tc.row, got, tc.want)
		}
	}
}
