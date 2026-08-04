package middleware

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/auth"
)

const testSecret = "test-secret"

// mintJWT builds a VALID HS256 token for tests, signed with testSecret.
//
// It fills in the three claims the validator now requires whenever the case
// left them zero — exp, iss and aud (#242) — so that a test about roles or
// portfolios stays about roles and portfolios. A case that is ABOUT one of the
// three sets it explicitly and gets it verbatim; a case about their ABSENCE
// cannot use this helper at all and uses mintRawJWT below, which is the point
// of having two.
func mintJWT(t *testing.T, claims jwtClaims) string {
	t.Helper()
	if claims.Expiry == 0 {
		claims.Expiry = time.Now().Add(time.Hour).Unix()
	}
	if claims.Issuer == "" {
		claims.Issuer = auth.DevHS256Issuer
	}
	if len(claims.Audience) == 0 {
		claims.Audience = jwtAudience{auth.DevHS256Audience}
	}
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	header := enc(map[string]string{"alg": "HS256", "typ": "JWT"})
	payload := enc(claims)
	return signJWT(t, header, payload)
}

// mintRawJWT signs an ARBITRARY header and payload, so a test can express what
// the typed helper cannot: a claim that is absent rather than zero, an `aud`
// that is a bare string rather than an array, or a header claiming `alg: none`.
// These are the shapes an attacker sends, and the typed struct rounds every one
// of them off into something well-formed.
func mintRawJWT(t *testing.T, header, payload map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return signJWT(t, enc(header), enc(payload))
}

func signJWT(t *testing.T, header, payload string) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(header + "." + payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return header + "." + payload + "." + sig
}

// validPayload is the claim set mintRawJWT cases start from and then break in
// exactly one place, so a refusal is attributable to that one thing.
func validPayload() map[string]any {
	return map[string]any{
		"sub":    "user-1",
		"tenant": "acme",
		"iss":    auth.DevHS256Issuer,
		"aud":    auth.DevHS256Audience,
		"exp":    time.Now().Add(time.Hour).Unix(),
	}
}

func hs256Header() map[string]any { return map[string]any{"alg": "HS256", "typ": "JWT"} }

func TestJWTAuthenticator(t *testing.T) {
	a := NewJWTAuthenticator(testSecret)

	t.Run("valid", func(t *testing.T) {
		tok := mintJWT(t, jwtClaims{Subject: "user-1", Tenant: "acme", Roles: []string{"risk.read"}})
		p, err := a.Authenticate(tok)
		if err != nil {
			t.Fatal(err)
		}
		if p.Subject != "user-1" || p.Tenant != "acme" || !p.HasRole("risk.read") {
			t.Errorf("principal = %+v", p)
		}
	})
	t.Run("bad signature", func(t *testing.T) {
		tok := mintJWT(t, jwtClaims{Subject: "u"})
		if _, err := NewJWTAuthenticator("other-secret").Authenticate(tok); err == nil {
			t.Error("want error for wrong-secret signature")
		}
	})
	t.Run("expired", func(t *testing.T) {
		tok := mintJWT(t, jwtClaims{Subject: "u", Expiry: time.Now().Add(-time.Hour).Unix()})
		if _, err := a.Authenticate(tok); err == nil {
			t.Error("want error for expired token")
		}
	})
	t.Run("malformed", func(t *testing.T) {
		if _, err := a.Authenticate("not.a.jwt.at.all"); err == nil {
			t.Error("want error for malformed token")
		}
	})
}

// THE #242 DEFECT, ASSERTED DIRECTLY. The check was `c.Expiry != 0 && expired`,
// so a payload that simply OMITTED `exp` skipped it and the token was valid
// forever — against a symmetric secret with no revocation path, on the
// platform's sole identity authority. Anyone who could mint one once could
// authenticate with it indefinitely.
//
// Both spellings are covered because they are one bug seen twice: the claim
// absent, and the claim present as 0. The typed helper cannot express the first
// (a zero int64 marshals to `"exp":0`), which is why mintRawJWT exists.
func TestJWTAuthenticator_MissingExpiryIsRefused(t *testing.T) {
	a := NewJWTAuthenticator(testSecret)

	t.Run("exp absent", func(t *testing.T) {
		p := validPayload()
		delete(p, "exp")
		if _, err := a.Authenticate(mintRawJWT(t, hs256Header(), p)); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("got %v, want ErrUnauthenticated — a token with no exp is a permanent credential", err)
		}
	})
	t.Run("exp zero", func(t *testing.T) {
		p := validPayload()
		p["exp"] = 0
		if _, err := a.Authenticate(mintRawJWT(t, hs256Header(), p)); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("got %v, want ErrUnauthenticated", err)
		}
	})
	// The control: identical token WITH an exp authenticates, so the two cases
	// above are failing on the expiry and not on something else being wrong.
	t.Run("exp present", func(t *testing.T) {
		if _, err := a.Authenticate(mintRawJWT(t, hs256Header(), validPayload())); err != nil {
			t.Fatalf("a well-formed token was refused (%v) — the cases above prove nothing", err)
		}
	})
}

