package main

import "time"

// Config is the universe TUI's runtime configuration.
//
// It holds NO CONNECTION COORDINATE and no credential. Both used to live here:
// OperatorAddr pointed at a `kubectl port-forward` target, which meant the TUI's
// access boundary was the operator's kubeconfig. Reaching the estate is now the
// gatewaySource's business (see gatewaysource.go) and the credential is a bearer
// token the gateway validates, so what remains here is presentation only.
type Config struct {
	// PollInterval is how often the TUI re-fetches the estate; <=0 ⇒ 3s.
	PollInterval time.Duration
}
