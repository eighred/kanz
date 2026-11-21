package linkstore

// The chain-fork tests (the fix for regulatory's single-writer constraint).
//
// Extending a hash chain is a check-then-act: read the head, compute
// chain.Next(head, body), insert. Two regulatory pods each doing that against one
// database both read the SAME head and both insert — producing two links with the
// same prev_hash and different hashes. The chain forks into two divergent
// histories, and ON CONFLICT (hash) cannot see it, because the hashes differ.
//
// A regulator asking "show me the unbroken chain of filings" would get two answers.
// That is why this service was pinned to one replica.

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eighred/kanz/internal/audit/chain"
	"github.com/eighred/kanz/internal/audit/signer"
)

func chainPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	// Use the existing owned-schema lifecycle for the complete migration set.
	// Dropping only audit_chain_links leaves filing functions/tables behind and
	// makes the next fixture replay fail before exercising any chain behavior.
	return newPool(t)
}

// TestAppendChainedDoesNotForkUnderConcurrentWriters is the whole point. It
// simulates N regulatory pods filing at the same moment against one database.
func TestAppendChainedDoesNotForkUnderConcurrentWriters(t *testing.T) {
	pool := chainPool(t)
	store := NewPostgres(pool)
	ctx := context.Background()

	const writers = 16
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		links []signer.Link
		start = make(chan struct{})
	)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // collide inside the database
			link, err := store.AppendChained(ctx, []byte{byte(i)})
			if err != nil {
				t.Errorf("append: %v", err)
				return
			}
			mu.Lock()
			links = append(links, link)
			mu.Unlock()
		}(i)
	}
	close(start)
	wg.Wait()

	if len(links) != writers {
		t.Fatalf("appended %d links, want %d", len(links), writers)
	}

	// Every link must have a DISTINCT prev_hash. If two links share one, they
	// chained off the same head — that IS the fork.
	seenPrev := make(map[string]int, writers)
	for _, l := range links {
		seenPrev[l.Prev]++
	}
	for prev, n := range seenPrev {
		if n > 1 {
			t.Fatalf("%d links share prev_hash %s — THE CHAIN FORKED. %d concurrent writers produced %d divergent histories",
				n, prev[:12], writers, n)
		}
	}

	// And the persisted chain must verify as one unbroken line from Genesis.
	persisted, err := store.Links(ctx)
	if err != nil {
		t.Fatalf("links: %v", err)
	}
	if len(persisted) != writers {
		t.Fatalf("persisted %d links, want %d", len(persisted), writers)
	}
	prev := chain.Genesis
	for i, l := range persisted {
		if l.PrevHash() != prev {
			t.Fatalf("link %d chains off %s, but the previous link's hash is %s — the persisted chain is broken",
				i, l.PrevHash()[:12], prev[:12])
		}
		prev = l.Hash()
	}
}

// TestAppendChainedIsIdempotent: the identical body at the identical head is the
// identical link, recorded once.
func TestAppendChainedIsIdempotent(t *testing.T) {
	pool := chainPool(t)
	store := NewPostgres(pool)
	ctx := context.Background()

	first, err := store.AppendChained(ctx, []byte("filing-A"))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// A second, DIFFERENT filing advances the head, so the same body would now
	// chain to a different hash — that is correct, not a duplicate.
	if _, err := store.AppendChained(ctx, []byte("filing-B")); err != nil {
		t.Fatalf("second: %v", err)
	}

	links, err := store.Links(ctx)
	if err != nil {
		t.Fatalf("links: %v", err)
	}
	if len(links) != 2 {
		t.Fatalf("chain has %d links, want 2", len(links))
	}
	if links[0].Hash() != first.Cur {
		t.Fatalf("first link hash changed under us")
	}

	// Repeat the real migration lifecycle in this root, even when run alone.
	// Reset must remove all fixture objects, not redefine a shared tenant helper.
	fresh := NewPostgres(chainPool(t))
	if head, err := fresh.Head(ctx); err != nil || head != "" {
		t.Fatal("repeated fixture did not produce an empty migrated chain")
	}
}
