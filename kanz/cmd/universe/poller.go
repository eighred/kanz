package main

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// pollTimeout bounds each fetch so an unreachable operator degrades the tick
// (err in the status line) rather than hanging the ticker.
const pollTimeout = 5 * time.Second

// pollTick arms a single tea.Tick that fires after cfg.PollInterval and does
// the gRPC fetch off the UI thread, returning a fetchMsg. Update re-arms it
// after every fetchMsg, so the TUI polls forever at a fixed cadence.
func (m model) pollTick() tea.Cmd {
	return tea.Tick(m.cfg.PollInterval, func(time.Time) tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
		defer cancel()
		msg, err := m.src.fetch(ctx)
		if err != nil {
			return fetchMsg{err: err}
		}
		return msg
	})
}
