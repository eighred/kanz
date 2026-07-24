package main

import "time"

// Config is the universe TUI's connection configuration. Every field is a
// connection coordinate — there is no credential beyond the kubeconfig the
// operator already holds (used out-of-band by `kubectl port-forward`) and the
// SSH key path the operator reads from the Add Node form. The TUI dials the
// operator service over that port-forward.
type Config struct {
	// OperatorAddr is the operator.v1 gRPC endpoint, typically a local
	// port-forward target (see the run instructions in Task 8).
	OperatorAddr string
	// PollInterval is how often the TUI re-fetches the estate; <=0 ⇒ 3s.
	PollInterval time.Duration
}
