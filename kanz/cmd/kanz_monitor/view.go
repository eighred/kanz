package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Lip Gloss styles for the feed and counters. These are colour-only —
// layout (widths, borders) is computed per-render from m.width/m.height so
// the frame always adapts, never these package-level styles.
var (
	styleFilled   = lipgloss.NewStyle().Foreground(lipgloss.Color("2")) // green
	styleRejected = lipgloss.NewStyle().Foreground(lipgloss.Color("1")) // red
	styleIncident = lipgloss.NewStyle().Foreground(lipgloss.Color("1")) // red, non-zero incidents
	styleOK       = lipgloss.NewStyle().Foreground(lipgloss.Color("2")) // green ✓
	styleBad      = lipgloss.NewStyle().Foreground(lipgloss.Color("1")) // red ✗
	styleBoxTitle = lipgloss.NewStyle().Bold(true)
	styleErr      = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleDim      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// narrowWidth is the terminal width below which the feed/counters degrade
// to plain (uncoloured) text. Colour escapes cost no display cells, but at
// very small widths the render favours legibility of the raw text over
// colour, and the box borders themselves eat into the little space there
// is.
const narrowWidth = 60

// render is the pure function of model that produces the whole frame. It
// performs no I/O and reads no clock — every value it needs (including
// "now") is already in m — so view_test.go can call it directly against a
// hand-built model, no TTY required.
func (m model) render() string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	height := m.height
	if height <= 0 {
		height = 24
	}
	narrow := width < narrowWidth

	// Reserve one line for the status bar; the rest is split into the
	// two-column body.
	bodyHeight := height - 1
	if bodyHeight < 1 {
		bodyHeight = 1
	}

	rightWidth := width / 3
	if rightWidth < 20 {
		rightWidth = width / 2
	}
	if rightWidth < 1 {
		rightWidth = 1
	}
	leftWidth := width - rightWidth
	if leftWidth < 1 {
		leftWidth = 1
	}
	// Guard the pathological case (very small width) where the two columns
	// no longer sum correctly after clamping — shrink the right column
	// rather than let the row exceed m.width.
	if leftWidth+rightWidth > width {
		rightWidth = width - leftWidth
		if rightWidth < 0 {
			rightWidth = 0
		}
	}

	left := renderFeed(m.events, leftWidth, bodyHeight, narrow)

	bookBox := renderBook(m.book, rightWidth, narrow)
	countersBox := renderCounters(m.counters, m.events, rightWidth, narrow)
	healthBox := renderHealth(m.health, rightWidth, narrow)
	right := lipgloss.JoinVertical(lipgloss.Left, bookBox, countersBox, healthBox)
	right = clampWidth(right, rightWidth)

	body := lipgloss.JoinHorizontal(lipgloss.Top, clampWidth(left, leftWidth), right)
	body = clampWidth(body, width)

	status := renderStatus(m, width)

	return body + "\n" + status
}

// renderFeed renders the EXECUTION feed: the last N events, newest at the
// bottom, one line each as "HH:MM:SS  event_type  order_id  detail".
func renderFeed(events []lifecycleEvent, width, height int, narrow bool) string {
	title := styleBoxTitle.Render("EXECUTION FEED")
	if narrow {
		title = "EXECUTION FEED"
	}

	maxRows := height - 2 // title + spacing
	if maxRows < 1 {
		maxRows = 1
	}
	shown := events
	if len(shown) > maxRows {
		shown = shown[len(shown)-maxRows:]
	}

	lines := make([]string, 0, len(shown)+1)
	lines = append(lines, title)
	for _, ev := range shown {
		lines = append(lines, feedLine(ev, width, narrow))
	}
	if len(shown) == 0 {
		lines = append(lines, styleDim.Render("(no events yet)"))
	}
	return clampWidth(strings.Join(lines, "\n"), width)
}

// feedLine renders one lifecycle event as a single truncated-to-width line,
// coloured by event type unless narrow degrades it to plain text.
func feedLine(ev lifecycleEvent, width int, narrow bool) string {
	ts := ev.At.Format("15:04:05")
	line := fmt.Sprintf("%s  %s  %s  %s", ts, ev.Type, ev.OrderID, ev.Detail)
	line = truncateWidth(line, width)
	if narrow {
		return line
	}
	switch ev.Type {
	case eventTypeFilled:
		return styleFilled.Render(line)
	case eventTypeRejected:
		return styleRejected.Render(line)
	default:
		return line
	}
}