// nbf: a token that is not yet valid must be refused. Untested until #242, and
// it is the branch an attacker probes precisely because it is the one nobody
// exercises — a validator that ignores nbf accepts a post-dated token the
// moment it is minted, which is the opposite of what post-dating it meant.
func TestJWTAuthenticator_NotBefore(t *testing.T) {
	a := NewJWTAuthenticator(testSecret)

	t.Run("future nbf refused", func(t *testing.T) {
		p := validPayload()
		p["nbf"] = time.Now().Add(time.Hour).Unix()
		if _, err := a.Authenticate(mintRawJWT(t, hs256Header(), p)); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("got %v, want ErrUnauthenticated for a not-yet-valid token", err)
		}
	})
	t.Run("past nbf accepted", func(t *testing.T) {
		p := validPayload()
		p["nbf"] = time.Now().Add(-time.Hour).Unix()
		if _, err := a.Authenticate(mintRawJWT(t, hs256Header(), p)); err != nil {
			t.Fatalf("a token whose nbf has passed was refused: %v", err)
		}
	})
	// nbf stays OPTIONAL (RFC 7519 §4.1.5) — unlike a missing exp, an absent
	// nbf widens nothing. Asserted so nobody later "fixes" the asymmetry by
	// making both mandatory and breaks every token this estate mints.
	t.Run("absent nbf accepted", func(t *testing.T) {
		if _, err := a.Authenticate(mintRawJWT(t, hs256Header(), validPayload())); err != nil {
			t.Fatalf("a token with no nbf was refused: %v", err)
		}
	})
}

// alg=none, and the reason there are TWO cases.
//
// The issue's claim — that this validator was never vulnerable to alg confusion
// — is TRUE and is verified by the first case: the HMAC is computed
// unconditionally over header.payload, so an unsigned token's empty signature
// fails hmac.Equal before `alg` is ever read. Nothing about the fix depends on
// that, but a test claiming to cover alg=none must not pass for a reason it did
// not check.
//
// The second case is what the explicit header check adds: a token that says
// `alg: none` and CARRIES A VALID HMAC anyway. Only an attacker holding the
// secret can produce one — so this closes no hole today — and that is exactly
// why the check is stated rather than left emergent: the next person to add an
// algorithm branch or an early return for an unsigned token has to delete a
// line that says no.
func TestJWTAuthenticator_AlgNoneRefused(t *testing.T) {
	a := NewJWTAuthenticator(testSecret)

	t.Run("unsigned", func(t *testing.T) {
		enc := func(v any) string {
			b, _ := json.Marshal(v)
			return base64.RawURLEncoding.EncodeToString(b)
		}
		tok := enc(map[string]any{"alg": "none", "typ": "JWT"}) + "." + enc(validPayload()) + "."
		if _, err := a.Authenticate(tok); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("got %v, want ErrUnauthenticated for an unsigned alg=none token", err)
		}
	})
	t.Run("alg none with a real signature", func(t *testing.T) {
		tok := mintRawJWT(t, map[string]any{"alg": "none", "typ": "JWT"}, validPayload())
		if _, err := a.Authenticate(tok); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("got %v, want ErrUnauthenticated — the header must be inspected, not just the MAC", err)
		}
	})
	t.Run("alg HS512 refused", func(t *testing.T) {
		tok := mintRawJWT(t, map[string]any{"alg": "HS512", "typ": "JWT"}, validPayload())
		if _, err := a.Authenticate(tok); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("got %v, want ErrUnauthenticated — HS256 is the only accepted alg", err)
		}
	})
}

