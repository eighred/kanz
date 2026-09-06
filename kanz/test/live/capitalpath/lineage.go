package main

import (
	"errors"
	"sync/atomic"
)

type factKind uint32

const (
	factAccepted factKind = 1 << iota
	factRouted
	factFilled
	factAccounted
)

var (
	errMissingAccepted  = errors.New("capitalpath: missing ORDER_ACCEPTED fact")
	errMissingRouted    = errors.New("capitalpath: missing ORDER_ROUTED fact")
	errMissingFill      = errors.New("capitalpath: missing ORDER_FILLED fact")
	errMissingAccounted = errors.New("capitalpath: missing accounting post-commit fact")
)

// lineage is the lock-free correlation core shared by the independent FACT
// subscriptions. IDs and payloads stay outside it; its only job is exactly-once
// observation of a fixed, compile-time vocabulary.
type lineage struct {
	seen atomic.Uint32
}

func (l *lineage) observe(kind factKind) bool {
	bit := uint32(kind)
	for {
		before := l.seen.Load()
		if before&bit != 0 {
			return true
		}
		if l.seen.CompareAndSwap(before, before|bit) {
			return false
		}
	}
}

func (l *lineage) complete() error {
	seen := l.seen.Load()
	if seen&uint32(factAccepted) == 0 {
		return errMissingAccepted
	}
	if seen&uint32(factRouted) == 0 {
		return errMissingRouted
	}
	if seen&uint32(factFilled) == 0 {
		return errMissingFill
	}
	if seen&uint32(factAccounted) == 0 {
		return errMissingAccounted
	}
	return nil
}

func (l *lineage) reset() { l.seen.Store(0) }
