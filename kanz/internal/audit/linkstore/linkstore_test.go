package linkstore

import (
	"context"
	"testing"

	"github.com/kanz-eng/kanz/internal/audit/chain"
	"github.com/kanz-eng/kanz/internal/audit/signer"
)

func asChainLinks(ls []signer.Link) []chain.Link {
	out := make([]chain.Link, len(ls))
	for i, l := range ls {
		out[i] = l
	}
	return out
}

// newSigner builds a ChainSigner that recovers its head from the store and
// appends every link back to it — the production wiring (buildSigner) in
// miniature.
func newSigner(t *testing.T, store Store) *signer.ChainSigner {
	t.Helper()
	head, err := store.Head(context.Background())
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	return signer.New(head, signer.WithSink(func(l signer.Link) error {
		return store.Append(context.Background(), l)
	}))
}

// driveRestartContinuity is the store contract REG-02 exists to satisfy: a
// chain signed across a simulated restart (a fresh signer recovering the head
// from the store) is one unbroken, walk-verifiable sequence — and tampering
// with any persisted link is detectable.
func driveRestartContinuity(t *testing.T, store Store) {
	t.Helper()
	ctx := context.Background()

	if head, err := store.Head(ctx); err != nil || head != "" {
		t.Fatalf("empty store Head = (%q, %v), want (\"\", nil)", head, err)
	}

	s1 := newSigner(t, store)
	s1.Sign([]byte("filing-A"))
	s1.Sign([]byte("filing-B"))
	preRestartHead := s1.Head()

	// Restart: a new signer must resume from the persisted head, not Genesis.
	s2 := newSigner(t, store)
	if s2.Head() != preRestartHead {
		t.Fatalf("head not recovered across restart: got %q want %q", s2.Head(), preRestartHead)
	}
	s2.Sign([]byte("filing-C"))

	links, err := store.Links(ctx)
	if err != nil {
		t.Fatalf("Links: %v", err)
	}
	if len(links) != 3 {
		t.Fatalf("persisted %d links, want 3", len(links))
	}
	if idx, err := chain.Verify(asChainLinks(links)); err != nil {
		t.Fatalf("persisted chain broke at index %d: %v", idx, err)
	}

	// Tamper: mutate a stored link's canonical bytes and verification must catch
	// it — the durability is worthless if it isn't tamper-evident.
	links[1].Body = []byte("forged")
	if idx, err := chain.Verify(asChainLinks(links)); err == nil {
		t.Fatal("tampered chain verified clean, want a failure")
	} else if idx != 1 {
		t.Fatalf("tamper detected at index %d, want 1", idx)
	}
}

func TestMemory_RestartContinuity(t *testing.T) {
	driveRestartContinuity(t, NewMemoryStore())
}

func TestMemory_AppendIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	link := signer.Link{Prev: "genesis", Cur: "abc123", Body: []byte("x")}
	if err := store.Append(ctx, link); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := store.Append(ctx, link); err != nil { // same hash again
		t.Fatalf("Append (dup): %v", err)
	}
	links, _ := store.Links(ctx)
	if len(links) != 1 {
		t.Fatalf("idempotent append kept %d links, want 1", len(links))
	}
	if head, _ := store.Head(ctx); head != "abc123" {
		t.Fatalf("Head = %q, want abc123", head)
	}
}

func TestMemory_RejectsEmptyHash(t *testing.T) {
	if err := NewMemoryStore().Append(context.Background(), signer.Link{Cur: ""}); err == nil {
		t.Fatal("Append accepted an empty-hash link, want an error")
	}
}

func TestMemory_LinksIsACopy(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	_ = store.Append(ctx, signer.Link{Cur: "h1", Body: []byte("a")})
	links, _ := store.Links(ctx)
	links[0].Cur = "mutated" // must not affect the store's own slice
	again, _ := store.Links(ctx)
	if again[0].Cur != "h1" {
		t.Fatal("Links returned an aliased slice; mutation leaked into the store")
	}
}
