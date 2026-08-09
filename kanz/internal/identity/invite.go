package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// DefaultInviteTTL bounds how long an unredeemed invite is usable.
//
// Short because an invite is a bearer credential that grants whatever authority
// the operator attached to it: leaving one valid for a month means a link in a
// mailbox is a live trader account for a month. Long enough that a person on
// leave can still use it without a second round-trip to an operator.
const DefaultInviteTTL = 72 * time.Hour

// inviteTokenBytes is the entropy in a raw invite token. 32 bytes is what makes
// the SHA-256 storage below sound — see InviteTokenHash.
const inviteTokenBytes = 32

var (
	// ErrInviteNotFound is returned for a token that matches no invite. It is
	// also what a caller sees for an EXPIRED or ALREADY-REDEEMED invite when the
	// store chooses not to distinguish them — see Invite.Redeemable.
	ErrInviteNotFound = errors.New("identity: no such invite")
	// ErrInviteExpired is returned when an invite exists but its TTL has passed.
	ErrInviteExpired = errors.New("identity: invite has expired")
	// ErrInviteAlreadyRedeemed is returned when an invite has already produced a
	// credential. Single use is the whole point: a redeemed invite that still
	// works is a shared password with an expiry date.
	ErrInviteAlreadyRedeemed = errors.New("identity: invite has already been redeemed")
)

// Invite is an operator's offer of an account, carrying the authority that
// account will have.
//
// THE AUTHORITY IS ON THE INVITE, NOT SUPPLIED AT REDEMPTION. Tenant, Roles and
// Portfolios are chosen by the operator who created it; the invitee supplies
// exactly one thing, their credential. That is the difference between
// provisioning and registration, and it is the reason this type carries claim
// fields at all — if redemption accepted them, whoever held the link would
// choose their own authority.
type Invite struct {
	// ID is the invite's own identifier, safe to log and to show in a list.
	ID string
	// TokenHash is the SHA-256 of the raw token. THE RAW TOKEN IS NEVER STORED —
	// see InviteTokenHash for why this hash, and not Argon2id.
	TokenHash string

	// Subject is the account to be created, e.g. "user:alice@eighred.com".
	Subject string
	// Tenant, Roles and Portfolios are the claims the issued token will carry.
	Tenant     string
	Roles      []string
	Portfolios []string

	CreatedBy  string // the operator's subject — who granted this authority
	CreatedAt  time.Time
	ExpiresAt  time.Time
	RedeemedAt *time.Time // nil until redeemed
}

// NewInviteToken mints a raw token and its storable hash.
//
// The raw token is returned ONCE and never again: it is the caller's job to hand
// it to the invitee, and nothing in this package can recover it afterwards. That
// is deliberate — an invite an operator can re-read is an invite an operator can
// leak, and "resend the link" is a re-issue, not a lookup.
func NewInviteToken() (raw string, hash string, err error) {
	b := make([]byte, inviteTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("identity: invite token: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	return raw, InviteTokenHash(raw), nil
}

// InviteTokenHash is the storage form of an invite token.
//
// SHA-256 RATHER THAN ARGON2ID, AND THAT IS NOT AN INCONSISTENCY. A password is
// low-entropy and chosen by a human, so it must be made expensive to guess —
// hence Argon2id, salted per credential. An invite token is 32 bytes of
// cryptographic randomness that nobody chose and nobody memorises; there is
// nothing to guess, and a per-invite salt would make lookup impossible (the
// store has to find a row FROM the presented token). A fast digest of a
// high-entropy secret is the correct primitive, and using Argon2id here would
// buy nothing while making redemption a table scan.
func InviteTokenHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Redeemable reports why an invite cannot be used, or nil if it can.
//
// The three states are kept distinct HERE, at the domain layer, so an operator
// listing invites can see which are expired and which were used. The transport
// layer collapses them for an unauthenticated caller — see the comment on
// ErrInviteNotFound — because telling a stranger "that invite exists but has
// expired" confirms an account was offered to someone.
func (i *Invite) Redeemable(now time.Time) error {
	if i.RedeemedAt != nil {
		return ErrInviteAlreadyRedeemed
	}
	if !now.Before(i.ExpiresAt) {
		return ErrInviteExpired
	}
	return nil
}

// NewInvite builds an unredeemed invite for a subject with the authority the
// operator chose. It does NOT mint the token — the caller does that with
// NewInviteToken and keeps the raw value.
//
// An invite with no roles is refused: it would create an account that
// authenticates and can do nothing, which reads as a broken login rather than
// the misconfiguration it is.
func NewInvite(id, tokenHash, subject, tenant string, roles, portfolios []string, createdBy string, now time.Time, ttl time.Duration) (*Invite, error) {
	switch {
	case id == "":
		return nil, errors.New("identity: invite id required")
	case tokenHash == "":
		return nil, errors.New("identity: invite token hash required")
	case subject == "":
		return nil, errors.New("identity: invite subject required")
	case tenant == "":
		// MT-01: a principal with no tenant reaches no tenant's data, and the
		// gateway refuses an authenticated caller without one on the order path.
		// Minting an account that cannot trade is not a kindness.
		return nil, errors.New("identity: invite tenant required")
	case len(roles) == 0:
		return nil, errors.New("identity: invite must carry at least one role")
	case createdBy == "":
		// Provisioning is an act by somebody. An invite with no author cannot be
		// audited, and "who granted this person trade authority" is the first
		// question asked after an incident.
		return nil, errors.New("identity: invite creator required")
	}
	if ttl <= 0 {
		ttl = DefaultInviteTTL
	}
	return &Invite{
		ID:         id,
		TokenHash:  tokenHash,
		Subject:    subject,
		Tenant:     tenant,
		Roles:      copyOf(roles),
		Portfolios: copyOf(portfolios),
		CreatedBy:  createdBy,
		CreatedAt:  now.UTC(),
		ExpiresAt:  now.UTC().Add(ttl),
	}, nil
}

// copyOf returns a non-nil copy, so a caller mutating their slice afterwards
// cannot change authority already granted.
//
// NON-NIL EVEN WHEN EMPTY, and that is not cosmetic: pgx sends a nil []string as
// SQL NULL, and `portfolios TEXT[] NOT NULL DEFAULT '{}'` rejects an explicit
// NULL — a column default applies only when the column is OMITTED. So an invite
// carrying no portfolios failed to insert at all, with an error naming the
// constraint rather than the nil. A fake store would have accepted it.
func copyOf(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}
