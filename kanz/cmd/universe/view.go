package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var (
	styleReady  = lipgloss.NewStyle().Foreground(lipgloss.Color("2")) // green
	styleNotRdy = lipgloss.NewStyle().Foreground(lipgloss.Color("1")) // red
	styleTitle  = lipgloss.NewStyle().Bold(true)
	styleErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// render is the pure function of model that produces the whole frame — no I/O,
// no clock (Age is precomputed in the poller), so view_test.go calls it
// directly against a hand-built model.
func (m model) render() string {
	width := m.width
	if width <= 0 {
		width = 80
	}

	var body string
	switch m.active {
	case paneClusters:
		body = m.renderClusters()
	default:
		body = m.renderNodes()
	}

	return body + "\n" + m.renderStatus(width)
}

func (m model) renderNodes() string {
	var b strings.Builder
	b.WriteString(styleTitle.Render("NODES") + "\n")
	b.WriteString(fmt.Sprintf("%-16s %-9s %-16s %-10s %-10s %-6s\n",
		"NAME", "STATUS", "ROLES", "REGION", "VERSION", "AGE"))
	if len(m.nodes) == 0 {
		b.WriteString(styleDim.Render("(no nodes)") + "\n")
		return b.String()
	}
	for _, n := range m.nodes {
		st := n.Status
		if n.Status == "Ready" {
			st = styleReady.Render(n.Status)
		} else {
			st = styleNotRdy.Render(n.Status)
		}
		// Colour escapes occupy no display cells, so pad the raw value and
		// substitute the coloured span after — keeps the columns aligned.
		row := fmt.Sprintf("%-16s %-9s %-16s %-10s %-10s %-6s",
			n.Name, n.Status, n.Roles, n.Region, n.Version, n.Age)
		row = strings.Replace(row, n.Status, st, 1)
		b.WriteString(row + "\n")
	}
	return b.String()
}

func (m model) renderClusters() string {
	var b strings.Builder
	b.WriteString(styleTitle.Render("CLUSTERS") + "\n")
	b.WriteString(fmt.Sprintf("%-14s %-8s %-8s\n", "REGION", "ONLINE", "OFFLINE"))
	if len(m.clusters) == 0 {
		b.WriteString(styleDim.Render("(no clusters)") + "\n")
		return b.String()
	}
	for _, c := range m.clusters {
		b.WriteString(fmt.Sprintf("%-14s %-8d %-8d\n", c.Region, c.Online, c.Offline))
	}
	return b.String()
}

func (m model) renderStatus(width int) string {
	left := styleDim.Render("[tab] switch pane   [q] quit")
	if m.err != nil {
		return left + "   " + styleErr.Render("error: "+m.err.Error())
	}
	return left
}
