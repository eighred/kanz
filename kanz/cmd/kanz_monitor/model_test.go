package main

import (
	"errors"
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

// pollMsg must (a) re-arm the ticker — a dropped tea.Cmd here means polling
// runs once and silently stops for the life of the program — and (b) replace
// m.counters wholesale while MERGING m.health key-by-key, so a service status
// reported on a prior tick survives a tick that didn't mention it.
func TestUpdate_PollMsg(t *testing.T) {
	m := newModel(Config{})
	m.health["preexisting"] = true
	m.counters = counterSnapshot{Filled: 1, Rejected: 2}

	newCounters := counterSnapshot{Filled: 9, Rejected: 0, Quarantined: 3}
	updated, cmd := m.Update(pollMsg{
		counters: newCounters,
		health:   map[string]bool{"gateway": true},
	})

	if cmd == nil {
		t.Fatal("pollMsg produced no tea.Cmd; ticker would stop after one poll")
	}

	got, ok := updated.(model)
	if !ok {
		t.Fatalf("Update returned %T, want model", updated)
	}

	if got.counters != newCounters {
		t.Fatalf("counters = %+v, want full replace with %+v", got.counters, newCounters)
	}

	if v, ok := got.health["preexisting"]; !ok || !v {
		t.Fatalf("health[\"preexisting\"] lost on merge: %v, %v", v, ok)
	}
	if v, ok := got.health["gateway"]; !ok || !v {
		t.Fatalf("health[\"gateway\"] not merged in: %v, %v", v, ok)
	}
}

// A poll cycle that failed (gateway unreachable, etc.) must still land its
// error in m.err for the status line, without panicking, AND must still
// re-arm the next tick — a failed poll must not stop polling.
func TestUpdate_PollMsg_ErrStillRearms(t *testing.T) {
	m := newModel(Config{})
	wantErr := errors.New("metrics: connection refused")

	updated, cmd := m.Update(pollMsg{
		counters: counterSnapshot{},
		health:   map[string]bool{},
		err:      wantErr,
	})

	if cmd == nil {
		t.Fatal("pollMsg with err produced no tea.Cmd; ticker would stop on a failed poll")
	}

	got, ok := updated.(model)
	if !ok {
		t.Fatalf("Update returned %T, want model", updated)
	}
	if got.err != wantErr {
		t.Fatalf("m.err = %v, want %v", got.err, wantErr)
	}
}
