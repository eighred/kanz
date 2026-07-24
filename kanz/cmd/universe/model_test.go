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
