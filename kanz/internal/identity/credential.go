// Package identity is the platform's own identity provider (#99): the user
// store, the credential primitive, and the invite lifecycle that puts a person
// in it.
//
// WHY THIS EXISTS RATHER THAN AN EXTERNAL IdP. The gateway has always been the
// sole identity authority — it authenticates and injects X-Kanz-Principal-*, and
// upstreams trust those headers. What it did NOT have was anywhere for a person
// to come from: the production path was an OIDC device flow against an Eighred
// SSO that does not exist, so `/login` failed with "login required but failed"
// and the only working credential was a hand-minted KANZ_TOKEN in an env var.
//
// The authenticator SEAM is kept. pkg/auth already has two implementations and
// the gateway picks one at its composition root; this adds a third source of
// truth behind the same interface rather than replacing the interface. A fund
// manager who later requires their own Okta or Entra tenant is then a new
// implementation, not a re-architecture — which is a routine institutional
// demand and a bad thing to have designed out.
//
// # PROVISIONING, NOT REGISTRATION
//
// There is no self-service signup, and that is the central decision. On this
// platform a token's claims ARE authority: kanz-trader moves capital,
// kanz-operator writes venue keys and drains nodes, and the portfolios claim
// decides which portfolios a caller can see at all. A person who can register
// themselves either chooses their own authority, or is a stranger who reached
// the TUI. So an operator CREATES the account — tenant, roles and portfolios are
// set by them — and the invitee supplies exactly one thing: their credential.
package identity

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. THEY ARE STORED WITH EVERY HASH, not just applied at
// write time — see Encode's PHC string — so raising them later does not
// invalidate a single existing credential: Verify reads the parameters the hash
// was made with, and NeedsRehash reports when a successful login should be
// upgraded in place.
//
// The memory cost is the security parameter that matters and it is also a DoS
// surface: each concurrent verification holds memoryKiB. 64 MiB at p=4 is the
// OWASP-recommended shape for a login that happens rarely per user, and a
// deployment that fronts this with no rate limit has a different problem than
// its Argon2 parameters.
const (
	argonTimeCost  = 2
	argonMemoryKiB = 64 * 1024
	argonThreads   = 4
	argonKeyLen    = 32
	argonSaltLen   = 16
)

// ErrCredentialMismatch is returned when a credential does not verify. It is
// deliberately the SAME error for a wrong password and an unparseable hash: a
// caller must not be able to distinguish "this account exists and you got the
// password wrong" from anything else, and neither must a log reader.
var ErrCredentialMismatch = errors.New("identity: credential does not match")

// Hash is an encoded Argon2id credential — the PHC string, safe to store.
//
// It is a named type so a raw password and a stored hash cannot be passed to the
// same parameter by mistake. That has happened in enough systems to be worth one
// line of type safety.
type Hash string

// HashCredential derives a new Argon2id hash with a fresh random salt.
//
// The plaintext is never returned, logged, or retained: it exists in the
// caller's memory for the length of this call and nowhere else in this package.
func HashCredential(plaintext string) (Hash, error) {
	if plaintext == "" {
		return "", errors.New("identity: empty credential")
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("identity: salt: %w", err)
	}
	key := argon2.IDKey([]byte(plaintext), salt, argonTimeCost, argonMemoryKiB, argonThreads, argonKeyLen)
	return encode(salt, key, argonTimeCost, argonMemoryKiB, argonThreads), nil
}

// Verify reports whether plaintext produced h.
//
// It re-derives with the parameters STORED IN h rather than the current
// constants, so a credential written under older parameters still verifies after
// a bump. The comparison is constant-time: a byte-wise compare leaks the length
// of the matching prefix through timing, which is enough to recover a hash given
// patience.
func Verify(h Hash, plaintext string) error {
	salt, want, t, m, p, err := decode(h)
	if err != nil {
		// Same error as a mismatch, deliberately — see ErrCredentialMismatch.
		return ErrCredentialMismatch
	}
	got := argon2.IDKey([]byte(plaintext), salt, t, m, p, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrCredentialMismatch
	}
	return nil
}

// NeedsRehash reports whether h was made with weaker parameters than the current
// ones, so a successful login can upgrade it in place.
//
// Without this, raising the cost protects only accounts created afterwards — and
// the accounts that predate the raise are exactly the oldest and most valuable
// ones. An unparseable hash reports true: it cannot be verified against anyway,
// and rewriting it on the next successful login is the repair.
func NeedsRehash(h Hash) bool {
	_, _, t, m, p, err := decode(h)
	if err != nil {
		return true
	}
	return t < argonTimeCost || m < argonMemoryKiB || p < argonThreads
}

func encode(salt, key []byte, t, m uint32, p uint8) Hash {
	b64 := base64.RawStdEncoding.EncodeToString
	return Hash(fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, m, t, p, b64(salt), b64(key)))
}

// decode parses a PHC string back into its salt, digest and parameters.
func decode(h Hash) (salt, key []byte, t, m uint32, p uint8, err error) {
	parts := strings.Split(string(h), "$")
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, key
	if len(parts) != 6 || parts[1] != "argon2id" {
		return nil, nil, 0, 0, 0, errors.New("identity: not an argon2id hash")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return nil, nil, 0, 0, 0, errors.New("identity: unsupported argon2 version")
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return nil, nil, 0, 0, 0, errors.New("identity: unreadable argon2 parameters")
	}
	if salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil {
		return nil, nil, 0, 0, 0, errors.New("identity: unreadable salt")
	}
	if key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil {
		return nil, nil, 0, 0, 0, errors.New("identity: unreadable digest")
	}
	return salt, key, t, m, p, nil
}
