// Package dualcontrol is the maker-checker rule: an act that moves capital or
// relaxes a control takes two different people (#410).
//
// # Why this is a package and not four lines in a handler
//
// #410 names three acts that need it — a pricing/compliance override, a mandate
// change, and an order above a notional. The rule is one sentence, which is
// exactly why it would get retyped at each of the three sites, and this
// repository has already paid that bill once: 17 services each had their own
// secret() and 15 were wrong. "The approver must not be the proposer" is a
// clause that ROTS INTO A COMMENT, so it lives in one place with the tests that
// prove it, and each act supplies its own Act constant and payload.
//
// # What makes dual control real rather than ceremonial
//
// Three properties, and skipping any one of them leaves a workflow that LOOKS
// like four-eyes in the audit trail while one person still holds the outcome:
//
//  1. THE APPROVER IS NOT THE PROPOSER. Compared after normalising case and
//     surrounding space, because "Alice@kanz" approving "alice@kanz" is one
//     person and a case-sensitive comparison would call it two.
//
//  2. THE APPROVAL IS BOUND TO WHAT WAS PROPOSED. An approval that names only a
//     proposal id lets the payload change after the second signature: propose a
//     defensible price, collect the approval, apply a different one. The digest
//     of the exact payload is carried through the approval and re-checked, so
//     the approver's signature covers the VALUE and not merely the request.
//
//  3. A PENDING PROPOSAL EXPIRES. Without a deadline an approval collected today
//     can be applied against next quarter's book, and a proposal nobody acted on
//     rests forever looking like a decision that was made.
//
// # What this package does NOT decide
//
// It does not decide WHICH acts require dual control, or above what threshold —
// that is the policy question #410 is blocked on, and it belongs to the caller
// and its configuration. This package answers only "is this approval valid",
// which is the half that must not vary between the three acts.
package dualcontrol

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Act names the kind of act under dual control. It is carried on the proposal so
// an approval for one act can never be replayed against another.
type Act string

const (
	// ActPricingOverride is a human override of a pricing-oversight exception —
	// the first act wired, chosen because it is one endpoint with a durable
	// append-only trail whose actor is authenticated (#444).
	ActPricingOverride Act = "PRICING_OVERRIDE"
	// ActMandateChange and ActOrderSubmission are the other two acts #410 names.
	// They are declared here so the next implementation extends this package
	// rather than restating the rule; nothing constructs them yet.
	ActMandateChange   Act = "MANDATE_CHANGE"
	ActOrderSubmission Act = "ORDER_SUBMISSION"
)

// Errors. Each is distinguishable because the caller's HTTP status differs and,
// more importantly, because an operator staring at a refused approval needs to
// know WHICH rule refused it.
var (
	// ErrSelfApproval is the whole point of the package.
	ErrSelfApproval = errors.New("dualcontrol: the approver must be a different person from the proposer")
	// ErrPayloadChanged: the approval does not cover what is being applied.
	ErrPayloadChanged = errors.New("dualcontrol: the payload changed after it was proposed, so the approval does not cover it")
	// ErrExpired: the proposal aged out before anyone approved it.
	ErrExpired = errors.New("dualcontrol: the proposal expired before it was approved")
	// ErrMalformed: the proposal or the approver is not well-formed enough to
	// decide on. Never a silent pass.
	ErrMalformed = errors.New("dualcontrol: proposal is not well-formed")
)

// Proposal is a pending act: who asked, for what, over what payload, and until
// when.
//
// IT CARRIES A DIGEST RATHER THAN THE PAYLOAD. This package must not grow an
// opinion about what a pricing override or a mandate looks like — that is how a
// shared rule becomes a shared type becomes a dependency in both directions. The
// caller hashes its own payload with Digest and keeps the payload itself.
type Proposal struct {
	ID        string
	Act       Act
	Subject   string // what is being changed: an exception id, a mandate id, an order id
	Proposer  string // the AUTHENTICATED subject who proposed it, never self-asserted
	Digest    string // Digest() over the exact payload proposed
	CreatedAt time.Time
	ExpiresAt time.Time
}

// DefaultTTL is how long a pending proposal stays approvable.
//
// Long enough that a second approver in another timezone is a normal workflow
// rather than a race; short enough that an approval cannot be collected against
// a stale view of the book. It is a default and not a constant of nature — a
// caller with a faster desk may shorten it.
const DefaultTTL = 24 * time.Hour

