package main

import "time"

// Config is the universe TUI's read configuration. Every field is a read
// coordinate — there is no write path, no credential beyond the kubeconfig the
// operator already holds (used out-of-band by `kubectl port-forward`), and no
// mutation anywhere. The TUI dials the operator service over that port-forward.
type Config struct {
	// OperatorAddr is the operator.v1 gRPC endpoint, typically a local
	// port-forward target (see the run instructions in Task 8).
	OperatorAddr string
	// PollInterval is how often the TUI re-fetches the estate; <=0 ⇒ 3s.
	PollInterval time.Duration
}
