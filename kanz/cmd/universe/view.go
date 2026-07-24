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
	var body string
	switch m.active {
	case paneClusters:
		body = m.renderClusters()
	default:
		body = m.renderNodes()
	}

	return body + "\n" + m.renderStatus()
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
		// Pad the status to its column width as plain text, THEN colour the
		// padded cell and place it directly. Styling a pre-padded cell (rather
		// than a post-hoc strings.Replace on the formatted row) keeps the columns
		// aligned — colour escapes have zero display width — and cannot mis-target
		// a Name/Region cell that happens to contain the status token.
		statusCell := fmt.Sprintf("%-9s", n.Status)
		if n.Status == "Ready" {
			statusCell = styleReady.Render(statusCell)
		} else {
			statusCell = styleNotRdy.Render(statusCell)
		}
		b.WriteString(fmt.Sprintf("%-16s %s %-16s %-10s %-10s %-6s\n",
			n.Name, statusCell, n.Roles, n.Region, n.Version, n.Age))
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

func (m model) renderStatus() string {
	left := styleDim.Render("[tab] switch pane   [q] quit")
	if m.err != nil {
		return left + "   " + styleErr.Render("error: "+m.err.Error())
	}
	return left
}
