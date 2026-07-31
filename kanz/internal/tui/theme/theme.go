// Package theme is every colour and style the shell draws with, in one place.
//
// It is separated for the reason opencode's packages/tui separates it: a style
// defined next to the widget that uses it gets copied to the next widget, and
// the copies drift. One palette means changing the accent colour is one edit,
// and it means a reviewer can see the whole visual surface without reading the
// views.
//
// ADAPTIVE, NOT FIXED. lipgloss.AdaptiveColor picks per the terminal's detected
// background, so the shell is legible on a light terminal without a setting.
// Hardcoding dark-terminal colours is the single most common way a TUI becomes
// unreadable for half its users, and it is invisible to whoever wrote it.
package theme

import "github.com/charmbracelet/lipgloss"

// Palette is the shell's colours. Values are AdaptiveColor so each has a
// light-terminal and a dark-terminal form.
var (
	// Accent marks the active pane and the cursor.
	Accent = lipgloss.AdaptiveColor{Light: "#1F6FEB", Dark: "#58A6FF"}
	// Muted is for inactive tabs and secondary text — present, not competing.
	Muted = lipgloss.AdaptiveColor{Light: "#6E7781", Dark: "#8B949E"}
	// Text is default foreground.
	Text = lipgloss.AdaptiveColor{Light: "#1F2328", Dark: "#E6EDF3"}
	// Danger marks the destructive plane. See BusTab below — this is the only
	// colour with a rule attached to it.
	Danger = lipgloss.AdaptiveColor{Light: "#B22222", Dark: "#FF7B72"}
	// Border draws frames and rules.
	Border = lipgloss.AdaptiveColor{Light: "#D0D7DE", Dark: "#30363D"}
)

// Styles the shell composes from. They are values, not functions, because a
// style is immutable in lipgloss — copying one to add a width is free and does
// not mutate the shared original.
var (
	// ActiveTab is the pane currently shown.
	ActiveTab = lipgloss.NewStyle().Bold(true).Foreground(Accent)
	// InactiveTab is a pane one keypress away.
	InactiveTab = lipgloss.NewStyle().Foreground(Muted)

	// BusTab is an inactive tab for a pane that leaves this process and runs
	// with its own SPIFFE identity — kanz-halt, kanz-monitor.
	//
	// IT IS COLOURED DIFFERENTLY ON PURPOSE. Selecting one suspends the shell
	// and hands the terminal to another program; one of them is the platform
	// kill switch. An operator should be able to see, without reading, that
	// those two tabs are not like the others.
	BusTab = lipgloss.NewStyle().Foreground(Danger)

	// StatusBar is the bottom line: key hints and pane state.
	StatusBar = lipgloss.NewStyle().Foreground(Muted)

	// Rule is the horizontal separator under the tab bar.
	Rule = lipgloss.NewStyle().Foreground(Border)

	// Body is the pane content area.
	Body = lipgloss.NewStyle().Foreground(Text)

	// Prompt is an input prompt marker ("kanz› "). A style, not a bare colour:
	// callers must not build styles at the call site, or the palette stops being
	// the one place the visual surface is defined.
	Prompt = lipgloss.NewStyle().Bold(true).Foreground(Accent)

	// Error is a failure surfaced in the shell rather than swallowed.
	Error = lipgloss.NewStyle().Bold(true).Foreground(Danger)
)
