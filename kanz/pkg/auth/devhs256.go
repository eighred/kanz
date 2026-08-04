package auth

// The scope a DEV HS256 token is bound to (#242).
//
// ONLY THE NAMES LIVE HERE, NOT THE VALIDATOR. The bundled HS256 stand-in is
// still the gateway's alone (services/api-gateway/internal/middleware) and
// nothing in this package will ever accept an HMAC-signed token — see
// asymmetricAlgs. What sits here are the two strings the MINTER
// (internal/devtoken) writes and the VALIDATOR requires, in the one place both
// can import. They were a single contract expressed twice in a repository whose
// standing rule is that a copied constant is how a fix stops spreading; a drift
// between the two halves is a dev estate where nothing authenticates and the
// error says only "unauthenticated".
//
// WHY A SYMMETRIC TOKEN IS SCOPED AT ALL. An HMAC secret is a credential every
// holder can forge with, so `iss` and `aud` here are NOT the security property
// they are on the OIDC path, where the issuer is bound to independently fetched
// key material. What they do buy is real and narrow: any OTHER token-shaped
// blob signed with the same secret — a value reused as a request-signing key,
// a strategy HMAC, a partner system handed "the kanz dev secret" — no longer
// authenticates a caller here just because the bytes verify. That is the
// realistic failure for a shared secret in a dev estate, and it is what the
// gateway's HS256 arm now refuses.
//
// WHAT THEY DO NOT BUY, SAID PLAINLY SO NOBODY BANKS ON IT: these are FIXED,
// not per-deployment, so they do not stop a token being replayed at a second
// kanz gateway configured with the same secret. The answer to two gateways
// sharing an HMAC key is not a better audience string — it is OIDC
// (API_GATEWAY_OIDC_ISSUER), where kanz holds no signing key and mints nothing.
// The HS256 arm is reachable only with API_GATEWAY_ALLOW_DEV_HS256=true for
// exactly that reason.
const (
	// DevHS256Issuer is the `iss` a dev token carries: the minter itself. There
	// is no identity provider on this path — the issuer IS whoever holds the
	// secret — so the claim names the tool rather than pretending to an
	// authority it does not have.
	DevHS256Issuer = "kanz-devtoken"

	// DevHS256Audience is the `aud` a dev token is minted for: the api-gateway's
	// bundled validator, and nothing else.
	DevHS256Audience = "kanz-api-gateway"
)
