package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var (
	styleReady    = lipgloss.NewStyle().Foreground(lipgloss.Color("2")) // green
	styleNotRdy   = lipgloss.NewStyle().Foreground(lipgloss.Color("1")) // red
	styleDraining = lipgloss.NewStyle().Foreground(lipgloss.Color("3")) // yellow
	styleTitle    = lipgloss.NewStyle().Bold(true)
	styleErr      = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleDim      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleSelected = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
)

// render is the pure function of model that produces the whole frame — no I/O,
// no clock (Age is precomputed in the poller), so view_test.go calls it
// directly against a hand-built model.
func (m model) render() string {
	if m.showForm {
		out := m.form.render()
		if m.testResult != "" {
			out += "\n" + m.testResult
		}
		if m.formErr != nil {
			out += "\n" + styleErr.Render("error: "+m.formErr.Error())
		}
		return out
	}

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
	b.WriteString(fmt.Sprintf("   %-16s %-14s %-16s %-10s %-10s %-6s\n",
		"NAME", "STATUS", "ROLES", "REGION", "VERSION", "AGE"))
	if len(m.nodes) == 0 {
		b.WriteString(styleDim.Render("(no nodes)") + "\n")
		return b.String()
	}
	for i, n := range m.nodes {
		// Pad the status to its column width as plain text, THEN colour the
		// padded cell and place it directly. Styling a pre-padded cell (rather
		// than a post-hoc strings.Replace on the formatted row) keeps the columns
		// aligned — colour escapes have zero display width — and cannot mis-target
		// a Name/Region cell that happens to contain the status token.
		label := nodeStateLabel(n)
		statusCell := fmt.Sprintf("%-14s", label)
		switch {
		case label == "Ready":
			statusCell = styleReady.Render(statusCell)
		case strings.HasPrefix(label, "Draining"):
			statusCell = styleDraining.Render(statusCell)
		default:
			statusCell = styleNotRdy.Render(statusCell)
		}
		marker := "  "
		if i == m.selected {
			marker = styleSelected.Render("▸ ")
		}
		b.WriteString(marker + fmt.Sprintf("%-16s %s %-16s %-10s %-10s %-6s\n",
			n.Name, statusCell, n.Roles, n.Region, n.Version, n.Age))
	}
	if m.confirmingDrain {
		if name, ok := m.selectedNodeName(); ok {
			b.WriteString("\n" + styleErr.Render(
				fmt.Sprintf("Drain %s? evicts %d pods  [y/n]", name, m.nodes[m.selected].evictablePods)))
			b.WriteString("\n")
		}
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
	left := styleDim.Render("[tab] switch pane   [a] add node   [q] quit")
	if m.active == paneNodes {
		left += "\n" + styleDim.Render("[↑↓] select  [c]ordon [u]ncordon [d]rain")
	}
	if p := m.renderProvisions(); p != "" {
		left += "\n" + p
	}
	if m.actionErr != nil {
		left += "\n" + styleErr.Render("action error: "+m.actionErr.Error())
	}
	if m.err != nil {
		return left + "\n" + styleErr.Render("error: "+m.err.Error())
	}
	return left
}

// renderProvisions is a one-line strip summarising in-flight node
// provisioning (e.g. "provisioning: london=Installing tokyo=Failed"). It is
// empty when there is nothing in flight, so it adds no blank lines to the
// steady-state frame.
func (m model) renderProvisions() string {
	if len(m.provisions) == 0 {
		return ""
	}
	parts := make([]string, 0, len(m.provisions))
	for _, p := range m.provisions {
		parts = append(parts, p.hostname+"="+p.status)
	}
	return styleDim.Render("provisioning: " + strings.Join(parts, " "))
}
