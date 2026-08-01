// Package theme is every style the shell draws with, in one place.
//
// It is separated for the reason opencode's packages/tui separates it: a style
// defined next to the widget that uses it gets copied to the next widget, and
// the copies drift. One palette means a reviewer can see the whole visual
// surface without reading the views.
//
// # BLACK AND WHITE, BY DECISION (owner, 2026-08-01)
//
// The shell carries NO COLOUR. bubbletea and lipgloss stay — they are the event
// loop and the layout engine — but every distinction below is drawn with
// ATTRIBUTES (bold, faint, underline, reverse) rather than hue, and no
// foreground colour is set at all: text renders in whatever the terminal's own
// foreground is.
//
// The reason given was that the TUI is the system's framework and a placeholder
// for the initial development phase, and that is the right time to make this
// choice rather than later — a palette is easy to add once and expensive to
// remove from thirty call sites.
//
// It also removes a class of bug outright. The previous palette was
// AdaptiveColor precisely because hardcoded dark-terminal colours are the most
// common way a TUI becomes unreadable for half its users, invisibly to whoever
// wrote it. Attributes have no light/dark form to get wrong: bold is bold on
// both.
//
// # THE ONE STYLE WITH A RULE ATTACHED SURVIVED THE CHANGE
//
// BusTab used to be the only coloured style with a stated rule — the panes that
// leave this process, one of which is the platform kill switch, must be
// distinguishable WITHOUT READING. Dropping colour could have quietly dropped
// that guarantee, which is why it is now reverse video: a solid inverted block
// is if anything harder to miss than red was, and it is the one attribute
// nothing else here uses.
package theme

import "github.com/charmbracelet/lipgloss"

// Styles the shell composes from. They are values, not functions, because a
// style is immutable in lipgloss — copying one to add a width is free and does
// not mutate the shared original.
//
// None of them sets a Foreground. That is the point: the terminal's own
// foreground is the only "colour" in the shell.
var (
	// ActiveTab is the pane currently shown. Bold AND underlined, because bold
	// alone is a weak signal in a row of short words.
	ActiveTab = lipgloss.NewStyle().Bold(true).Underline(true)

	// InactiveTab is a pane one keypress away — present, not competing.
	InactiveTab = lipgloss.NewStyle().Faint(true)

	// BusTab is an inactive tab for a pane that leaves this process and runs
	// with its own SPIFFE identity — kanz-halt, kanz-monitor.
	//
	// IT IS DRAWN DIFFERENTLY ON PURPOSE, AND THIS IS A SECURITY PROPERTY RATHER
	// THAN DECORATION. Selecting one suspends the shell and hands the terminal to
	// another program; one of them is the platform kill switch. An operator
	// should be able to see, WITHOUT READING, that those two tabs are not like
	// the others.
	//
	// Reverse video rather than a colour, and reverse is used by nothing else in
	// this file so the signal stays unambiguous in monochrome.
	BusTab = lipgloss.NewStyle().Reverse(true)

	// StatusBar is the bottom line: key hints and pane state.
	StatusBar = lipgloss.NewStyle().Faint(true)

	// Rule is the horizontal separator under the tab bar.
	Rule = lipgloss.NewStyle().Faint(true)

	// Body is the pane content area — the terminal's plain foreground, unstyled.
	Body = lipgloss.NewStyle()

	// Prompt is an input prompt marker ("kanz› "). A style, not a bare attribute:
	// callers must not build styles at the call site, or the palette stops being
	// the one place the visual surface is defined.
	Prompt = lipgloss.NewStyle().Bold(true)

	// Error is a failure surfaced in the shell rather than swallowed. Bold and
	// underlined so it separates from Body (plain) and from Prompt (bold alone),
	// which is the distinction that used to be carried by red.
	Error = lipgloss.NewStyle().Bold(true).Underline(true)
)
