package tui

import "testing"

// The Nodes-pane rows as they actually reach the pty, reproduced byte for byte.
//
// Bubble Tea's standard renderer writes each changed line followed by an
// erase-line-right ("\x1b[K") and "\r\n", and lipgloss wraps the marker glyph — and
// only the marker glyph — in an SGR pair, so the highlighted name is NOT textually
// adjacent to it. Both facts are load-bearing for selectedNodeIn, so they are in the
// fixture rather than in a comment claiming them.
const (
	sel   = "\x1b[1;38;5;6m▸ \x1b[0m"
	unsel = "  "
	eol   = "\x1b[K\r\n"
)

func row(marker, name string) string {
	return marker + name + "         Ready          control-plane    -          v1.31     3d" + eol
}

func TestSelectedNodeInReadsTheCurrentHighlight(t *testing.T) {
	const (
		cp    = "ip-172-26-11-140"
		node2 = "ip-172-26-12-47"
	)
	frame := func(selected string) string {
		out := "NODES" + eol
		for _, n := range []string{cp, node2} {
			if n == selected {
				out += row(sel, n)
			} else {
				out += row(unsel, n)
			}
		}
		return out
	}

	tests := []struct {
		name    string
		capture string
		want    string
		wantOK  bool
	}{
		{"no marker yet", "NODES" + eol, "", false},
		{"first frame highlights the control plane", frame(cp), cp, true},
		// The one that matters: the capture is cumulative, so the earlier frame's
		// "▸ control-plane" line is still in the buffer. A Contains-anywhere matcher
		// would report BOTH nodes as selected; only the last marker line is current.
		{"after moving down, the stale frame must not win", frame(cp) + frame(node2), node2, true},
		{"and moving back up is read as up", frame(cp) + frame(node2) + frame(cp), cp, true},
		// Bubble Tea rewrites only changed lines, so a one-row move emits the row that
		// LOST the marker first and the row that gained it second.
		{"partial repaint of just the two changed rows",
			frame(cp) + row(unsel, cp) + row(sel, node2), node2, true},
		// A chunked read can cut a line in half; a half-written row is not a selection.
		{"truncated trailing row is ignored", frame(cp) + row(unsel, cp) + "\x1b[1;38;5;6m▸ \x1b[0mip-172-26",
			cp, true},
		// A terminal lipgloss judges colourless emits the same glyph unstyled.
		{"uncoloured terminal", "NODES\r\n" + row(unsel, cp) + "▸ " + node2 + " Ready\r\n", node2, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := selectedNodeIn(tc.capture)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("selectedNodeIn = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
