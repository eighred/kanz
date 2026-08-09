package identity

import (
	"errors"
	"time"
)

// Status is an account's lifecycle state. A disabled account keeps its row —
// deleting it would lose the audit trail of what it was allowed to do, which is
// the thing an investigation needs most.
type Status string

const (
	StatusActive   Status = "active"
	StatusDisabled Status = "disabled"
)

// ErrUserNotFound is returned when no account matches. Login must report the
// SAME error for an unknown subject and a wrong credential — see LoginFailed.
var ErrUserNotFound = errors.New("identity: no such user")

// ErrLoginFailed is what an unauthenticated caller is told, always.
//
// A caller who can distinguish "no such account" from "wrong password" can
// enumerate the platform's users — every subject they try is answered. On a
// system whose users are a fund's traders and operators, that list is worth
// having on its own, before anyone guesses a single credential.
var ErrLoginFailed = errors.New("identity: login failed")

// User is an account and the authority it carries.
//
// Roles and Portfolios are the claims the issued token will bear, and they are
// set from the INVITE — never from anything the account holder supplies. See the
// package comment.
type User struct {
	Subject    string
	Tenant     string
	Roles      []string
	Portfolios []string
	Credential Hash
	Status     Status
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Active reports whether this account may authenticate at all.
func (u *User) Active() bool { return u.Status == StatusActive }

// UserFromInvite builds the account an invite describes, given the credential
// its holder has just chosen.
//
// The claims come from the invite and the credential from the redeemer: that
// split IS the provisioning boundary, expressed as a function signature rather
// than a rule someone has to remember.
func UserFromInvite(inv *Invite, cred Hash, now time.Time) *User {
	return &User{
		Subject:    inv.Subject,
		Tenant:     inv.Tenant,
		Roles:      copyOf(inv.Roles),
		Portfolios: copyOf(inv.Portfolios),
		Credential: cred,
		Status:     StatusActive,
		CreatedAt:  now.UTC(),
		UpdatedAt:  now.UTC(),
	}
}
