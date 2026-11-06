package server

import (
	"crypto/sha256"
	"sync"
	"sync/atomic"
)

const maxUnresolvedFolds = 4096

type foldIdentity struct{ tenant, event [32]byte }
type foldFailures struct {
	mu       sync.Mutex
	pending  map[foldIdentity]struct{}
	overflow bool
	blocked  atomic.Bool
}

// FoldResult never exposes identities in metrics, headers or errors. Only a
// successful retry of the same tenant/event can discharge its failed fold.
// Overflow or absent identity stays closed until a fresh process performs its
// durable catch-up; it cannot be cleared by a later unrelated delivery.
func (r *Readiness) FoldResult(tenant, event string, failed bool) {
	r.foldFailures.record(tenant, event, failed)
}
func (f *foldFailures) record(tenant, event string, failed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if tenant == "" || event == "" {
		f.overflow = true
	}
	key := foldIdentity{sha256.Sum256([]byte(tenant)), sha256.Sum256([]byte(event))}
	if failed {
		if f.pending == nil {
			f.pending = make(map[foldIdentity]struct{})
		}
		if _, exists := f.pending[key]; !exists {
			if len(f.pending) >= maxUnresolvedFolds {
				f.overflow = true
			} else {
				f.pending[key] = struct{}{}
			}
		}
	} else {
		delete(f.pending, key)
	}
	f.blocked.Store(f.overflow || len(f.pending) > 0)
}
