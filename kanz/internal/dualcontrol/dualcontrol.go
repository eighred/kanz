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
// that belongs to the caller and its configuration. This package answers only
// "is this approval valid", which is the half that must not vary between the
// three acts.
//
// Both halves now have an owner. The pricing override takes EVERY one (#495: "a
// control with no threshold has nothing to calibrate"); the order path has a
// number, because "every order takes two people" is not a workable rule, and
// services/oms/internal/approval owns it — the threshold in gate.go and, more
// importantly, the decision about exactly which of an order's fields the digest
// covers. THAT SECOND HALF IS WHERE THIS PACKAGE'S GUARANTEE CAN BE HOLLOWED
// OUT: Approve and Covers bind a signature to a digest exactly as tightly as the
// caller's digest binds it to the payload, and a field the caller leaves out is
// a field somebody can change after the second signature.
//
// # HOW A REFUSAL REACHES THE PERSON REFUSED IS ALSO THE CALLER'S (#558)
//
// This is worth stating because it looks like divergence and is not. #558 asked
// whether all three acts share the same silence on a refused approval. Checked
// against the code rather than the issue text, they do not, and the difference
// is the transport rather than the rule:
//
//   - datamaster's pricing override approves over HTTP. refuseApproval answers
//     403/409/500 with a message naming the rule, in the same request. The
//     approver is told synchronously.
//   - a mandate change approves over HTTP too, on either of its two producers:
//     kanz-mandate's runApprove returns the error from Approve and main prints it
//     to stderr and exits 1, and the compliance service's approve route answers
//     403/409/500 with a message naming the rule (#562). Both tell the approver
//     synchronously, which is why act two adds no "refused" state to its queue.
//   - the OMS approves over the BUS. The gateway answers 202 at publish time and
//     nothing is waiting for a reply, so there is no synchronous channel to
//     answer on — and no FACT can carry it either, because all four
//     CommandOutcomeStatus values are terminal while a refused proposal is not.
//     That is why the OMS records the refusal on the proposal and surfaces it on
//     ListPendingApprovals, and why the other two need nothing added.
//
// The rule that must not vary is Approve's, and it does not. A REFUSAL REPORTED
// THE SAME WAY ON ALL THREE would mean giving the two synchronous callers a
// second, asynchronous channel they have no reader for — which is the failure
// #563 recorded for the override path's lapsed proposals, in reverse.
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
	// ActMandateChange is a change to the mandate itself — the control that
	// governs everything else. It has TWO producers, and the difference between
	// them is the whole of #562:
	//
	//   - cmd/kanz-mandate (#511) takes two INVOCATIONS. Both run under one
	//     operator's SVID and the proposal is a file, so a unilateral change is
	//     DETECTABLE in the trail and not prevented. It is kept as the break-glass
	//     path for an estate with no gateway; without it a fresh install has no way
	//     to put a portfolio under mandate at all.
	//   - services/compliance/internal/api (#562) takes two separately
	//     authenticated REQUESTS through the gateway, behind authz.Mandate. The
	//     proposal rests server-side between them, so the second signature comes
	//     from a second credential and a unilateral change is PREVENTED.
	//
	// The FACT they publish is identical; its envelope source is what says which
	// path produced it, and that is the honest limit of what the trail can carry.
	ActMandateChange Act = "MANDATE_CHANGE"
	// ActOrderSubmission is an order at or above a notional threshold. Wired by
	// #410 act three, in services/oms/internal/approval, which owns the two halves
	// this package deliberately does not: which orders are large enough
	// (OMS_DUAL_CONTROL_MIN_NOTIONAL) and exactly which of an order's fields the
	// digest covers.
	//
	// ALL THREE ARE NOW CONSTRUCTED. An earlier version of this comment said
	// "nothing constructs them yet", which is what the declarations were for — a
	// third implementation extending this package rather than restating the rule.
	// That worked; the note is kept because it is the argument for why a fourth
	// act belongs here too.
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

