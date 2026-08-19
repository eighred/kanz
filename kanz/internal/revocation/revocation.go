// Package revocation carries the per-subject token revocation feed (#532) — the
// wire contract identity SERVES and the api-gateway ENFORCES, written down once
// so the two halves cannot drift into disagreeing about what a revocation is.
//
// # The gap this closes
//
// Disabling an account stopped the NEXT login and nothing else. The gateway
// verifies a signature against JWKS and reads no account state at all, so a
// token already in someone's hands stayed good for its full lifetime — up to
// IDENTITY_TOKEN_TTL after an offboarded trader was locked out, or after a
// credential was known to be compromised. The uncovered case is a token
// exfiltrated from a process: a leaked log, an operator's `curl`, a compromised
// pod. Shortening the TTL (#552) bounded that window; it did not close it.
//
// # Why a monotonic timestamp and not a list of disabled subjects
//
// An entry is "every token this subject holds that was minted before T is
// refused", NOT "this subject is currently disabled". The difference shows up
// on re-enable: with a currently-disabled list, enabling an account
// RESURRECTS the very token the disable was meant to kill. NotBefore only ever
// moves forward, so a token minted before a disable stays dead for good, while
// the fresh token from the re-enabled account's next login is admitted with no
// operator action at all.
//
// It is also what makes the feed safe to fetch repeatedly and in any order: the
// state is a per-subject high-water mark, so a fetch that arrives late or twice
// converges on the same answer. There is nothing to replay and no ordering to
// preserve.
//
// # Why subjects are hashed
//
// The feed is a list, and a list enumerates without guessing. identity goes to
// deliberate lengths to stop its login route becoming a user-enumeration oracle
// (see the decoy hash in services/identity/internal/server/server.go), and
// serving the plaintext subject of every account an operator has ever disabled
// would hand out for free what that decoy exists to make expensive. The gateway
// hashes the `sub` it just verified and looks THAT up, so the feed answers the
// question without publishing the roster.
//
// This is not a secrecy claim — SHA-256 over a guessable subject is guessable —
// it is the difference between an attacker who must already know a subject and
// one who is handed the list.
package revocation

import (
	"crypto/sha256"
	"encoding/hex"
)

// Entry is one account's revocation high-water mark.
type Entry struct {
	// SubjectHash is HashSubject(subject). Never the subject itself; see the
	// package comment.
	SubjectHash string `json:"subject_hash"`
	// NotBefore is Unix SECONDS. Every token for this subject with an `iat`
	// strictly before it is refused.
	//
	// SECONDS, not nanoseconds, because that is the unit `iat` is in (RFC 7519
	// §4.1.6) and the comparison is against `iat`. Carrying a finer unit here
	// would invent precision the other side of the comparison does not have.
	NotBefore int64 `json:"not_before"`
}

// Feed is the whole document identity serves and the gateway caches.
//
// IT IS A FULL SNAPSHOT, NOT A DELTA. A delta feed has to be applied in order
// and cannot be recovered from a missed fetch without a cursor the server must
// then remember per client; a snapshot makes a cold gateway pod and a
// long-running one converge on the same state by the same code path, which is
// the property the cold-start clause of #532 asks for.
type Feed struct {
	// Kind is FeedKind, always. It is what stops a misdirected URL from
	// decoding into an empty denylist that reads as "nobody is revoked" — see
	// FeedKind for the failure it removes.
	Kind string `json:"kind"`
	// AsOf is when identity read the table, in Unix SECONDS. It is the
	// SERVER's clock, and the gateway does not trust it for staleness — see
	// Cache.Check, which measures against its own successful-fetch time. It is
	// carried for operators reading the endpoint by hand.
	AsOf int64 `json:"as_of"`
	// Entries is every account with a revocation mark, in no guaranteed order.
	//
	// NEVER TRUNCATED. A denylist cut short to fit a limit is a denylist that
	// silently admits whoever fell off the end, so the reader refuses an
	// oversized body outright (Cache.Refresh) rather than accepting a prefix.
	Entries []Entry `json:"entries"`
}

// HashSubject maps an account subject onto the identifier the feed carries.
//
// SHA-256, hex, unsalted and unstretched — on purpose. A salt would have to be
// shared with every gateway to be usable, at which point it is not a secret; a
// KDF would cost the gateway a stretch per request on the hot authentication
// path to buy nothing, because the input space is subjects an attacker can
// already enumerate by other means. The job here is to avoid PUBLISHING the
// roster, not to make a known subject unrecognisable.
func HashSubject(subject string) string {
	sum := sha256.Sum256([]byte(subject))
	return hex.EncodeToString(sum[:])
}
