package universe

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestTabCyclesThroughAllPanesFromAnyStart drives tab from every starting pane
// rather than only from the default one. A cycle that is broken from one entry
// point is still a cycle from the others, so a start-at-ScreenNodes-only test
// would keep passing while an operator who tabbed once could no longer reach a
// third of the console.
func TestTabCyclesThroughAllPanesFromAnyStart(t *testing.T) {
	for start := 0; start < 3; start++ {
		m := NewModel(Config{}, stubSource{})
		for i := 0; i < start; i++ {
			u, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
			m = u.(Model)
		}
		seen := map[Screen]bool{m.active: true}
		for i := 0; i < 3; i++ {
			u, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
			m = u.(Model)
			seen[m.active] = true
		}
		if len(seen) != 3 {
			t.Errorf("starting from pane %d, tab reached only %d of 3 panes — a pane an "+
				"operator cannot reach is a feature that does not exist", start, len(seen))
		}
	}
}

// TestTabReturnsToTheStartingPane is the other half of "cycle": reaching all
// three panes proves no pane is stranded, but only returning to the start
// proves the walk is closed. An operator navigates by counting presses, so a
// path that visits everything and then parks somewhere new is still a trap.
func TestTabReturnsToTheStartingPane(t *testing.T) {
	for start := 0; start < 3; start++ {
		m := NewModel(Config{}, stubSource{})
		for i := 0; i < start; i++ {
			u, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
			m = u.(Model)
		}
		origin := m.active
		for i := 0; i < 3; i++ {
			u, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
			m = u.(Model)
		}
		if m.active != origin {
			t.Errorf("starting from pane %d, three tabs landed on pane %d rather than back "+
				"at the start — tab is a walk, not a cycle", start, m.active)
		}
	}
}

// TestActionKeysAreInertWithAnEmptyEstate covers the state every session opens
// in: the first poll has not landed, so there is no row to act on. Each of
// these keys reads nodes[selected], and index 0 of an empty estate is either a
// panic or an action against a node that is not there.
func TestActionKeysAreInertWithAnEmptyEstate(t *testing.T) {
	for _, key := range []string{"c", "u", "d", "m"} {
		m := NewModel(Config{}, stubSource{})
		u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		if cmd != nil {
			t.Errorf("%q issued a command against an empty node list — an action with no "+
				"target must do nothing rather than act on index 0 of nothing", key)
		}
		got := u.(Model)
		if got.confirmingDrain {
			t.Errorf("%q opened the drain confirm with no node selected", key)
		}
		if got.movingRegion {
			t.Errorf("%q opened the move-region prompt with no node selected", key)
		}
	}
}

// TestNodeActionKeysAreInertOutsideTheNodesPane is the same guard along the
// other axis. Every node action is gated on m.active == ScreenNodes, so an
// operator reading the Clusters or API pane must not be able to cordon or drain
// whatever row the Nodes pane happens to have highlighted underneath — the
// estate they are acting on is not the one on screen.
func TestNodeActionKeysAreInertOutsideTheNodesPane(t *testing.T) {
	for _, active := range []Screen{ScreenClusters, ScreenAPI} {
		for _, key := range []string{"c", "u", "d", "m"} {
			m := NewModel(Config{}, stubSource{})
			m.active = active
			m.nodes = []nodeRow{{Name: "london", schedulable: true}}

			u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
			if cmd != nil {
				t.Errorf("pane %d: %q issued a node command from a pane that shows no nodes",
					active, key)
			}
			got := u.(Model)
			if got.confirmingDrain || got.movingRegion {
				t.Errorf("pane %d: %q opened a node prompt from a pane that shows no nodes",
					active, key)
			}
		}
	}
}

// TestAddAndKeyFormsOnlyOpenFromTheirOwnPane pins the two form-opening keys to
// their panes for the same reason: 'a' provisions a node and 'k' writes venue
// credentials, and neither has any meaning from a pane that cannot show the
// thing it acts on.
func TestAddAndKeyFormsOnlyOpenFromTheirOwnPane(t *testing.T) {
	for _, active := range []Screen{ScreenClusters, ScreenAPI} {
		m := NewModel(Config{}, stubSource{})
		m.active = active
		u, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
		if u.(Model).showForm {
			t.Errorf("pane %d: 'a' opened the Add Node form outside the Nodes pane", active)
		}
	}
	for _, active := range []Screen{ScreenNodes, ScreenClusters} {
		m := NewModel(Config{}, stubSource{})
		m.active = active
		m.venues = []venueRow{{venue: "binance"}}
		u, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
		if u.(Model).showKeyForm {
			t.Errorf("pane %d: 'k' opened the Set API Keys form outside the API pane", active)
		}
	}
}

// TestSelectionKeysCannotLeaveTheirBounds walks the cursor past both ends of a
// short list in both panes. An index that runs past the end is not a cosmetic
// bug here: selected is what every action key dereferences.
func TestSelectionKeysCannotLeaveTheirBounds(t *testing.T) {
	m := NewModel(Config{}, stubSource{})
	m.active = ScreenNodes
	m.nodes = []nodeRow{{Name: "a"}, {Name: "b"}}
	for i := 0; i < 5; i++ {
		u, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = u.(Model)
	}
	if m.selected != 1 {
		t.Errorf("down past the end left selected=%d, want 1 (last row)", m.selected)
	}
	for i := 0; i < 5; i++ {
		u, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
		m = u.(Model)
	}
	if m.selected != 0 {
		t.Errorf("up past the start left selected=%d, want 0", m.selected)
	}

	m = NewModel(Config{}, stubSource{})
	m.active = ScreenAPI
	m.venues = []venueRow{{venue: "binance"}, {venue: "okx"}}
	for i := 0; i < 5; i++ {
		u, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = u.(Model)
	}
	if m.apiSelected != 1 {
		t.Errorf("down past the end left apiSelected=%d, want 1 (last row)", m.apiSelected)
	}
	for i := 0; i < 5; i++ {
		u, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
		m = u.(Model)
	}
	if m.apiSelected != 0 {
		t.Errorf("up past the start left apiSelected=%d, want 0", m.apiSelected)
	}
}

// TestEveryPaneRendersWithAnEmptyEstate is the cheapest possible crash sweep:
// a fresh session shows every pane before the first poll lands, and a nil slice
// indexed by a header loop is a panic that takes the whole console down.
func TestEveryPaneRendersWithAnEmptyEstate(t *testing.T) {
	for _, active := range []Screen{ScreenNodes, ScreenClusters, ScreenAPI} {
		m := NewModel(Config{}, stubSource{})
		m.active = active
		if out := m.render(); out == "" {
			t.Errorf("pane %d rendered an empty frame with no data — a blank screen is "+
				"indistinguishable from a hung TUI", active)
		}
	}
}
