package chain

import "testing"

// link is a minimal chain.Link for testing the pure chain logic.
type link struct {
	prev, hash string
	canon      []byte
}

func (l link) PrevHash() string  { return l.prev }
func (l link) Hash() string      { return l.hash }
func (l link) Canonical() []byte { return l.canon }

// build constructs a valid chain over the given canonical payloads.
func build(payloads ...[]byte) []Link {
	var out []Link
	prev := Genesis
	for _, p := range payloads {
		h := Next(prev, p)
		out = append(out, link{prev: prev, hash: h, canon: p})
		prev = h
	}
	return out
}

func TestNextDeterministicAndChained(t *testing.T) {
	a := Next(Genesis, []byte("one"))
	if a != Next(Genesis, []byte("one")) {
		t.Fatal("Next not deterministic")
	}
	// Folding in the predecessor must change the hash for identical content.
	if Next(Genesis, []byte("x")) == Next(a, []byte("x")) {
		t.Fatal("Next ignores prevHash — chain would not bind position")
	}
}

func TestVerifyIntactChain(t *testing.T) {
	links := build([]byte("a"), []byte("b"), []byte("c"))
	if idx, err := Verify(links); err != nil {
		t.Fatalf("intact chain failed verify at %d: %v", idx, err)
	}
	if idx, err := Verify(nil); err != nil || idx != -1 {
		t.Fatalf("empty chain: idx=%d err=%v", idx, err)
	}
}

func TestVerifyDetectsContentTamper(t *testing.T) {
	links := build([]byte("a"), []byte("b"), []byte("c"))
	// Rewrite record 1's content without recomputing its hash — the classic
	// silent edit. Its stored hash no longer matches H(prev||content).
	links[1] = link{prev: links[1].PrevHash(), hash: links[1].Hash(), canon: []byte("FORGED")}
	idx, err := Verify(links)
	if err == nil {
		t.Fatal("tamper not detected")
	}
	if idx != 1 {
		t.Fatalf("tamper located at %d, want 1", idx)
	}
}

func TestVerifyDetectsReorder(t *testing.T) {
	links := build([]byte("a"), []byte("b"), []byte("c"))
	links[1], links[2] = links[2], links[1] // swap → prev-hash linkage breaks
	if _, err := Verify(links); err == nil {
		t.Fatal("reorder not detected")
	}
}
