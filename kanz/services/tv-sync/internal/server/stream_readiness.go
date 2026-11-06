package server

import (
	"errors"
	"github.com/eighred/kanz/services/tv-sync/internal/brokerapi"
)

type streamGeneration struct {
	done   chan struct{}
	closed bool
}
type servingLease struct {
	r          *Readiness
	generation *streamGeneration
}

func (l *servingLease) Done() <-chan struct{} { return l.generation.done }
func (l *servingLease) Write(write func() error) error {
	l.r.streamMu.Lock()
	defer l.r.streamMu.Unlock()
	if l.generation.closed || l.r.generation != l.generation || !l.r.Ready() {
		return errors.New("tv-sync: serving generation revoked")
	}
	return write()
}
func (r *Readiness) AcquireStream() (brokerapi.StreamLease, bool) {
	r.streamMu.Lock()
	defer r.streamMu.Unlock()
	if !r.Ready() {
		return nil, false
	}
	if r.generation == nil || r.generation.closed {
		r.generation = &streamGeneration{done: make(chan struct{})}
	}
	return &servingLease{r: r, generation: r.generation}, true
}
func (r *Readiness) revokeStreams() {
	r.streamMu.Lock()
	defer r.streamMu.Unlock()
	if r.generation != nil && !r.generation.closed {
		r.generation.closed = true
		close(r.generation.done)
	}
}
