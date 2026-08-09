package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/eighred/kanz/pkg/auth"
)

// DefaultTokenTTL bounds a session. Short enough that a stolen token expires
// while the theft is still recent; long enough that a trading day does not
// become a sequence of logins.
const DefaultTokenTTL = 8 * time.Hour

// SIGNING IS ASYMMETRIC, AND THAT IS THE WHOLE POINT OF THIS FILE.
//
// The obvious shortcut was HS256 against API_GATEWAY_JWT_SECRET — the gateway
// already validates that, so issuance would have been thirty lines and no new
// verification path. internal/devtoken exists precisely to mint those, and its
// own package doc says why it must not become the production credential:
//
//	"a shared HMAC secret is a symmetric credential every holder can forge with"
//
// With a shared secret the GATEWAY can mint an operator's token, and so can
// anything else that reads the secret — a debug endpoint, a log line, a leaked
// env var. The property the OIDC design had, and the one worth keeping, is that
// the verifier holds a PUBLIC key and cannot forge anything: compromising the
// gateway lets an attacker serve wrong answers, not become the head of trading.
//
// So the identity service holds the private key and mints; the gateway keeps its
// existing pkg/auth.OIDCAuthenticator, pointed at THIS issuer and THIS JWKS.
// That is not "keeping SSO" — it is standard JWT verification against a key set,
// and it means the gateway needs no new code at all: OIDCConfig.JWKSURI is
// settable directly (oidc.go:235), so no discovery document is required either.
const (
	// ES256 over P-256: the gateway's verifier accepts it (pkg/auth/oidc.go's
	// asymmetricAlgs), keys and signatures are small, and signing is fast enough
	// that a login is not noticeably slower than the Argon2id verify preceding it.
	tokenAlg = jose.ES256
)

// Signer mints platform tokens and publishes the public half as a JWKS.
type Signer struct {
	key      *ecdsa.PrivateKey
	kid      string
	issuer   string
	audience string
	ttl      time.Duration
	now      func() time.Time
}

// NewSigner builds a signer over an existing private key.
//
// issuer and audience must be the SAME strings the gateway is configured with —
// its verifier requires `iss` to match exactly and `aud` to contain the
// audience, so a mismatch is a token that authenticates nobody. They are
// parameters rather than constants for that reason: one deployment's issuer URL
// is not another's.
// SignerOption customises a Signer.
type SignerOption func(*Signer)

// WithClock injects the issuing clock. Tests use it to mint a token whose
// expiry is already past — which cannot be done by passing a negative TTL,
// because a non-positive one falls back to the default below. Without this seam
// "sessions expire" is an unprovable claim.
func WithClock(now func() time.Time) SignerOption {
	return func(s *Signer) {
		if now != nil {
			s.now = now
		}
	}
}

func NewSigner(key *ecdsa.PrivateKey, issuer, audience string, ttl time.Duration, opts ...SignerOption) (*Signer, error) {
	switch {
	case key == nil:
		return nil, errors.New("identity: signing key required")
	case issuer == "":
		return nil, errors.New("identity: token issuer required — it must equal the gateway's configured issuer exactly")
	case audience == "":
		return nil, errors.New("identity: token audience required — the gateway requires `aud` to contain it")
	}
	// A NON-POSITIVE TTL FALLS BACK, rather than minting a token that is born
	// expired. Zero means "unset" at almost every call site, and a caller who
	// genuinely wants a past expiry wants WithClock, not a negative duration.
	if ttl <= 0 {
		ttl = DefaultTokenTTL
	}
	sn := &Signer{
		key:      key,
		kid:      thumbprint(key),
		issuer:   issuer,
		audience: audience,
		ttl:      ttl,
		now:      time.Now,
	}
	for _, o := range opts {
		o(sn)
	}
	return sn, nil
}

// GenerateKey mints a fresh P-256 key, for a first boot with no key on record.
func GenerateKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// Mint issues a bearer token for an account.
//
// A DISABLED ACCOUNT IS REFUSED HERE, not only at login. This is the last point
// that sees the User, so a future caller that mints from a cached record cannot
// route around the status check.
func (s *Signer) Mint(u *User) (string, time.Time, error) {
	if u == nil || u.Subject == "" {
		return "", time.Time{}, errors.New("identity: cannot mint a token for no subject")
	}
	if !u.Active() {
		return "", time.Time{}, fmt.Errorf("identity: account %s is %s", u.Subject, u.Status)
	}

	now := s.now().UTC()
	expiry := now.Add(s.ttl)

	sig, err := jose.NewSigner(
		jose.SigningKey{Algorithm: tokenAlg, Key: s.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader(jose.HeaderKey("kid"), s.kid),
	)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("identity: signer: %w", err)
	}

	// EXPIRY IS ALWAYS SET, and it is not belt-and-braces. The verifier's own
	// comment claims go-jose makes `exp` mandatory; it does not — validation
	// skips the check when the claim is absent, so a token minted without one
	// would be accepted FOREVER. Setting it here is what bounds a session at all.
	claims := jwt.Claims{
		Subject:  u.Subject,
		Issuer:   s.issuer,
		Audience: jwt.Audience{s.audience},
		IssuedAt: jwt.NewNumericDate(now),
		Expiry:   jwt.NewNumericDate(expiry),
	}
	// The claim NAMES are the gateway's, not ours. `portfolios` is fixed at
	// pkg/auth.ClaimPortfolios; tenant/roles are renameable in OIDCConfig but
	// default to these, and a deployment that renames them must rename here too.
	//
	// Roles and portfolios are ARRAYS. The gateway's OIDC path also accepts a
	// space-delimited string, but its HS256 path rejects one outright — arrays
	// are the shape both read, and minting the shape only one arm accepts is how
	// a token works in dev and fails in production.
	custom := map[string]any{
		"tenant":             u.Tenant,
		"roles":              append([]string{}, u.Roles...),
		auth.ClaimPortfolios: append([]string{}, u.Portfolios...),
	}

	raw, err := jwt.Signed(sig).Claims(claims).Claims(custom).Serialize()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("identity: sign: %w", err)
	}
	return raw, expiry, nil
}

// JWKS is the public half, for the gateway to fetch.
//
// PUBLIC KEYS ONLY — jose.JSONWebKey.Public() strips the private scalar. Serving
// the private key here would hand every reader the ability to mint an operator's
// token, which is the exact property this whole file exists to preserve.
func (s *Signer) JWKS() jose.JSONWebKeySet {
	return jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key:       s.key.Public(),
		KeyID:     s.kid,
		Algorithm: string(tokenAlg),
		Use:       "sig",
	}}}
}

// KeyID is the `kid` this signer stamps, so a rotation can be observed.
func (s *Signer) KeyID() string { return s.kid }

// thumbprint derives a stable key id from the public key.
//
// DERIVED RATHER THAN RANDOM so the same key always advertises the same kid: a
// restart that minted a new random id would make every token issued before it
// unverifiable, because the verifier looks the token's kid up in the key set.
func thumbprint(key *ecdsa.PrivateKey) string {
	// PublicKey.Bytes rather than the X/Y big.Ints: those accessors are
	// deprecated precisely because big.Int is the wrong tool for cryptographic
	// values, and the encoded form is what any other implementation would hash.
	pub, err := key.PublicKey.Bytes()
	if err != nil {
		// Unreachable for a key produced by GenerateKey; a key that cannot encode
		// its own public half cannot sign either, and NewSigner would fail next.
		return ""
	}
	sum := sha256.Sum256(pub)
	return base64.RawURLEncoding.EncodeToString(sum[:16])
}
