package brokerapi

// StreamLease binds one already-open response to a serving generation. Write
// authorizes a bounded network write atomically against revocation. Recovery
// never renews an old lease; the client must establish a new stream.
type StreamLease interface {
	Done() <-chan struct{}
	Write(func() error) error
}
type StreamGate interface{ AcquireStream() (StreamLease, bool) }

// SetStreamGate is startup-only wiring from the serving boundary. An unwired
// streaming handler refuses service rather than inventing a healthy default.
func (h *Handler) SetStreamGate(gate StreamGate) { h.streamGate = gate }
