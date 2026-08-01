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
	// EnterInput focuses the active pane's text entry. Only meaningful in
	// Navigate mode, on a pane that takes typed text.
	EnterInput
	// LeaveInput returns to Navigate mode. Only meaningful in Input mode.
	LeaveInput
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
// NAVIGATION IS ARROWS AND TAB, NOT h/l. The vim pair was removed with this
// table's move to `h` for help: `h` and `l` are printable characters, and a
// printable character bound globally is one the pane taking text never receives.
// That is not hypothetical — `h`, `l`, `q` and `?` were all global in #65, which
// made typing "help" into the Copilot prompt navigate two panes and quit the
// shell. Modal input (Mode, below) fixed the symptom; removing the two bindings
// that had no non-printable alternative removes the cause.
//
// `q` stays printable and global because Navigate mode is where it applies and
// TextSafe excludes it from Input mode. `h` is the same trade, now for help.
var Global = []Binding{
	{Keys: []string{"tab", "right"}, Act: NextPane, Label: "tab: next", Help: "change pane"},
	{Keys: []string{"shift+tab", "left"}, Act: PrevPane, Label: "shift+tab: prev", Help: "change pane"},
	{Keys: []string{"alt+1", "alt+2", "alt+3", "alt+4", "alt+5", "alt+6", "alt+7", "alt+8", "alt+9"},
		Act: SelectPane, Label: "alt+N: jump", Help: "jump to pane N"},
	{Keys: []string{"h"}, Act: Help, Label: "h: help", Help: "toggle help"},
	{Keys: []string{"q", "ctrl+d"}, Act: Quit, Label: "q: quit", Help: "quit kanz"},
	{Keys: []string{"i", "enter"}, Act: EnterInput, Label: "i: type", Help: "focus input field"},
	{Keys: []string{"esc"}, Act: LeaveInput, Label: "esc: navigate", Help: "unfocus input field"},
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

// Mode is what a keystroke MEANS right now.
//
// THE PROBLEM THIS SOLVES IS NOT KEY THEFT, IT IS AMBIGUITY. The first fix made
// text panes keep printable characters, which stopped the Copilot prompt eating
// its own input — but it left `l` navigating on the Nodes tab and typing on the
// Copilot tab. One key, two meanings, decided by which tab you were on. That is
// not learnable, and no amount of care makes it so.
//
// A mode makes the answer uniform: in Navigate every binding is live everywhere,
// in Input the pane gets the characters and only keys a text field cannot
// produce stay global.
type Mode int

const (
	// Navigate is the read-only posture: the whole table is live.
	Navigate Mode = iota
	// Input means the active pane is taking typed characters.
	Input
)

func (m Mode) String() string {
	if m == Input {
		return "INPUT"
	}
	return "NAV"
}

// TextSafe reports whether the shell may keep key while a pane is taking typed
// input.
//
// Derived from the key itself rather than a per-binding flag, so a new binding
// cannot forget to declare it, and deliberately conservative:
//
//   - anything with a modifier (alt+1, shift+tab, ctrl+d) — a text field does
//     not receive these as characters
//   - tab, and esc, which is how Input mode is left
//
// Everything else is refused, which covers bare letters AND the arrows. Arrows
// matter because a text field wants left/right for the cursor.
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

// LookupIn resolves key for the current mode.
//
// In Input mode only text-safe keys resolve, so the pane receives everything a
// person could type. In Navigate mode the full table is live — including the
// vim-style keys, which is the posture they were written for.
func LookupIn(key string, mode Mode) Action {
	if mode == Input && !TextSafe(key) {
		return None
	}
	act := Lookup(key)
	// EnterInput and LeaveInput are each meaningful in exactly one mode. Letting
	// them resolve in the other is how `i` would stop being typeable, and how esc
	// would silently do nothing that anybody could see.
	switch {
	case act == EnterInput && mode != Navigate:
		return None
	case act == LeaveInput && mode != Input:
		return None
	}
	return act
}

// StatusHintsIn is the compact summary for the current mode: bindings that
// cannot fire are not advertised.
func StatusHintsIn(mode Mode) []string {
	out := make([]string, 0, len(Global))
	for _, b := range Global {
		if LookupIn(b.Keys[0], mode) == None {
			continue
		}
		out = append(out, b.Label)
	}
	return out
}
