package dualcontrol_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dualcontrol"
)

var now = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

func proposal(t *testing.T, proposer string) dualcontrol.Proposal {
	t.Helper()
	p, err := dualcontrol.Propose("prop-1", dualcontrol.ActPricingOverride, "EX1", proposer,
		dualcontrol.Digest("1.23", "vendor is stale"), now, dualcontrol.DefaultTTL)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	return p
}

func digest() string { return dualcontrol.Digest("1.23", "vendor is stale") }

// The act is authorised when a DIFFERENT person approves the SAME payload before
// it expires. If this ever fails, dual control has become an outage.
func TestApprove_ADifferentPersonOnTheSamePayload(t *testing.T) {
	if _, err := proposal(t, "alice@kanz").Approve("bob@kanz", digest(), now.Add(time.Hour)); err != nil {
		t.Fatalf("a valid second signature was refused: %v", err)
	}
}

// THE RULE.
func TestApprove_SelfApprovalIsRefused(t *testing.T) {
	_, err := proposal(t, "alice@kanz").Approve("alice@kanz", digest(), now.Add(time.Hour))
	if !errors.Is(err, dualcontrol.ErrSelfApproval) {
		t.Fatalf("self-approval = %v, want ErrSelfApproval", err)
	}
}

// AND THE RULE SURVIVES THE SPELLINGS OF ONE NAME.
//
// This is the clause that decides whether the control is real. A case-sensitive
// comparison lets one person hold both signatures by capitalising a letter, and
// the audit trail then shows two distinct actors — dual control that reads as
// satisfied in exactly the record an auditor would read.
func TestApprove_OneNameSpelledTwoWaysIsStillOnePerson(t *testing.T) {
	for _, approver := range []string{
		"Alice@kanz",   // capitalised
		"ALICE@KANZ",   // shouted
		" alice@kanz ", // padded
		"\talice@kanz",
	} {
		_, err := proposal(t, "alice@kanz").Approve(approver, digest(), now.Add(time.Hour))
		if !errors.Is(err, dualcontrol.ErrSelfApproval) {
			t.Errorf("%q approving alice@kanz = %v, want ErrSelfApproval — one person holds both "+
				"signatures and the trail shows two actors", approver, err)
		}
	}
	// And symmetrically: it is the PROPOSER side that varies.
	_, err := proposal(t, " ALICE@kanz ").Approve("alice@kanz", digest(), now.Add(time.Hour))
	if !errors.Is(err, dualcontrol.ErrSelfApproval) {
		t.Errorf("proposer spelled differently = %v, want ErrSelfApproval", err)
	}
}

// THE APPROVAL COVERS THE VALUE, NOT MERELY THE REQUEST.
//
// Propose a defensible price, collect the second signature, apply a different
// one. Without this the whole workflow authorises whatever is applied last.
func TestApprove_ThePayloadCannotChangeAfterApproval(t *testing.T) {
	_, err := proposal(t, "alice@kanz").Approve("bob@kanz",
		dualcontrol.Digest("9999.00", "vendor is stale"), now.Add(time.Hour))
	if !errors.Is(err, dualcontrol.ErrPayloadChanged) {
		t.Fatalf("applying a different price than was approved = %v, want ErrPayloadChanged", err)
	}
}

func TestApprove_Expiry(t *testing.T) {
	p := proposal(t, "alice@kanz")
	if _, err := p.Approve("bob@kanz", digest(), p.ExpiresAt.Add(time.Nanosecond)); !errors.Is(err, dualcontrol.ErrExpired) {
		t.Errorf("after expiry = %v, want ErrExpired", err)
	}
	// Inclusive of the instant: AT ExpiresAt it is already expired. A boundary
	// stated one way in the code and another in the test is how an off-by-one
	// survives both.
	if _, err := p.Approve("bob@kanz", digest(), p.ExpiresAt); !errors.Is(err, dualcontrol.ErrExpired) {
		t.Errorf("at exactly ExpiresAt = %v, want ErrExpired", err)
	}
	if _, err := p.Approve("bob@kanz", digest(), p.ExpiresAt.Add(-time.Nanosecond)); err != nil {
		t.Errorf("one nanosecond before expiry = %v, want nil", err)
	}
}

// A ZERO-VALUE PROPOSAL AUTHORISES NOTHING.
//
// It is what a caller gets from a map miss, a failed decode, or a struct built
// by hand around Propose. Every check in Approve would pass vacuously on it: the
// empty approver differs from no proposer, the empty digest matches the empty
// digest, and a zero expiry read as "no deadline" never expires. That is a
// silent full-authorisation, so it is refused explicitly.
func TestApprove_AZeroValueProposalIsRefusedRatherThanVacuouslyPassing(t *testing.T) {
	var empty dualcontrol.Proposal
	_, err := empty.Approve("bob@kanz", "", now)
	if err == nil {
		t.Fatal("a zero-value proposal APPROVED an act — every check passed vacuously")
	}
	if !errors.Is(err, dualcontrol.ErrMalformed) {
		t.Errorf("zero-value proposal = %v, want ErrMalformed", err)
	}
}

