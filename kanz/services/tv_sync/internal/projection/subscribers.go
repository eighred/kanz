package projection

import "sync"

// subscribers is the streaming fan-out registry: each SSE client gets a buffered
// channel of Deltas for one (tenant, account). Delivery is best-effort — a slow
// client whose buffer is full drops the delta rather than blocking the fold path
// (the client re-reads current state via REST on reconnect).
type subscribers struct {
	mu   sync.Mutex
	next int
	subs map[int]*sub
}

type sub struct {
	tenant  string
	account string
	ch      chan Delta
}

func newSubscribers() *subscribers { return &subscribers{subs: make(map[int]*sub)} }

// subscribe registers a client and returns its delta channel + an unregister.
func (s *subscribers) subscribe(tenant, account string) (<-chan Delta, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.next
	s.next++
	ch := make(chan Delta, 64)
	s.subs[id] = &sub{tenant: tenant, account: account, ch: ch}
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if sb, ok := s.subs[id]; ok {
			delete(s.subs, id)
			close(sb.ch)
		}
	}
}

// publish delivers d to every subscriber watching (tenant, d.Account).
func (s *subscribers) publish(tenant string, d Delta) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sb := range s.subs {
		if sb.tenant != tenant || sb.account != d.Account {
			continue
		}
		select {
		case sb.ch <- d:
		default: // full — drop; the client resyncs via REST
		}
	}
}
