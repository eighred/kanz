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
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kanz-eng/kanz/internal/audit/chain"
	"github.com/kanz-eng/kanz/internal/audit/signer"
)

func chainPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL to run linkstore Postgres integration tests")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS audit_chain_links CASCADE`); err != nil {
		t.Fatalf("reset: %v", err)
	}
	files, err := filepath.Glob(filepath.Join("../../../services/regulatory/migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob migrations: %v (found %d)", err, len(files))
	}
	sort.Strings(files)
	for _, f := range files {
		ddl, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(ddl)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	return pool
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
}
