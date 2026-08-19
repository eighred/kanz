package middleware

import (
	"errors"
	"fmt"
	"time"

	"github.com/eighred/kanz/internal/revocation"
)

// ErrAuthUnavailable means the gateway could not JUDGE the credential, as
// distinct from judging it and refusing it.
//
// THE TWO MUST NOT COLLAPSE, and until #532 they did — Auth mapped every
// authenticator error to 401. A 401 is an instruction to the client: the web app
// reads it as "your session ended", destroys the session and sends the user to
// log in. During an identity-service outage that is the worst possible response,
// because it happens to every signed-in user at once and points them all at the
// service that is down. 503 says what is true — the credential was never judged,
// and it is the server's fault.
var ErrAuthUnavailable = errors.New("auth: the gateway cannot judge this credential right now")

// RevocationChecker answers whether an already-verified token is still
// honoured, given its subject and when it was minted. internal/revocation.Cache
// is the implementation the gateway wires; the interface is here so this
// package can be tested without an HTTP server behind it.
type RevocationChecker interface {
	// Check returns nil when the token stands, revocation.ErrRevoked when the
	// subject was disabled after the token was minted, and revocation.ErrUnusable
	// when it cannot answer at all.
	Check(subject string, issuedAt time.Time) error
}

// Revoking wraps an Authenticator with the per-subject revocation check (#532).
//
// # Why it is a decorator and not a branch inside each authenticator
//
// There are two authenticators (OIDC and the dev HS256 arm) and there will be
// more the day this estate federates. A check written inside one of them is a
// check the next one silently does not have — which is exactly how the
// production arm shipped without the caller's portfolio entitlement in #225.
// Wrapping is what makes "every authenticator this gateway builds is subject to
// revocation" a property of the composition root, checkable in one place, rather
// than a habit each new arm has to remember.
//
// # Why it runs second
//
// A revocation lookup on an UNVERIFIED token would let anyone probe the denylist
// with forged tokens. Running after the inner authenticator means only a
// genuine, correctly-signed token ever reaches it.
type Revoking struct {
	inner Authenticator
	rev   RevocationChecker
}

// NewRevoking wraps inner. Both arguments are REQUIRED: a nil checker would make
// this decorator a pass-through that looks, in a composition root and in a diff,
// exactly like a working revocation control. That is the shape #535 and #539
// both took — a control present in the wiring and reachable by nobody — so it is
// refused at construction instead.
func NewRevoking(inner Authenticator, rev RevocationChecker) (*Revoking, error) {
	switch {
	case inner == nil:
		return nil, errors.New("middleware: revocation needs an authenticator to wrap")
	case rev == nil:
		return nil, errors.New("middleware: revocation needs a checker — wrapping an authenticator " +
			"with a nil one produces a pass-through indistinguishable from an enforced control")
	}
	return &Revoking{inner: inner, rev: rev}, nil
}

// Authenticate verifies the token, then checks it against the revocation feed.
func (r *Revoking) Authenticate(token string) (*Principal, error) {
	p, err := r.inner.Authenticate(token)
	if err != nil {
		return nil, err
	}
	// NEITHER A PRINCIPAL NOR AN ERROR IS NOT AN ADMISSION. Dereferencing a nil
	// here would panic the platform's sole ingress on a request path, so the one
	// authenticator that ever behaves this way would take order entry down rather
	// than be caught. Refusing as unavailable says what is true: nothing
	// identified this caller.
	if p == nil {
		return nil, fmt.Errorf("%w: the authenticator returned no principal and no error", ErrAuthUnavailable)
	}
	switch err := r.rev.Check(p.Subject, p.IssuedAt); {
	case err == nil:
		return p, nil
	case errors.Is(err, revocation.ErrRevoked):
		// ErrUnauthenticated, and nothing more specific. From the holder's side a
		// revoked token is simply not a valid credential; telling them WHICH check
		// refused them tells the holder of a stolen token when the theft was
		// noticed.
		return nil, ErrUnauthenticated
	default:
		// EVERY OTHER OUTCOME REFUSES AS UNAVAILABLE — revocation.ErrUnusable and
		// anything unclassified alike. An unclassified error means the checker
		// answered neither "fine" nor a failure this decorator recognises, so
		// nothing here can say whether the caller is revoked. Admitting them would
		// make a bug in the checker indistinguishable from a clean bill of health,
		// which is the failure mode this control exists to remove.
		return nil, fmt.Errorf("%w: %w", ErrAuthUnavailable, err)
	}
}

var _ Authenticator = (*Revoking)(nil)