// iss/aud bind a dev token to THIS credential path (#242). A token-shaped blob
// signed with the same shared secret for some other purpose — a request-signing
// key reused, a strategy HMAC, a partner handed "the kanz dev secret" — must not
// authenticate a caller here merely because the bytes verify.
//
// See pkg/auth.DevHS256Issuer for what this does NOT buy: the values are fixed,
// so they do not stop replay at a second kanz gateway sharing the secret. The
// answer to that is OIDC, not a better constant.
func TestJWTAuthenticator_IssuerAndAudienceRequired(t *testing.T) {
	a := NewJWTAuthenticator(testSecret)

	cases := map[string]func(map[string]any){
		"iss absent": func(p map[string]any) { delete(p, "iss") },
		"iss wrong":  func(p map[string]any) { p["iss"] = "some-other-minter" },
		"aud absent": func(p map[string]any) { delete(p, "aud") },
		"aud wrong":  func(p map[string]any) { p["aud"] = "another-gateway" },
		"aud array miss": func(p map[string]any) {
			p["aud"] = []string{"another-gateway", "a-third"}
		},
	}
	for name, break_ := range cases {
		t.Run(name, func(t *testing.T) {
			p := validPayload()
			break_(p)
			if _, err := a.Authenticate(mintRawJWT(t, hs256Header(), p)); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("got %v, want ErrUnauthenticated", err)
			}
		})
	}

	// RFC 7519 allows `aud` as an array; a token that lists the gateway among
	// several audiences is legal and must be accepted. Rejecting it would make a
	// spec-conformant token fail with the same opaque error as a forged one.
	t.Run("aud array containing the gateway", func(t *testing.T) {
		p := validPayload()
		p["aud"] = []string{"something-else", auth.DevHS256Audience}
		if _, err := a.Authenticate(mintRawJWT(t, hs256Header(), p)); err != nil {
			t.Fatalf("a token listing the gateway in an aud ARRAY was refused: %v", err)
		}
	})
}

func TestAuthMiddleware(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	authn := NewJWTAuthenticator(testSecret)

	call := func(mw http.Handler, token string) int {
		req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rr := httptest.NewRecorder()
		mw.ServeHTTP(rr, req)
		return rr.Code
	}

	t.Run("missing token 401", func(t *testing.T) {
		h := Auth(authn, "", nil)(ok)
		if code := call(h, ""); code != http.StatusUnauthorized {
			t.Errorf("code = %d, want 401", code)
		}
	})
	t.Run("valid token passes", func(t *testing.T) {
		h := Auth(authn, "", nil)(ok)
		tok := mintJWT(t, jwtClaims{Subject: "u", Roles: []string{"risk.read"}})
		if code := call(h, tok); code != http.StatusOK {
			t.Errorf("code = %d, want 200", code)
		}
	})
	t.Run("insufficient role 403", func(t *testing.T) {
		h := Auth(authn, "risk.admin", nil)(ok)
		tok := mintJWT(t, jwtClaims{Subject: "u", Roles: []string{"risk.read"}})
		if code := call(h, tok); code != http.StatusForbidden {
			t.Errorf("code = %d, want 403", code)
		}
	})
	t.Run("required role granted", func(t *testing.T) {
		h := Auth(authn, "risk.read", nil)(ok)
		tok := mintJWT(t, jwtClaims{Subject: "u", Roles: []string{"risk.read"}})
		if code := call(h, tok); code != http.StatusOK {
			t.Errorf("code = %d, want 200", code)
		}
	})
	// A nil authenticator means "I cannot establish who you are", which is never
	// "come in". It used to be a pass-through, and POST /v1/orders sits behind
	// this chain. 503, not 401: the fault is the server's, not the caller's.
	t.Run("nil authenticator refuses", func(t *testing.T) {
		var reached bool
		sink := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reached = true
			w.WriteHeader(http.StatusOK)
		})
		h := Auth(nil, "", nil)(sink)

		req := httptest.NewRequest(http.MethodPost, "/v1/orders", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)

		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("code = %d, want 503", rr.Code)
		}
		if reached {
			t.Error("POST /v1/orders reached the handler with no authenticator")
		}
	})
}

