package signer_test

import (
	"context"
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

	sig1, _ := s.Sign([]byte("REG-01 FRTB report as-of 2026-01-01"))
	sig2, _ := s.Sign([]byte("CLIMATE-01 TCFD report as-of 2026-01-01"))
	sig3, _ := s.Sign([]byte("REG-01 FormPF report as-of 2026-01-02"))

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
	_, _ = s.Sign([]byte("clean-1"))
	_, _ = s.Sign([]byte("clean-2")) // driving the chain forward; the value is asserted below
	_, _ = s.Sign([]byte("clean-3"))

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
	h, _ := first.Sign([]byte("before-restart"))

	// New process resumes from the persisted head.
	var links []signer.Link
	resumed := signer.New(h, signer.WithSink(func(l signer.Link) error {
		links = append(links, l)
		return nil
	}))
	sig, _ := resumed.Sign([]byte("after-restart"))

	if links[0].PrevHash() != h {
		t.Errorf("resumed first link prev = %q want persisted head %q", links[0].PrevHash(), h)
	}
	if sig != chain.Next(h, []byte("after-restart")) {
		t.Error("resumed signature does not chain from the persisted head")
	}
}

// A sink error is FATAL to the signature. This test used to assert the opposite —
// "signature must still be returned on a sink error" — which is how a regulatory
// filing could be issued carrying a chain position that exists in no durable chain.
// The old contract encoded the bug; this one encodes the fix.
func TestSinkErrorIsFatalToTheSignature(t *testing.T) {
	var gotErr error
	s := signer.New("",
		signer.WithSink(func(signer.Link) error { return errors.New("store down") }),
		signer.WithErrorHandler(func(err error) { gotErr = err }))

	sig, err := s.Sign([]byte("x"))
	if err == nil {
		t.Fatal("Sign succeeded although the link was never recorded — the filing would claim a chain position that does not exist")
	}
	if sig != "" {
		t.Fatalf("Sign returned signature %q with an error; callers would use it", sig)
	}
	if gotErr == nil {
		t.Error("sink error not reported to the hook — a failed audit-chain append must alert as well as fail")
	}
}

// --- a filing that cannot be recorded must not be signed ---

type failingChainer struct{ err error }

func (f *failingChainer) AppendChained(context.Context, []byte) (signer.Link, error) {
	return signer.Link{}, f.err
}

// TestSignRefusesWhenTheChainLinkCannotLand pins the defect that shipped: Sign
// logged a failed durable append and RETURNED THE SIGNATURE ANYWAY ("the signature
// is still returned and the head still advances"). A regulatory filing could be
// issued carrying a chain position that exists in no durable chain — the chain has
// a hole, and the filing claims a place in it.
func TestSignRefusesWhenTheChainLinkCannotLand(t *testing.T) {
	var alerted error
	s := signer.New("",
		signer.WithChainer(&failingChainer{err: errors.New("database is down")}),
		signer.WithErrorHandler(func(err error) { alerted = err }),
	)

	sig, err := s.Sign([]byte("a filing that must not be issued"))
	if err == nil {
		t.Fatal("Sign succeeded although the chain link never landed — the filing would claim a chain position that does not exist")
	}
	if sig != "" {
		t.Fatalf("Sign returned signature %q alongside the error — callers would use it", sig)
	}
	if alerted == nil {
		t.Fatal("the error handler was not called: a failed audit-chain append must alert, as well as fail")
	}
}

// The in-process path must refuse identically: a sink failure means the link is
// not durable, so the head must NOT advance and no signature is issued.
func TestSignRefusesWhenTheSinkFails(t *testing.T) {
	s := signer.New("", signer.WithSink(func(signer.Link) error { return errors.New("append failed") }))

	before := s.Head()
	sig, err := s.Sign([]byte("filing"))
	if err == nil {
		t.Fatal("Sign succeeded although the sink rejected the link")
	}
	if sig != "" {
		t.Fatalf("Sign returned %q with an error", sig)
	}
	if s.Head() != before {
		t.Fatal("the chain head ADVANCED past a link that was never recorded — the next filing would chain off a hole")
	}
}
