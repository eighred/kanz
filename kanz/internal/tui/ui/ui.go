// Package ui is the shell's own rendering primitives: tab bar, status bar, and
// the clipping every pane's output passes through.
//
// HAND-ROLLED, AND THAT IS THE EXISTING CONVENTION HERE RATHER THAN A NEW ONE.
// `bubbles` is not a dependency of this module, and cmd/universe/form.go
// already records the same choice for its form widget ("a minimal hand-rolled
// form (no bubbles dependency)"). This package turns that per-file habit into
// structure, so the next widget has somewhere to go other than the pane that
// needed it first — which is how the copies start.
//
// bubbletea and lipgloss stay: they are the event loop and the styling, the
// layer opencode's packages/tui gets from OpenTUI. What is written here is
// everything ABOVE that line.
package ui

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"

	"github.com/eighred/kanz/internal/tui/keymap"
	"github.com/eighred/kanz/internal/tui/pane"
	"github.com/eighred/kanz/internal/tui/theme"
)

// Tab is one entry in the tab bar.
type Tab struct {
	Title  string
	Plane  pane.Plane
	Active bool
}

// TabBar renders the pane tabs, clipped to width.
//
// Bus-plane tabs are styled with theme.BusTab so the two panes that leave this
// process — one of which is the kill switch — do not look like the rest. See
// the theme package for why that is a rule and not decoration.
func TabBar(tabs []Tab, width int) string {
	if len(tabs) == 0 || width <= 0 {
		return ""
	}
	parts := make([]string, 0, len(tabs))
	for i, t := range tabs {
		label := " " + t.Title + " "
		switch {
		case t.Active:
			label = theme.ActiveTab.Render("▸" + label)
		case t.Plane == pane.Bus:
			label = theme.BusTab.Render(" " + label)
		default:
			label = theme.InactiveTab.Render(" " + label)
		}
		if i > 0 {
			parts = append(parts, theme.Rule.Render("│"))
		}
		parts = append(parts, label)
	}
	return Clip(lipgloss.JoinHorizontal(lipgloss.Top, parts...), width)
}

// Rule is the horizontal separator drawn under the tab bar.
func Rule(width int) string {
	if width <= 0 {
		return ""
	}
	return theme.Rule.Render(strings.Repeat("─", width))
}

// StatusBar renders left-aligned state and right-aligned key hints, clipped to
// width. When the two cannot both fit, the HINTS are dropped and the state is
// kept: the state is what the operator cannot reconstruct, and the hints are
// one "?" away.
func StatusBar(state string, width int) string {
	if width <= 0 {
		return ""
	}
	hints := strings.Join(keymap.StatusHints(), "  ")
	left := theme.StatusBar.Render(state)
	right := theme.StatusBar.Render(hints)

	lw, rw := lipgloss.Width(left), lipgloss.Width(right)
	if lw+rw+1 > width {
		return Clip(left, width)
	}
	return left + strings.Repeat(" ", width-lw-rw) + right
}

// Clip truncates a single line to at most width display cells.
//
// It measures with lipgloss.Width, which accounts for ANSI escapes and wide
// runes — len() would count escape bytes as visible and cut a styled string
// mid-sequence, which leaks the escape into the terminal and corrupts every
// line after it.
func Clip(line string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(line) <= width {
		return line
	}
	// Truncate by display cells, keeping the string valid UTF-8. lipgloss has no
	// public truncate, so this walks runes and re-measures; lines are terminal
	// width, so the cost is bounded and small.
	var b strings.Builder
	for _, r := range line {
		next := b.String() + string(r)
		if lipgloss.Width(next) > width {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Frame lays the shell out: tab bar, rule, body clipped to bodyHeight, status
// bar. It is the ONE place the vertical budget is spent, so a pane cannot push
// the status bar off screen by returning too many lines.
func Frame(tabBar, body, statusBar string, width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	lines := make([]string, 0, height)
	lines = append(lines, Clip(tabBar, width))
	if height > 1 {
		lines = append(lines, Rule(width))
	}

	// Reserve: tab bar (1), rule (1), status bar (1).
	bodyHeight := height - 3
	if bodyHeight < 0 {
		bodyHeight = 0
	}
	bodyLines := []string{}
	if body != "" {
		bodyLines = strings.Split(body, "\n")
	}
	for i := 0; i < bodyHeight; i++ {
		if i < len(bodyLines) {
			lines = append(lines, Clip(bodyLines[i], width))
			continue
		}
		lines = append(lines, "")
	}
	if height > 2 {
		lines = append(lines, Clip(statusBar, width))
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

// Wrap breaks text to width on rune boundaries, for pane bodies that hold
// free-form output. Existing newlines are preserved.
func Wrap(text string, width int) []string {
	if width <= 0 {
		return nil
	}
	var out []string
	for _, para := range strings.Split(text, "\n") {
		if para == "" {
			out = append(out, "")
			continue
		}
		line := ""
		for _, r := range para {
			if utf8.RuneCountInString(line) >= width {
				out = append(out, line)
				line = ""
			}
			line += string(r)
		}
		out = append(out, line)
	}
	return out
}
