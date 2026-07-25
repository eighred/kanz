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

	// CallTimeout bounds ONE ordinary control-plane call — the estate poll and the
	// cordon/drain/relabel/add/set-keys actions, all of which the operator answers
	// from state it already holds. <=0 ⇒ defaultCallTimeout.
	//
	// It does NOT bound Test Connection, which waits on a probe Job and has its own,
	// far longer bound (testConnTimeout). Lowering this must not be able to re-break
	// that: see model.testConnCmd.
	CallTimeout time.Duration
}

// defaultCallTimeout is the bound on an ordinary control-plane read when the operator
// sets none. Tight on purpose: every call it covers is answered from cached state, so a
// call that outlives it is a control plane that is not answering, and the TUI is better
// off degrading one tick than hanging the ticker.
const defaultCallTimeout = 5 * time.Second