func TestApprove_AnUnauthenticatedApproverIsRefused(t *testing.T) {
	for _, approver := range []string{"", "   ", "\t\n"} {
		_, err := proposal(t, "alice@kanz").Approve(approver, digest(), now.Add(time.Hour))
		if !errors.Is(err, dualcontrol.ErrMalformed) {
			t.Errorf("approver %q = %v, want ErrMalformed", approver, err)
		}
	}
}

// Propose refuses a proposal that could never be approved, at the moment the
// operator who could fix it is still there.
func TestPropose_RefusesTheUnapprovable(t *testing.T) {
	d := dualcontrol.Digest("1.23")
	cases := []struct {
		name                             string
		id, subject, proposer, digestStr string
		ttl                              time.Duration
	}{
		{"empty id", "", "EX1", "alice@kanz", d, time.Hour},
		{"empty subject", "p1", "", "alice@kanz", d, time.Hour},
		{"empty proposer", "p1", "EX1", "", d, time.Hour},
		{"blank proposer", "p1", "EX1", "   ", d, time.Hour},
		{"empty digest", "p1", "EX1", "alice@kanz", "", time.Hour},
		{"zero ttl", "p1", "EX1", "alice@kanz", d, 0},
		{"negative ttl", "p1", "EX1", "alice@kanz", d, -time.Hour},
	}
	for _, c := range cases {
		_, err := dualcontrol.Propose(c.id, dualcontrol.ActPricingOverride, c.subject, c.proposer, c.digestStr, now, c.ttl)
		if !errors.Is(err, dualcontrol.ErrMalformed) {
			t.Errorf("%s: Propose = %v, want ErrMalformed", c.name, err)
		}
	}
}

// AN EMPTY PROPOSER IS THE #444 DEFECT ARRIVING AS A VACUOUS PASS. If it were
// allowed, every approver would differ from it and the self-approval check would
// be satisfied by construction.
func TestPropose_AnEmptyProposerWouldMakeEveryApprovalSelfConsistent(t *testing.T) {
	if _, err := dualcontrol.Propose("p1", dualcontrol.ActPricingOverride, "EX1", "", digest(), now, time.Hour); err == nil {
		t.Fatal("a proposal with no proposer was built — any approver would pass the self-approval check")
	}
}

// THE DIGEST IS LENGTH-PREFIXED, NOT JOINED.
//
// Payload fields include free text, so any separator can appear inside one. With
// a joined digest, reason "a|b" + price "c" hashes identically to reason "a" +
// price "b|c" — an approval for one payload authorising a different one, which
// is the exact property the digest exists to provide.
func TestDigest_FieldBoundariesCannotBeForged(t *testing.T) {
	if dualcontrol.Digest("a|b", "c") == dualcontrol.Digest("a", "b|c") {
		t.Error(`Digest("a|b","c") == Digest("a","b|c") — a field separator inside a value shifts ` +
			`the boundary and two different payloads share one approval`)
	}
	for _, sep := range []string{"", ":", "\x00", "\n"} {
		if dualcontrol.Digest("x"+sep+"y", "z") == dualcontrol.Digest("x", sep+"y"+"z") {
			t.Errorf("separator %q collides", sep)
		}
	}
	// Same input, same digest — it has to be deterministic to be re-checkable
	// across two processes, since the approver is rarely on the same replica.
	parts := []string{"1.23", "why"}
	first, second := dualcontrol.Digest(parts...), dualcontrol.Digest(parts[0], parts[1])
	if first != second {
		t.Error("Digest is not deterministic")
	}
	// Field COUNT matters too: one field of "ab" is not two fields "a","b".
	if dualcontrol.Digest("ab") == dualcontrol.Digest("a", "b") {
		t.Error(`Digest("ab") == Digest("a","b")`)
	}
}

func TestPending(t *testing.T) {
	p := proposal(t, "alice@kanz")
	if !p.Pending(now.Add(time.Hour)) {
		t.Error("a fresh proposal is not pending — an unapproved act would be invisible")
	}
	if p.Pending(p.ExpiresAt) {
		t.Error("an expired proposal still reads as pending")
	}
	var empty dualcontrol.Proposal
	if empty.Pending(now) {
		t.Error("a zero-value proposal reads as pending")
	}
}

// The error naming which rule refused is what an operator reads. A refusal that
// says only "denied" sends them to the code.
func TestErrorsNameTheRuleThatRefused(t *testing.T) {
	_, err := proposal(t, "alice@kanz").Approve("alice@kanz", digest(), now.Add(time.Hour))
	if !strings.Contains(err.Error(), "alice@kanz") {
		t.Errorf("self-approval refusal does not name the proposer: %v", err)
	}
}
