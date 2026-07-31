// Package keymap is every global key the shell binds, in one table.
//
// Centralised for the reason opencode's packages/tui centralises it: the
// question an operator asks is "what does this key do?", and the question a
// maintainer asks is "is this key already taken?". Both are answerable here in
// one read. Bindings scattered across pane Update methods answer neither, and
// the second pane to claim a key wins silently.
//
// GLOBAL KEYS ONLY. A pane's own keys stay in that pane — this table is the set
// the shell consumes BEFORE the active pane sees the message, so every entry
// here is a key panes can never receive. That is a cost, so the table is
// deliberately small.
package keymap

import "strings"

// Action is what a global key does.
type Action int

const (
	// None means the key is not a global binding and belongs to the active pane.
	None Action = iota
	// NextPane / PrevPane cycle the tab bar.
	NextPane
	PrevPane
	// SelectPane jumps straight to a pane by ordinal (alt+1..alt+9).
	SelectPane
	// Help toggles the key overlay.
	Help
	// Quit leaves the shell.
	Quit
)

// Binding is one row of the table: the key, what it does, and how it is
// described in the help overlay and status bar.
type Binding struct {
	Keys []string
	Act  Action
	// Label is the short form for the status bar ("tab: next").
	Label string
	// Help is the long form for the overlay.
	Help string
}

// Global is the table. Order is display order in the help overlay.
//
// ctrl+c IS NOT BOUND TO QUIT HERE, deliberately. bubbletea delivers it as a
// KeyMsg like any other, so binding it would make the shell swallow the
// interrupt an operator expects to reach a running child process. The shell
// exits on "q" and ctrl+d; ctrl+c is left to mean interrupt.
var Global = []Binding{
	{Keys: []string{"tab", "right", "l"}, Act: NextPane, Label: "tab: next", Help: "next pane"},
	{Keys: []string{"shift+tab", "left", "h"}, Act: PrevPane, Label: "shift+tab: prev", Help: "previous pane"},
	{Keys: []string{"alt+1", "alt+2", "alt+3", "alt+4", "alt+5", "alt+6", "alt+7", "alt+8", "alt+9"},
		Act: SelectPane, Label: "alt+N: jump", Help: "jump to pane N"},
	{Keys: []string{"?"}, Act: Help, Label: "?: help", Help: "toggle this help"},
	{Keys: []string{"q", "ctrl+d"}, Act: Quit, Label: "q: quit", Help: "quit kanz"},
}

// Lookup maps a pressed key to its global action.
//
// It returns None for anything unbound, which is how a keystroke reaches the
// active pane. The default must be None and not a guess: a shell that
// half-matches keys steals input from whatever the operator is typing into.
func Lookup(key string) Action {
	for _, b := range Global {
		for _, k := range b.Keys {
			if k == key {
				return b.Act
			}
		}
	}
	return None
}

// PaneOrdinal returns the zero-based pane index for an alt+N key, and whether
// the key was one. alt+1 is the first pane.
func PaneOrdinal(key string) (int, bool) {
	if len(key) != len("alt+1") || key[:4] != "alt+" {
		return 0, false
	}
	d := key[4]
	if d < '1' || d > '9' {
		return 0, false
	}
	return int(d - '1'), true
}

// StatusHints is the compact one-line summary for the status bar.
func StatusHints() []string {
	out := make([]string, 0, len(Global))
	for _, b := range Global {
		out = append(out, b.Label)
	}
	return out
}

// A GLOBAL BINDING MUST NOT SWALLOW A CHARACTER SOMEBODY IS TYPING.
//
// The table above binds `h`, `l`, `q` and `?`. That is ordinary for a TUI made
// of read-only panes, and it made the shell's DEFAULT pane unusable: typing
// "help" into the Copilot prompt sent `h` to prev-pane, `e` to the pane, `l` to
// next-pane, `p` to the pane — and `q` quit the shell mid-sentence.
//
// It shipped in #65 because the routing test probed with `x` and `z`, which
// happen to be unbound. A test that picks its own keys proves the mechanism, not
// the table.
//
// So a pane that accepts typed text (pane.TextInput) is asked FIRST, and the
// shell keeps only the keys a text field would never produce.

// TextSafe reports whether the shell may keep key even while the active pane is
// taking typed input.
//
// The rule is derived from the key itself rather than a per-binding flag, so a
// new binding cannot forget to declare it — and it is deliberately conservative:
//
//   - anything with a modifier (alt+1, shift+tab, ctrl+d) — a text field does
//     not receive these as characters
//   - tab and esc — named keys, not characters
//
// Everything else is refused, which covers bare letters AND the arrows. Arrows
// matter because a text field wants left/right for the cursor: the Copilot pane
// does not implement cursor movement yet, and binding them globally is exactly
// how it would be prevented from ever doing so.
func TextSafe(key string) bool {
	if strings.Contains(key, "+") {
		return true
	}
	switch key {
	case "tab", "esc":
		return true
	}
	return false
}

// LookupFor resolves key for the active pane, honouring whether that pane is
// taking typed input.
//
// acceptsText false behaves exactly like Lookup — a read-only pane keeps the
// full table, including the vim-style h/l that make it pleasant to drive.
func LookupFor(key string, acceptsText bool) Action {
	if acceptsText && !TextSafe(key) {
		return None
	}
	return Lookup(key)
}