// bearerToken's REJECTING branches, which had exactly one case (the wholly
// absent header) until #242.
//
// The scheme is not decoration. A gateway that accepted a bare token, or a
// Basic credential, would authenticate a value the client never meant as a
// bearer credential — a password typed into the wrong field, a Basic header
// forwarded by a proxy — and it would do so silently. Each row below is a
// header a client really sends.
func TestBearerToken_RejectingBranches(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := Auth(NewJWTAuthenticator(testSecret), "", nil)(ok)
	valid := mintJWT(t, jwtClaims{Subject: "u"})

	cases := map[string]struct {
		header string
		want   int
	}{
		"no header at all":     {"", http.StatusUnauthorized},
		"bare token no scheme": {valid, http.StatusUnauthorized},
		"basic scheme":         {"Basic dXNlcjpwYXNz", http.StatusUnauthorized},
		"scheme only":          {"Bearer", http.StatusUnauthorized},
		"scheme and space":     {"Bearer ", http.StatusUnauthorized},
		"wrong scheme word":    {"Token " + valid, http.StatusUnauthorized},
		// The scheme is case-insensitive per RFC 7235 §2.1, and curl/SDKs do send
		// lowercase. Accepting it is correct; asserting it stops a future
		// "tightening" from breaking real clients for no gain.
		"lowercase scheme accepted": {"bearer " + valid, http.StatusOK},
		"canonical":                 {"Bearer " + valid, http.StatusOK},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Errorf("Authorization %q → %d, want %d", tc.header, rr.Code, tc.want)
			}
		})
	}

	// AND THE SAME CASES AGAINST bearerToken ITSELF, because the HTTP-level
	// assertions above DO NOT PIN THIS BRANCH and it is worth saying why rather
	// than leaving a reader to assume they do. Deleting the scheme comparison
	// entirely leaves every case above still returning 401: the extractor slices
	// seven characters off whatever arrived, hands the remainder to the
	// validator, and a mangled token fails the signature check. Same status
	// code, different reason, and a guard that has stopped guarding.
	//
	// Verified by mutation, not assumed: with `!strings.EqualFold(…)` forced
	// false the sub-tests above stay green and these do not.
	t.Run("extractor", func(t *testing.T) {
		for _, tc := range []struct {
			header string
			want   string
			ok     bool
		}{
			{"", "", false},
			{valid, "", false},
			{"Basic dXNlcjpwYXNz", "", false},
			{"Bearer", "", false},
			{"Bearer ", "", false},
			{"Token " + valid, "", false},
			{"Bearer " + valid, valid, true},
			{"bearer " + valid, valid, true},
		} {
			req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			got, ok := bearerToken(req)
			if ok != tc.ok || got != tc.want {
				t.Errorf("bearerToken(%q) = (%q, %v), want (%q, %v)", tc.header, got, ok, tc.want, tc.ok)
			}
		}
	})
}

// TestAuthStashesPrincipal: a downstream handler sees the principal on ctx
// (the per-tenant rate limiter depends on this).
func TestAuthStashesPrincipal(t *testing.T) {
	var got *Principal
	sink := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = PrincipalFromContext(r.Context())
	})
	h := Auth(NewJWTAuthenticator(testSecret), "", nil)(sink)
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("Authorization", "Bearer "+mintJWT(t, jwtClaims{Subject: "u", Tenant: "acme"}))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got == nil || got.Tenant != "acme" {
		t.Errorf("principal not stashed: %+v", got)
	}
}

// The `portfolios` claim must reach the Principal. It is the only source of the
// caller's portfolio entitlement, and the OMS denies by default — so a claim
// that is parsed into nothing does not merely lose a feature, it refuses every
// cancel and amend the platform issues.
func TestJWTAuthenticator_CarriesPortfolioClaim(t *testing.T) {
	a := NewJWTAuthenticator(testSecret)
	tok := mintJWT(t, jwtClaims{
		Subject:    "user-1",
		Tenant:     "acme",
		Roles:      []string{"trader"},
		Portfolios: []string{"pf1", "pf7"},
	})

	p, err := a.Authenticate(tok)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Portfolios) != 2 || p.Portfolios[0] != "pf1" || p.Portfolios[1] != "pf7" {
		t.Fatalf("portfolios = %v, want [pf1 pf7]", p.Portfolios)
	}
}

// A token with no `portfolios` claim yields an EMPTY scope, which the OMS treats
// as deny. Asserted explicitly so nobody later "fixes" the empty case by
// widening it to mean unrestricted — that reintroduces the exact fail-open this
// whole change exists to close.
func TestJWTAuthenticator_AbsentPortfolioClaimIsEmptyNotUnrestricted(t *testing.T) {
	a := NewJWTAuthenticator(testSecret)
	tok := mintJWT(t, jwtClaims{Subject: "user-1", Tenant: "acme", Roles: []string{"trader"}})

	p, err := a.Authenticate(tok)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Portfolios) != 0 {
		t.Fatalf("portfolios = %v, want empty — absence must not be widened", p.Portfolios)
	}
}