// renderBook renders the position book: one row per m.book entry as
// "instrument  qty  avg". The quantity/avg strings are the exact wire
// decimals — printed verbatim, never parsed to float.
func renderBook(book map[string]position, width int, narrow bool) string {
	title := "BOOK"
	if !narrow {
		title = styleBoxTitle.Render(title)
	}
	lines := []string{title}

	keys := make([]string, 0, len(book))
	for k := range book {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if len(keys) == 0 {
		lines = append(lines, styleDim.Render("(no positions)"))
	}
	for _, k := range keys {
		p := book[k]
		row := fmt.Sprintf("%s  %s  %s", p.Instrument, p.Quantity, p.AvgPrice)
		lines = append(lines, truncateWidth(row, width))
	}
	return clampWidth(strings.Join(lines, "\n"), width)
}

// renderCounters renders the five real incident counters from m.counters
// plus the two DERIVED counters (Filled, Rejected) computed by counting
// m.events — the poller never scrapes those two (see poller.go), so this
// is the only place they are produced.
func renderCounters(c counterSnapshot, events []lifecycleEvent, width int, narrow bool) string {
	title := "COUNTERS"
	if !narrow {
		title = styleBoxTitle.Render(title)
	}

	var filled, rejected int
	for _, ev := range events {
		switch ev.Type {
		case eventTypeFilled:
			filled++
		case eventTypeRejected:
			rejected++
		}
	}

	rows := []struct {
		label string
		value int
	}{
		{"Filled", filled},
		{"Rejected", rejected},
		{"Quarantined", c.Quarantined},
		{"Ungoverned", c.Ungoverned},
		{"Unpriced", c.Unpriced},
		{"SharedCollateral", c.SharedCollateral},
		{"UnverifiedVenueAcct", c.UnverifiedVenueAccount},
	}

	lines := []string{title}
	for _, r := range rows {
		row := fmt.Sprintf("%-20s %d", r.label, r.value)
		row = truncateWidth(row, width)
		if !narrow && r.value != 0 {
			row = styleIncident.Render(row)
		}
		lines = append(lines, row)
	}
	return clampWidth(strings.Join(lines, "\n"), width)
}

// renderHealth renders ✓/✗ for whatever keys m.health actually holds.
// Deliberately NOT a fixed service list: a key never polled must never
// appear here, since printing ✗ for it would misread as "that service is
// down" when it only means "not wired" (see model.go's health field doc).
func renderHealth(health map[string]bool, width int, narrow bool) string {
	title := "HEALTH"
	if !narrow {
		title = styleBoxTitle.Render(title)
	}
	lines := []string{title}

	keys := make([]string, 0, len(health))
	for k := range health {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if len(keys) == 0 {
		lines = append(lines, styleDim.Render("(none polled)"))
	}
	for _, k := range keys {
		mark := "✗"
		style := styleBad
		if health[k] {
			mark = "✓"
			style = styleOK
		}
		// Truncate the PLAIN row first, then colour only the mark — colouring
		// before truncating risks slicing a rune out of the middle of an
		// ANSI escape sequence.
		row := truncateWidth(fmt.Sprintf("%s %s", mark, k), width)
		if !narrow && strings.HasPrefix(row, mark) {
			row = style.Render(mark) + strings.TrimPrefix(row, mark)
		}
		lines = append(lines, row)
	}
	return clampWidth(strings.Join(lines, "\n"), width)
}

// renderStatus renders the single bottom status line: tenant, spine URL,
// connection state (via m.err), and the quit hint.
func renderStatus(m model, width int) string {
	state := "connected"
	if m.err != nil {
		state = "error: " + m.err.Error()
	}
	line := fmt.Sprintf("tenant=%s  spine=%s  %s  |  q to quit", m.cfg.Tenant, m.cfg.NATSURL, state)
	line = truncateWidth(line, width)
	if m.err != nil {
		return styleErr.Render(line)
	}
	return line
}

// truncateWidth trims s to at most width display cells (via lipgloss.Width,
// which counts cells not bytes — required for multi-byte glyphs like ✓/✗
// and any colour escapes already applied). Enforces the horizontal-overflow
// guard at the single-line level.
func truncateWidth(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	// Trim rune-by-rune (not byte-by-byte) until it fits, leaving room for
	// an ellipsis marker.
	runes := []rune(s)
	for len(runes) > 0 && lipgloss.Width(string(runes)) > width-1 {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}

// clampWidth applies truncateWidth to every line of a (possibly
// multi-line) block, guaranteeing no line in the returned block exceeds
// width — the property view_test.go asserts across the whole frame.
func clampWidth(block string, width int) string {
	lines := strings.Split(block, "\n")
	for i, l := range lines {
		lines[i] = truncateWidth(l, width)
	}
	return strings.Join(lines, "\n")
}
