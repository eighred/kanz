package main

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// testConnTimeout bounds Test Connection ALONE, and is deliberately two orders of
// magnitude larger than Config.CallTimeout because it is not a read at all: the operator
// runs the reachability dial in an ephemeral Kubernetes Job, so this wait covers Job
// create, pod scheduling, possibly an image pull, and then up to the probe binary's
// own 10s dial (cmd/kanz-provisioner/probe.go).
//
// It must strictly EXCEED the gateway's own budget for this route
// (control.testConnectionTimeout), which must in turn exceed the operator's
// provision.probeTimeout. The deadlines nest outward so the innermost layer — the one
// that knows the probe Job did not answer — is the one that reports it. Invert the
// order and every unreachable target reads as a generic client-side deadline instead,
// which is the same wrong answer for a black-holed host as for a healthy one.
// test/arch asserts the ordering, because these three consts live in three packages.
//
// It is a CONST and not a flag for that reason: an operator lowering --timeout must not
// be able to re-break the chain from the outside (see model.testConnCmd).
const testConnTimeout = 100 * time.Second

// pollTick arms a single tea.Tick that fires after cfg.PollInterval and does
// the gRPC fetch off the UI thread, returning a fetchMsg. Update re-arms it
// after every fetchMsg, so the TUI polls forever at a fixed cadence.
func (m model) pollTick() tea.Cmd {
	return tea.Tick(m.cfg.PollInterval, func(time.Time) tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), m.cfg.CallTimeout)
		defer cancel()
		msg, err := m.src.fetch(ctx)
		if err != nil {
			return fetchMsg{err: err}
		}
		return msg
	})
}
