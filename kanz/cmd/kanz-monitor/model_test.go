package main

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// A monitor an operator cannot quit is a monitor they will kill with the process
// manager, losing the clean unsubscribe. q and ctrl+c must both quit.
func TestModel_QuitKeys(t *testing.T) {
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'q'}},
		{Type: tea.KeyCtrlC},
	} {
		m := newModel(Config{})
		_, cmd := m.Update(key)
		if cmd == nil {
			t.Fatalf("key %v produced no command; expected tea.Quit", key)
		}
		// tea.Quit is a func returning a tea.QuitMsg; invoke and check the type.
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("key %v did not map to tea.Quit", key)
		}
	}
}

// View must never panic on the zero model — Bubble Tea calls View before the
// first data message arrives, and a nil-map deref there crashes the UI on launch.
func TestModel_ViewOnEmptyModelDoesNotPanic(t *testing.T) {
	m := newModel(Config{})
	_ = m.View()
}
