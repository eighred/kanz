package signer_test

import (
	"errors"
	"testing"

	"github.com/kanz-eng/kanz/internal/audit/chain"
	"github.com/kanz-eng/kanz/internal/audit/signer"
)

// The signed reports form a valid audit chain: each signature chains from the
// prior one, and chain.Verify over the emitted links is intact — the property a
// bare SHA-256 content hash (the replaced HashSigner) cannot provide.
func TestChainSignerProducesVerifiableChain(t *testing.T) {
	var links []signer.Link
	s := signer.New("", signer.WithSink(func(l signer.Link) error {
		links = append(links, l)
		return nil
	}))

	sig1 := s.Sign([]byte("REG-01 FRTB report as-of 2026-01-01"))
	sig2 := s.Sign([]byte("CLIMATE-01 TCFD report as-of 2026-01-01"))
	sig3 := s.Sign([]byte("REG-01 FormPF report as-of 2026-01-02"))

	if sig1 == sig2 || sig2 == sig3 {
		t.Fatal("distinct reports must produce distinct signatures")
	}
	// The head after signing equals the last signature.
	if s.Head() != sig3 {
		t.Errorf("head = %q want last signature %q", s.Head(), sig3)
	}
	// The signature IS a chain position: sig2 folds in sig1.
	if want := chain.Next(sig1, []byte("CLIMATE-01 TCFD report as-of 2026-01-01")); want != sig2 {
		t.Errorf("sig2 = %q want chain.Next(sig1, body) = %q", sig2, want)
	}

	cl := make([]chain.Link, len(links))
	for i, l := range links {
		cl[i] = l
	}
	if idx, err := chain.Verify(cl); err != nil {
		t.Fatalf("chain.Verify failed at %d: %v", idx, err)
	}
	// The first link chains from Genesis.
	if links[0].PrevHash() != chain.Genesis {
		t.Errorf("first link prev = %q want Genesis", links[0].PrevHash())
	}
}

// Tampering with a signed report's body breaks verification at that link — the
// tamper-evidence the chain signer exists to provide.
func TestTamperBreaksVerification(t *testing.T) {
	var links []signer.Link
	s := signer.New("", signer.WithSink(func(l signer.Link) error {
		links = append(links, l)
		return nil
	}))
	s.Sign([]byte("clean-1"))
	s.Sign([]byte("clean-2"))
	s.Sign([]byte("clean-3"))

	// Forge the middle report's canonical bytes (signature/prev untouched).
	links[1].Body = []byte("forged-2")

	cl := make([]chain.Link, len(links))
	for i, l := range links {
		cl[i] = l
	}
	idx, err := chain.Verify(cl)
	if err == nil {
		t.Fatal("expected verification failure after tampering")
	}
	if idx != 1 {
		t.Errorf("failure index = %d want 1 (the forged link)", idx)
	}
}

// Continuing from an existing audit head keeps the chain unbroken across process
// restarts (the head is the durable audit-store state at boot).
func TestContinueFromExistingHead(t *testing.T) {
	first := signer.New("")
	h := first.Sign([]byte("before-restart"))

	// New process resumes from the persisted head.
	var links []signer.Link
	resumed := signer.New(h, signer.WithSink(func(l signer.Link) error {
		links = append(links, l)
		return nil
	}))
	sig := resumed.Sign([]byte("after-restart"))

	if links[0].PrevHash() != h {
		t.Errorf("resumed first link prev = %q want persisted head %q", links[0].PrevHash(), h)
	}
	if sig != chain.Next(h, []byte("after-restart")) {
		t.Error("resumed signature does not chain from the persisted head")
	}
}

// A sink error is surfaced via the hook but does not fail signing (the in-memory
// chain stays consistent so the report is still produced).
func TestSinkErrorReportedNotFatal(t *testing.T) {
	var gotErr error
	s := signer.New("",
		signer.WithSink(func(signer.Link) error { return errors.New("store down") }),
		signer.WithErrorHandler(func(err error) { gotErr = err }))

	if sig := s.Sign([]byte("x")); sig == "" {
		t.Fatal("signature must still be returned on a sink error")
	}
	if gotErr == nil {
		t.Error("sink error not reported to the hook")
	}
}