// Propose builds a pending proposal. It refuses to build a malformed one rather
// than producing a proposal that fails later at approval time, when the operator
// who could have fixed it has already left.
func Propose(id string, act Act, subject, proposer, digest string, now time.Time, ttl time.Duration) (Proposal, error) {
	switch {
	case strings.TrimSpace(id) == "":
		return Proposal{}, fmt.Errorf("%w: proposal id is empty", ErrMalformed)
	case act == "":
		return Proposal{}, fmt.Errorf("%w: act is empty", ErrMalformed)
	case strings.TrimSpace(subject) == "":
		return Proposal{}, fmt.Errorf("%w: subject is empty", ErrMalformed)
	case normalize(proposer) == "":
		// An unauthenticated proposer is the defect #444 fixed on this very
		// surface: an actor read from the request body. A proposal whose proposer
		// is empty cannot be approved by anyone, because EVERY approver would
		// differ from it — the self-approval check would pass vacuously.
		return Proposal{}, fmt.Errorf("%w: proposer is empty, so no approver could ever differ from it", ErrMalformed)
	case strings.TrimSpace(digest) == "":
		return Proposal{}, fmt.Errorf("%w: digest is empty, so an approval would cover nothing", ErrMalformed)
	case ttl <= 0:
		return Proposal{}, fmt.Errorf("%w: ttl must be positive, or the proposal is born expired", ErrMalformed)
	case now.IsZero():
		return Proposal{}, fmt.Errorf("%w: creation time is zero", ErrMalformed)
	}
	return Proposal{
		ID:        id,
		Act:       act,
		Subject:   subject,
		Proposer:  proposer,
		Digest:    digest,
		CreatedAt: now.UTC(),
		ExpiresAt: now.UTC().Add(ttl),
	}, nil
}

// Approve reports whether approver may approve this proposal for the payload
// digest being applied, at now.
//
// A NIL RETURN IS THE ONLY THING THAT AUTHORISES THE ACT. Every other path is an
// error naming which rule refused, and a caller that ignores the error applies an
// unapproved change — so callers must branch on it, never merely log it.
func (p Proposal) Approve(approver, digest string, now time.Time) error {
	// Malformed FIRST. A proposal that never validated must not reach the
	// interesting checks, where an empty field could satisfy one vacuously.
	if normalize(p.Proposer) == "" {
		return fmt.Errorf("%w: proposal has no proposer, so the approver check is vacuous", ErrMalformed)
	}
	if p.ExpiresAt.IsZero() {
		// UNKNOWN IS NOT FOREVER. A zero expiry is a proposal built by something
		// that did not go through Propose, and reading it as "never expires"
		// would turn a construction bug into an approval that outlives the book
		// it was reasoned about.
		return fmt.Errorf("%w: proposal has no expiry", ErrMalformed)
	}
	if normalize(approver) == "" {
		return fmt.Errorf("%w: approver is empty — an approval must come from an authenticated subject", ErrMalformed)
	}

	// THE RULE. Normalised on both sides: an approver who differs from the
	// proposer only by letter case or surrounding space is the same person, and
	// treating them as two is a self-approval that reads as four-eyes in the
	// trail.
	//
	// WHAT THIS DOES NOT CATCH is a subject that differs by homoglyph or by
	// Unicode normalisation form ("alice" with a Cyrillic 'а'). That is not
	// papered over here because it cannot be: deciding that two subjects denote
	// the same person is the identity provider's job (#364), and both sides of
	// this comparison are subjects the gateway AUTHENTICATED — so the attack
	// requires the IdP to have issued a credential for the look-alike, which is
	// the defect to fix there rather than to guess at here.
	if normalize(approver) == normalize(p.Proposer) {
		return fmt.Errorf("%w: %q proposed this and cannot approve it", ErrSelfApproval, p.Proposer)
	}

	// The signature must cover the VALUE. Checked before expiry so a caller that
	// tampered with the payload is told THAT, rather than being handed a
	// misleading "expired" it might retry its way past.
	if digest != p.Digest {
		return fmt.Errorf("%w: approved %s, applying %s", ErrPayloadChanged, short(p.Digest), short(digest))
	}

	// Expiry is inclusive of the instant: at ExpiresAt exactly, it is expired.
	if !now.UTC().Before(p.ExpiresAt) {
		return fmt.Errorf("%w: proposed %s, expired %s",
			ErrExpired, p.CreatedAt.Format(time.RFC3339), p.ExpiresAt.Format(time.RFC3339))
	}
	return nil
}

// Pending reports whether the proposal is still awaiting a decision at now.
// A caller lists these so an unapproved act is VISIBLY pending rather than
// silently dropped — an act that neither takes effect nor reports why is the
// failure mode this platform refuses everywhere else.
func (p Proposal) Pending(now time.Time) bool {
	return !p.ExpiresAt.IsZero() && now.UTC().Before(p.ExpiresAt)
}

// Digest hashes the exact payload a proposal covers.
//
// THE PARTS ARE LENGTH-PREFIXED, not joined by a separator. Payload fields
// include free text — an override reason is whatever a human typed — so any
// separator character can appear inside a field, and joining on one lets two
// different payloads hash identically: reason "a|b" with price "c" collides with
// reason "a" and price "b|c". A collision here is not a hash weakness, it is an
// approval for one value authorising another, which is precisely what property 2
// above exists to prevent.
func Digest(parts ...string) string {
	h := sha256.New()
	var n [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		_, _ = h.Write(n[:])
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// normalize folds the differences that are NOT a different person.
func normalize(subject string) string {
	return strings.ToLower(strings.TrimSpace(subject))
}

func short(digest string) string {
	if len(digest) <= 12 {
		return digest
	}
	return digest[:12]
}