// The STATE VOCABULARY every dual-control queue renders (#558, #563).
//
// # Why the spelling lives here and not in each act
//
// Two queues now list proposals awaiting a second signature — datamaster's
// GET /v1/exceptions/pending-overrides and the OMS's ListPendingApprovals — and
// each one spelled its own state. They diverged on the first day they both
// existed: one said "pending", the other "PENDING", so a client reading both
// controls needed two casings for one concept. That is the divergence this
// package exists to stop, arriving in the surface rather than in the rule, which
// is exactly where nobody was watching for it.
//
// Lowercase because datamaster's queue shipped first and is on main. One
// unmerged surface changing beats one merged surface changing.
//
// # THE SETS DIFFER PER ACT, AND THAT IS NOT AN OMISSION TO REPAIR
//
// Do not "complete" either queue by adding the state the other has. Each absence
// is a property of how that act reports, and adding the missing value would mean
// publishing a state that act can never reach:
//
//   - datamaster has NO "refused". An override is approved over HTTP, so a
//     refusal goes back on the same request as a 403/409 naming the rule. There
//     is no window in which a refused override sits on a queue waiting to be
//     discovered.
//   - the OMS has NO "lapsed". ProposalStore.Pending deliberately excludes
//     expired work — a queue must not invite a signature on an order that can no
//     longer be released — and expiry IS terminal there, so #547 answers it with
//     an ORDER_REJECTED FACT instead. datamaster cannot do that: it publishes one
//     subject nothing but the audit projector consumes (#563), so listing the
//     lapsed proposal is the only reader it has.
//
// A third act adds a value here only if it can genuinely reach it.
const (
	// StatePending: awaiting a second signature, and nobody has been turned away.
	StatePending = "pending"
	// StateRefused: STILL awaiting a second signature, and the last person who
	// tried was refused. It NEVER means finished — a decided proposal is not on
	// the queue at all. OMS only.
	StateRefused = "refused"
	// StateLapsed: nobody signed it before it expired, so it is no longer
	// actionable and is listed rather than erased. datamaster only.
	StateLapsed = "lapsed"
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
// digest being applied, at now, and on success returns the Approval that is the
// EVIDENCE of it.
//
// THE EVIDENCE IS A RETURN VALUE BECAUSE AN ERROR IS TOO EASY TO DROP. This
// function used to return only an error, with a doc note that "a caller that
// ignores the error applies an unapproved change — so callers must branch on
// it". That is a rule enforced by remembering, which is the kind this repository
// has already paid for once. An Approval cannot be obtained without passing
// every check, so a surface that REQUIRES one (mandate publishing does) cannot
// be called unilaterally at all — the refusal moves from run time to the
// signature, where it can be proven.
//
// The error is still returned and still names which rule refused, because an
// operator staring at a rejected approval needs to know which.
func (p Proposal) Approve(approver, digest string, now time.Time) (Approval, error) {
	// Malformed FIRST. A proposal that never validated must not reach the
	// interesting checks, where an empty field could satisfy one vacuously.
	if normalize(p.Proposer) == "" {
		return Approval{}, fmt.Errorf("%w: proposal has no proposer, so the approver check is vacuous", ErrMalformed)
	}
	if p.ExpiresAt.IsZero() {
		// UNKNOWN IS NOT FOREVER. A zero expiry is a proposal built by something
		// that did not go through Propose, and reading it as "never expires"
		// would turn a construction bug into an approval that outlives the book
		// it was reasoned about.
		return Approval{}, fmt.Errorf("%w: proposal has no expiry", ErrMalformed)
	}
	if normalize(approver) == "" {
		return Approval{}, fmt.Errorf("%w: approver is empty — an approval must come from an authenticated subject", ErrMalformed)
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
		return Approval{}, fmt.Errorf("%w: %q proposed this and cannot approve it", ErrSelfApproval, p.Proposer)
	}

	// The signature must cover the VALUE. Checked before expiry so a caller that
	// tampered with the payload is told THAT, rather than being handed a
	// misleading "expired" it might retry its way past.
	if digest != p.Digest {
		return Approval{}, fmt.Errorf("%w: approved %s, applying %s", ErrPayloadChanged, short(p.Digest), short(digest))
	}

	// Expiry is inclusive of the instant: at ExpiresAt exactly, it is expired.
	if !now.UTC().Before(p.ExpiresAt) {
		return Approval{}, fmt.Errorf("%w: proposed %s, expired %s",
			ErrExpired, p.CreatedAt.Format(time.RFC3339), p.ExpiresAt.Format(time.RFC3339))
	}
	return Approval{
		act:        p.Act,
		subject:    p.Subject,
		proposer:   p.Proposer,
		approver:   approver,
		digest:     p.Digest,
		approvedAt: now.UTC(),
	}, nil
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

// SameSubject reports whether two subject strings denote the SAME person under
// this package's rule.
//
// IT IS EXPORTED SO THAT NOBODY RETYPES THE COMPARISON. A store that has to
// refuse a self-approval before writing a row — services/oms/internal/order's
// ProposalStore.Claim is the first — otherwise reaches for
// strings.EqualFold or a byte comparison, and a byte comparison is the clause
// #495 recorded as the one that fails QUIETLY: one person holds both signatures
// by capitalising a letter, and the audit trail then shows two distinct actors,
// which reads as satisfied in exactly the record an auditor would check.
//
// The SQL side of the same rule is spelled lower(btrim(...)) in the CHECK
// constraints (datamaster/0004, oms/0009). All three must agree.
func SameSubject(a, b string) bool { return normalize(a) == normalize(b) }

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

// Approval is EVIDENCE that a specific payload was approved, for a specific act,
// by someone other than the person who proposed it, before the proposal expired.
//
// # Why it is a type and not two strings
//
// A surface that takes `proposedBy, approvedBy string` can be handed two names
// by one person, and nothing in its signature says otherwise. The mandate
// publisher was exactly that shape: one CLI flag for the actor, a FACT recording
// it, and no second party anywhere in the call. Taking an Approval instead means
// the two names ARRIVE TOGETHER, from a check that has already run, and cannot
// be supplied independently.
//
// # Its fields are unexported on purpose
//
// A composite literal cannot forge one from another package. The zero value is
// still constructible — Go gives every type that — which is why Covers refuses
// it explicitly rather than assuming a non-zero Approval is a valid one.
type Approval struct {
	act        Act
	subject    string
	proposer   string
	approver   string
	digest     string
	approvedAt time.Time
}

// Act, Subject, Proposer, Approver, Digest and ApprovedAt read the evidence.
// They exist so the caller can RECORD both names on whatever it publishes — the
// audit value of dual control is entirely in the trail naming two people.
func (a Approval) Act() Act              { return a.act }
func (a Approval) Subject() string       { return a.subject }
func (a Approval) Proposer() string      { return a.proposer }
func (a Approval) Approver() string      { return a.approver }
func (a Approval) Digest() string        { return a.digest }
func (a Approval) ApprovedAt() time.Time { return a.approvedAt }

// Covers reports whether this approval authorises applying digest under act.
//
// A CALLER THAT REQUIRES AN APPROVAL MUST STILL CALL THIS. Holding an Approval
// proves some approval happened; it does not prove this one covers what is about
// to be applied. The zero value is the case that makes it mandatory — Go permits
// dualcontrol.Approval{} anywhere, and a publisher that merely accepted the type
// would treat "no approval at all" as approved.
//
// Every check here re-derives from the stored evidence rather than trusting that
// Approve ran, so this is a complete gate on its own and not a second opinion.
func (a Approval) Covers(act Act, digest string) error {
	switch {
	case normalize(a.approver) == "" || normalize(a.proposer) == "":
		// THE ZERO VALUE LANDS HERE, and so does anything built by something that
		// did not go through Approve. Named as malformed rather than as
		// self-approval, because the operator needs to know the difference
		// between "nobody approved this" and "the same person approved it".
		return fmt.Errorf("%w: approval names no proposer/approver pair, so nothing was checked", ErrMalformed)
	case a.act == "":
		return fmt.Errorf("%w: approval names no act", ErrMalformed)
	case a.approvedAt.IsZero():
		return fmt.Errorf("%w: approval has no timestamp", ErrMalformed)
	case strings.TrimSpace(a.digest) == "":
		return fmt.Errorf("%w: approval covers no payload", ErrMalformed)
	}
	// RE-CHECKED, not assumed. An approval whose two names match is a
	// self-approval however it was obtained, and this is the last place before
	// the act takes effect.
	if normalize(a.approver) == normalize(a.proposer) {
		return fmt.Errorf("%w: %q proposed and approved this", ErrSelfApproval, a.proposer)
	}
	// AN APPROVAL FOR ONE ACT MUST NOT AUTHORISE ANOTHER. Without this an
	// approval collected for a pricing override would publish a mandate — the
	// two surfaces share this package precisely so they cannot share evidence.
	if act != a.act {
		return fmt.Errorf("%w: approval is for %s, applying %s", ErrPayloadChanged, a.act, act)
	}
	if digest != a.digest {
		return fmt.Errorf("%w: approved %s, applying %s", ErrPayloadChanged, short(a.digest), short(digest))
	}
	return nil
}
