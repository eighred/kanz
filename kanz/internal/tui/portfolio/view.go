package portfolio

import (
	"fmt"
	"strings"

	"github.com/eighred/kanz/internal/tui/theme"
	"github.com/eighred/kanz/internal/tui/ui"
)

// View renders positions above risk, each with its own failure line.
//
// THE TWO HALVES DEGRADE INDEPENDENTLY. A risk engine that is down leaves the
// book on screen with a note under it; a tv-sync that is down leaves the risk
// measures. Blanking the screen on either failure would report a partial outage
// as a total one, and an operator watching a book during an incident is exactly
// who must not be told less than is known.
func (m Model) View(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	var b strings.Builder

	if !m.loaded {
		return ui.Clip(theme.Body.Render("reading the book…"), w)
	}

	b.WriteString(theme.Prompt.Render("POSITIONS"))
	b.WriteString("\n")
	b.WriteString(m.positionsBlock(w))
	b.WriteString("\n")
	b.WriteString(theme.Prompt.Render("RISK"))
	b.WriteString("\n")
	b.WriteString(m.riskBlock(w))

	// The age of what is shown, because a pane that polls must say when it last
	// succeeded — a frozen feed and a quiet market look identical otherwise.
	if !m.lastAt.IsZero() {
		b.WriteString("\n")
		b.WriteString(theme.StatusBar.Render(
			fmt.Sprintf("as of %s · r to refresh", m.lastAt.Format("15:04:05"))))
	}

	lines := strings.Split(b.String(), "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	for i, l := range lines {
		lines[i] = ui.Clip(l, w)
	}
	return strings.Join(lines, "\n")
}

func (m Model) positionsBlock(w int) string {
	if m.posErr != nil {
		return theme.Error.Render(ui.Clip("positions unavailable: "+m.posErr.Error(), w))
	}
	if len(m.positions) == 0 {
		// "Flat" and "we could not read it" are different facts and must not
		// share a rendering — the error branch above owns the second one.
		return theme.Body.Render("no open positions")
	}
	rows := []string{fmt.Sprintf("%-16s %-6s %14s %14s %14s %14s",
		"INSTRUMENT", "SIDE", "QTY", "AVG", "REALIZED", "UNREALIZED")}
	for _, p := range m.positions {
		rows = append(rows, fmt.Sprintf("%-16s %-6s %14s %14s %14s %14s",
			p.Instrument, p.Side, p.Qty, p.AvgPrice, p.RealizedPnl, dash(p.UnrealizedPnl)))
	}
	return theme.Body.Render(strings.Join(rows, "\n"))
}

func (m Model) riskBlock(w int) string {
	if m.riskErr != nil {
		return theme.Error.Render(ui.Clip("risk unavailable: "+m.riskErr.Error(), w))
	}
	if len(m.measures) == 0 {
		return theme.Body.Render("no risk measures computed for this portfolio")
	}
	rows := make([]string, 0, len(m.measures))
	for _, ms := range m.measures {
		line := fmt.Sprintf("%-24s %18s", ms.Name, ms.Value)
		if ms.Unusable {
			// Rendered in the error style rather than dropped: a risk figure that
			// silently vanishes reads as "not computed", which is a much calmer
			// fact than "computed, and this display cannot represent it".
			rows = append(rows, theme.Error.Render(line))
			continue
		}
		rows = append(rows, theme.Body.Render(line))
	}
	return strings.Join(rows, "\n")
}

// dash renders an absent optional value as "—" rather than an empty column.
// UnrealizedPnl is omitempty on the wire: absent means the projection had no
// mark to value the position against, which is not the same as zero PnL.
func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
