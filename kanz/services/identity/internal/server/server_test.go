package server

// THE CREDENTIAL SURFACE (#364).
//
// Every test here guards a property whose failure is invisible from outside: the
// login still works, the tests still pass, and the only difference is that the
// endpoint has become an oracle — for who exists, for whether an invite was
// offered, or for a password given enough attempts.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/eighred/kanz/internal/clientip"
	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/internal/revocation"
	"github.com/eighred/kanz/pkg/auth"
)

// --- doubles ----------------------------------------------------------------

type fakeStore struct {
	users     map[string]*identity.User
	redeemErr error
	redeemed  *identity.User
	// redeemCalls counts store hits, so a test can prove the credential check
	// runs BEFORE the single-use invitation is spent.
	redeemCalls int
	updated     map[string]identity.Hash
	// revocations is what the feed serves; revocationsErr makes the store fail,
	// so a test can prove the handler answers 503 rather than an empty list.
	revocations    []revocation.Entry
	revocationsErr error
}

func (f *fakeStore) Revocations(context.Context) ([]revocation.Entry, error) {
	if f.revocationsErr != nil {
		return nil, f.revocationsErr
	}
	return f.revocations, nil
}

func (f *fakeStore) UserBySubject(_ context.Context, subject string) (*identity.User, error) {
	u, ok := f.users[subject]
	if !ok {
		return nil, identity.ErrUserNotFound
	}
	return u, nil
}

func (f *fakeStore) UpdateCredential(_ context.Context, subject string, cred identity.Hash, _ time.Time) error {
	if f.updated == nil {
		f.updated = map[string]identity.Hash{}
	}
	f.updated[subject] = cred
	return nil
}

func (f *fakeStore) Redeem(_ context.Context, _ string, _ identity.Hash, _ time.Time) (*identity.User, error) {
	f.redeemCalls++
	if f.redeemErr != nil {
		return nil, f.redeemErr
	}
	return f.redeemed, nil
}

type fakeMinter struct{ minted int }

func (m *fakeMinter) Mint(u *identity.User) (string, time.Time, error) {
	m.minted++
	return "token-for-" + u.Subject, time.Now().Add(time.Hour), nil
}

// allowN permits the first n attempts per key, then refuses.
type allowN struct {
	n    int
	seen map[string]int
}

func (a *allowN) Allow(k string) bool {
	if a.seen == nil {
		a.seen = map[string]int{}
	}
	a.seen[k]++
	return a.seen[k] <= a.n
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testServer(t *testing.T, st *fakeStore, lim Limiter) (*Server, *fakeMinter) {
	t.Helper()
	m := &fakeMinter{}
	if lim == nil {
		lim = &allowN{n: 100}
	}
	// THE DEFAULT SERVER TRUSTS THE PEER httptest GIVES EVERY REQUEST, so the
	// tests below exercise the deployed shape: a forwarded address arriving from
	// the web-bff, which is a peer this deployment named. The tests that matter
	// most for #888 build their own server with a DIFFERENT peer, to prove the
	// same header is ignored when it did not come from that one.
	s, err := New(st, m, lim, func() any { return map[string]any{"keys": []any{}} }, "https://identity.test", quiet(),
		WithClientIP(resolverTrusting(t, testPeerIP)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, m
}

// testPeerIP is the RemoteAddr httptest.NewRequest stamps on every request
// (192.0.2.1:1234, from the TEST-NET-1 range). Naming it makes the trusted-peer
// set in these tests a deliberate value rather than a coincidence.
const testPeerIP = "192.0.2.1"

func resolverTrusting(t *testing.T, cidrs ...string) *clientip.Resolver {
	t.Helper()
	r, err := clientip.NewResolver(ClientIPHeader, cidrs)
	if err != nil {
		t.Fatalf("clientip.NewResolver(%v): %v", cidrs, err)
	}
	if !r.Trusts() {
		t.Fatalf("resolver built from %v trusts nothing — the test would prove the header is "+
			"ignored for the wrong reason", cidrs)
	}
	return r
}

func post(t *testing.T, s *Server, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	mux := http.NewServeMux()
	s.Routes(mux)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

func userWith(t *testing.T, subject, password string, status identity.Status) *identity.User {
	t.Helper()
	h, err := identity.HashCredential(password)
	if err != nil {
		t.Fatalf("HashCredential: %v", err)
	}
	return &identity.User{
		Subject: subject, Tenant: "acme", Roles: []string{"kanz-trader"},
		Portfolios: []string{"pf-1"}, Credential: h, Status: status,
	}
}

// --- login ------------------------------------------------------------------

func TestAValidCredentialIssuesAToken(t *testing.T) {
	st := &fakeStore{users: map[string]*identity.User{
		"user:alice": userWith(t, "user:alice", "correct password", identity.StatusActive),
	}}
	s, m := testServer(t, st, nil)

	rr := post(t, s, "/login", loginRequest{Subject: "user:alice", Credential: "correct password"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	var got tokenResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Token == "" || got.Subject != "user:alice" || got.Tenant != "acme" {
		t.Fatalf("response = %+v, want a token for user:alice/acme", got)
	}
	if m.minted != 1 {
		t.Errorf("minted %d tokens, want 1", m.minted)
	}
}

// AN UNKNOWN SUBJECT AND A WRONG PASSWORD ARE INDISTINGUISHABLE.
//
// Same status, same body. A caller who can tell them apart can enumerate the
// platform's users — which here is a fund's traders and operators, a list worth
// having before a single password is guessed.
func TestAnUnknownSubjectAndAWrongPasswordAnswerIdentically(t *testing.T) {
	st := &fakeStore{users: map[string]*identity.User{
		"user:alice": userWith(t, "user:alice", "correct password", identity.StatusActive),
	}}
	s, _ := testServer(t, st, nil)

	unknown := post(t, s, "/login", loginRequest{Subject: "user:nobody", Credential: "whatever"})
	wrong := post(t, s, "/login", loginRequest{Subject: "user:alice", Credential: "not it"})

	if unknown.Code != http.StatusUnauthorized || wrong.Code != http.StatusUnauthorized {
		t.Fatalf("statuses = %d / %d, want 401 / 401", unknown.Code, wrong.Code)
	}
	if unknown.Body.String() != wrong.Body.String() {
		t.Fatalf("bodies differ:\n  unknown subject: %s  wrong password : %s\n"+
			"Any difference is a user-enumeration oracle.", unknown.Body.String(), wrong.Body.String())
	}
	if strings.Contains(strings.ToLower(unknown.Body.String()), "unknown") ||
		strings.Contains(strings.ToLower(unknown.Body.String()), "no such") {
		t.Errorf("the response names the reason: %s", unknown.Body.String())
	}
}

// A DISABLED ACCOUNT IS REFUSED — and only after its credential is checked, so
// its existence is not detectable without knowing the password.
func TestADisabledAccountCannotLogIn(t *testing.T) {
	st := &fakeStore{users: map[string]*identity.User{
		"user:bob": userWith(t, "user:bob", "correct password", identity.StatusDisabled),
	}}
	s, m := testServer(t, st, nil)

	rr := post(t, s, "/login", loginRequest{Subject: "user:bob", Credential: "correct password"})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — a disabled account must not obtain new authority", rr.Code)
	}
	if m.minted != 0 {
		t.Fatalf("a token was minted for a disabled account")
	}
}

// THE LIMITER IS CONSULTED, and exhausting it answers 429 rather than 401.
//
// Without this the endpoint is an offline guessing oracle that answers as fast
// as Argon2id allows — and nothing upstream provides a bound, because the
// gateway's quota middleware runs after authentication and never sees an
// anonymous request.
func TestLoginIsRateLimited(t *testing.T) {
	st := &fakeStore{users: map[string]*identity.User{
		"user:alice": userWith(t, "user:alice", "correct password", identity.StatusActive),
	}}
	s, _ := testServer(t, st, &allowN{n: 2})

	for i := range 2 {
		if rr := post(t, s, "/login", loginRequest{Subject: "user:alice", Credential: "wrong"}); rr.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i, rr.Code)
		}
	}
	rr := post(t, s, "/login", loginRequest{Subject: "user:alice", Credential: "correct password"})
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status after the budget = %d, want 429.\n\n"+
			"Note the third attempt used the CORRECT password: the limiter must refuse before "+
			"the credential is examined, or it bounds nothing.", rr.Code)
	}
}

// A NIL LIMITER IS REFUSED AT CONSTRUCTION rather than defaulted to permissive.
func TestAServerCannotBeBuiltWithoutALimiter(t *testing.T) {
	st := &fakeStore{}
	if _, err := New(st, &fakeMinter{}, nil, func() any { return nil }, "https://identity.test", quiet()); err == nil {
		t.Fatal("New accepted a nil limiter — an unbounded credential-guessing endpoint is " +
			"indistinguishable from a working one until someone uses it")
	}
	if _, err := New(nil, &fakeMinter{}, &allowN{n: 1}, func() any { return nil }, "https://identity.test", quiet()); err == nil {
		t.Error("New accepted a nil store")
	}
	if _, err := New(st, nil, &allowN{n: 1}, func() any { return nil }, "https://identity.test", quiet()); err == nil {
		t.Error("New accepted a nil minter")
	}
}

// A SUCCESSFUL LOGIN UPGRADES A WEAK CREDENTIAL IN PLACE.
//
// The fixture derives a REAL hash under deliberately weaker parameters, rather
// than doctoring a current one's parameter string — a doctored hash no longer
// matches its own digest, so the login fails and the upgrade branch is never
// reached. The first version of this test did exactly that and asserted nothing.
//
// The PHC format is spelled out here because there is no exported way to mint a
// weak hash, and mustn't be: production has one cost, and the only reason to
// construct another is to prove the upgrade path works.
func TestASuccessfulLoginRehashesAnOlderCredential(t *testing.T) {
	const pw = "correct password"
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte(pw), salt, 1, 8*1024, 1, 32)
	b64 := base64.RawStdEncoding.EncodeToString
	weak := identity.Hash(fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, 8*1024, 1, 1, b64(salt), b64(key)))

	// NON-VACUITY, both halves: it must verify (or the login fails for the wrong
	// reason) and it must be weaker than current (or there is nothing to upgrade).
	if err := identity.Verify(weak, pw); err != nil {
		t.Fatalf("the weak fixture does not verify: %v — the login would fail before reaching "+
			"the rehash branch, and this test would prove nothing", err)
	}
	if !identity.NeedsRehash(weak) {
		t.Fatal("the fixture is not weaker than the current parameters")
	}

	u := userWith(t, "user:alice", pw, identity.StatusActive)
	u.Credential = weak
	st := &fakeStore{users: map[string]*identity.User{"user:alice": u}}
	s, _ := testServer(t, st, nil)

	rr := post(t, s, "/login", loginRequest{Subject: "user:alice", Credential: pw})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}

	upgraded, ok := st.updated["user:alice"]
	if !ok {
		t.Fatal("the credential was not rewritten.\n\n" +
			"A successful login is the only moment the plaintext exists to re-derive with, so " +
			"without this the accounts that predate a cost raise stay on the weakest parameters " +
			"forever — and those are the oldest accounts.")
	}
	if identity.NeedsRehash(upgraded) {
		t.Error("the rewritten credential is STILL weaker than current — the upgrade wrote back " +
			"the old parameters")
	}
	if err := identity.Verify(upgraded, pw); err != nil {
		t.Fatalf("the rewritten credential does not verify the same password: %v — the user is "+
			"now locked out by a hardening step", err)
	}
}

// A CURRENT CREDENTIAL IS NOT REWRITTEN, so an ordinary login stays a read path.
func TestALoginWithACurrentCredentialWritesNothing(t *testing.T) {
	st := &fakeStore{users: map[string]*identity.User{
		"user:alice": userWith(t, "user:alice", "correct password", identity.StatusActive),
	}}
	s, _ := testServer(t, st, nil)

	if rr := post(t, s, "/login", loginRequest{Subject: "user:alice", Credential: "correct password"}); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if len(st.updated) != 0 {
		t.Fatalf("a current credential was rewritten (%v) — every login would become a write, "+
			"on the one endpoint an attacker can call without authenticating", st.updated)
	}
}

// --- redemption -------------------------------------------------------------

func TestRedeemingAnInviteIssuesAToken(t *testing.T) {
	st := &fakeStore{redeemed: userWith(t, "user:carol", "unused", identity.StatusActive)}
	s, m := testServer(t, st, nil)

	rr := post(t, s, "/invites/redeem", redeemRequest{Token: "raw-token", Credential: "chosen password"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if m.minted != 1 {
		t.Errorf("minted %d, want 1 — redemption signs the invitee straight in", m.minted)
	}
}

// EVERY REFUSAL LOOKS THE SAME to someone holding only a link.
// The credential is policy-conformant on purpose: ValidateCredential runs
// BEFORE the invite is looked up, so a short one would answer 400 and never
// reach the refusal being compared here.
//
// THAT ORDERING IS SAFE AND SHOULD NOT BE "FIXED". The 400 depends only on the
// credential the caller invented, never on any invite state, so it cannot be
// used to probe whether an invitation exists — and checking first means a
// rejected password does not spend a single-use invitation.
func TestEveryInviteRefusalAnswersIdentically(t *testing.T) {
	var bodies []string
	for _, err := range []error{
		identity.ErrInviteNotFound,
		identity.ErrInviteExpired,
		identity.ErrInviteAlreadyRedeemed,
	} {
		st := &fakeStore{redeemErr: err}
		s, _ := testServer(t, st, nil)
		rr := post(t, s, "/invites/redeem", redeemRequest{Token: "x", Credential: "a-valid-passphrase"})
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%v: status = %d, want 401", err, rr.Code)
		}
		bodies = append(bodies, rr.Body.String())
	}
	for i := 1; i < len(bodies); i++ {
		if bodies[i] != bodies[0] {
			t.Fatalf("refusal bodies differ:\n  %s  %s\n"+
				"Distinguishing 'expired' from 'unknown' confirms an account was offered to somebody.",
				bodies[0], bodies[i])
		}
	}
}

func TestRedeemingWithoutACredentialIsRefused(t *testing.T) {
	st := &fakeStore{redeemed: userWith(t, "user:carol", "x", identity.StatusActive)}
	s, m := testServer(t, st, nil)

	rr := post(t, s, "/invites/redeem", redeemRequest{Token: "raw", Credential: ""})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — an account with an empty credential is an account "+
			"anyone can use", rr.Code)
	}
	if m.minted != 0 {
		t.Error("a token was minted for an empty credential")
	}
}

// --- shape ------------------------------------------------------------------

func TestMalformedAndOversizedBodiesAreRefused(t *testing.T) {
	s, _ := testServer(t, &fakeStore{}, nil)
	mux := http.NewServeMux()
	s.Routes(mux)

	for name, body := range map[string]string{
		"not json":      "{",
		"unknown field": `{"subject":"a","credential":"b","admin":true}`,
		"oversized":     `{"subject":"` + strings.Repeat("a", 9<<10) + `"}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, rr.Code)
		}
	}
}

func TestTheJWKSIsServed(t *testing.T) {
	s, _ := testServer(t, &fakeStore{}, nil)
	mux := http.NewServeMux()
	s.Routes(mux)

	req := httptest.NewRequest(http.MethodGet, "/jwks.json", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the gateway cannot verify a single token without this", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content-type = %q", ct)
	}
}

// postFrom is post() with the web-bff's forwarded client address set. Note that
// httptest gives every request the SAME RemoteAddr, which is what makes these
// tests able to tell "the header is honoured" from "the peer happens to differ".
func postFrom(t *testing.T, s *Server, path string, body any, clientIP string) *httptest.ResponseRecorder {
	t.Helper()
	return postFromPeer(t, s, path, body, clientIP, "")
}

// postFromPeer is postFrom with the TCP peer chosen too. The peer is what decides
// whether the forwarded header is honoured at all (#888), so a test about
// spoofing has to be able to move it.
func postFromPeer(t *testing.T, s *Server, path string, body any, clientIP, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	mux := http.NewServeMux()
	s.Routes(mux)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	if clientIP != "" {
		req.Header.Set(ClientIPHeader, clientIP)
	}
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

// THE FORWARDED ADDRESS IS WHAT THE PER-SOURCE BOUND KEYS ON (#364).
//
// Every login arrives through the web-bff, so the peer address is the BFF for
// everyone. If the forwarded header is ignored, the per-source bucket is ONE
// bucket for the whole estate: one attacker's guessing throttles every operator,
// and the attacker's own traffic hides inside the same bucket as legitimate use.
//
// Both requests below carry the same RemoteAddr and differ only in the header,
// so the second assertion fails if the header is not read.
func TestThePerSourceBoundKeysOnTheForwardedAddress(t *testing.T) {
	st := &fakeStore{}
	s, _ := testServer(t, st, &allowN{n: 1})

	if rr := postFrom(t, s, "/login", loginRequest{Subject: "alice", Credential: "x"}, "10.0.0.1"); rr.Code == http.StatusTooManyRequests {
		t.Fatalf("the first attempt was throttled: %d", rr.Code)
	}
	if rr := postFrom(t, s, "/login", loginRequest{Subject: "bob", Credential: "x"}, "10.0.0.2"); rr.Code == http.StatusTooManyRequests {
		t.Fatal("a DIFFERENT caller was throttled by the first caller's attempt.\n\n" +
			"Both requests share a RemoteAddr and differ only in " + ClientIPHeader +
			", so this means the forwarded address is being ignored and every login in " +
			"the estate shares one bucket.")
	}
	if rr := postFrom(t, s, "/login", loginRequest{Subject: "carol", Credential: "x"}, "10.0.0.1"); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("a second attempt from 10.0.0.1 = %d, want 429.\n\n"+
			"Without a per-source bound one address sprays a single guess across thousands "+
			"of accounts — which is how leaked password lists are actually used — and never "+
			"exhausts a per-account bucket.", rr.Code)
	}
}

// AND THE SUBJECT BOUND STILL HOLDS INDEPENDENTLY, so an attacker rotating
// source addresses cannot grind one account.
func TestOneAccountIsBoundedAcrossManySources(t *testing.T) {
	st := &fakeStore{}
	s, _ := testServer(t, st, &allowN{n: 1})

	if rr := postFrom(t, s, "/login", loginRequest{Subject: "alice", Credential: "x"}, "10.0.0.1"); rr.Code == http.StatusTooManyRequests {
		t.Fatalf("the first attempt was throttled: %d", rr.Code)
	}
	if rr := postFrom(t, s, "/login", loginRequest{Subject: "alice", Credential: "x"}, "10.0.0.99"); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("a second guess at alice from a fresh address = %d, want 429.\n\n"+
			"A bound that only counts sources is no bound at all against anyone holding "+
			"more than one address.", rr.Code)
	}
}

// REDEMPTION IS BOUNDED PER SOURCE TOO. It has no subject to key on — the
// invite token IS the secret being guessed — so the forwarded address is the
// only axis available, and keying it on the BFF would make it estate-wide.
func TestRedemptionIsBoundedPerSource(t *testing.T) {
	st := &fakeStore{redeemErr: identity.ErrInviteNotFound}
	s, _ := testServer(t, st, &allowN{n: 1})

	if rr := postFrom(t, s, "/invites/redeem", redeemRequest{Token: "a", Credential: "a-valid-passphrase"}, "10.0.0.1"); rr.Code == http.StatusTooManyRequests {
		t.Fatalf("the first redemption was throttled: %d", rr.Code)
	}
	if rr := postFrom(t, s, "/invites/redeem", redeemRequest{Token: "b", Credential: "a-valid-passphrase"}, "10.0.0.2"); rr.Code == http.StatusTooManyRequests {
		t.Fatal("a different invitee was throttled by someone else's redemption — " +
			"the forwarded address is being ignored")
	}
	if rr := postFrom(t, s, "/invites/redeem", redeemRequest{Token: "c", Credential: "a-valid-passphrase"}, "10.0.0.1"); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("a second redemption from 10.0.0.1 = %d, want 429 — invite tokens would "+
			"otherwise be guessable at line rate", rr.Code)
	}
}

// A SPOOFED FORWARDED ADDRESS FROM AN UNTRUSTED PEER GETS NO FRESH BUCKET (#888).
//
// This is the property the service did NOT have. The address was read from
// ClientIPHeader unconditionally, on the stated premise that a NetworkPolicy made
// the web-bff the only caller able to reach :8087. It does not:
// infra/security/runtime/network-policies.yaml admits the ingress-nginx
// namespace, the api-gateway (for /jwks.json), the web-bff, and the
// kanz-observability namespace (/metrics shares this listener) — four peers.
//
// Any one of them could send a different X-Kanz-Client-IP per attempt. Every
// attempt would then land in a fresh bucket and the per-source half of
// allowAttempt would stop existing, while the metrics showed a wide spread of
// well-behaved clients. The per-subject half still bounds guessing at ONE
// account, so what this recovers is the bound on credential STUFFING: one guess
// sprayed across thousands of accounts.
//
// The server here trusts a peer that is NOT the one making these requests, so
// the header is a string the caller typed and the resolver must ignore it.
func TestASpoofedForwardedAddressFromAnUntrustedPeerSharesOneBucket(t *testing.T) {
	s, err := New(&fakeStore{}, &fakeMinter{}, &allowN{n: 1},
		func() any { return map[string]any{"keys": []any{}} }, "https://identity.test", quiet(),
		// The BFF's address in this deployment. The attacker below is not it.
		WithClientIP(resolverTrusting(t, "10.42.0.0/16")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const attacker = "198.51.100.7:5555" // TEST-NET-2, outside the trusted set

	if rr := postFromPeer(t, s, "/login", loginRequest{Subject: "alice", Credential: "x"},
		"10.0.0.1", attacker); rr.Code == http.StatusTooManyRequests {
		t.Fatalf("the first attempt was throttled: %d", rr.Code)
	}
	// Same peer, a DIFFERENT claimed address, and a different subject so the
	// per-subject axis cannot be what refuses it. Only the source axis can.
	rr := postFromPeer(t, s, "/login", loginRequest{Subject: "bob", Credential: "x"},
		"10.0.0.2", attacker)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("a second attempt from the same untrusted peer claiming a new address = %d, "+
			"want 429.\n\n"+
			"The peer %s is NOT in the trusted set, so "+ClientIPHeader+" arriving from it is a "+
			"string the caller typed. Honouring it gives every attempt a fresh bucket and ends "+
			"the per-source half of the credential bound — the half that stops one guess being "+
			"sprayed across thousands of accounts. The subject axis cannot cover this: the two "+
			"attempts name different subjects.", rr.Code, attacker)
	}
}

// AND THE HEADER IS IGNORED ENTIRELY WHEN NO PEER IS TRUSTED (#888).
//
// A deployment that names no IDENTITY_TRUSTED_PROXIES gets the peer address for
// everybody. That is a loss of PRECISION — behind the BFF the estate shares one
// per-source bucket — and it is the direction a missing configuration must fail
// in. The opposite default would make an unconfigured deployment an open oracle,
// which is exactly the state this issue found.
func TestWithNoTrustedPeerTheForwardedAddressIsIgnored(t *testing.T) {
	s, err := New(&fakeStore{}, &fakeMinter{}, &allowN{n: 1},
		func() any { return map[string]any{"keys": []any{}} }, "https://identity.test", quiet())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.clientIP.Trusts() {
		t.Fatal("a server built with no WithClientIP option trusts a peer — the zero value must " +
			"honour no header at all")
	}

	if rr := postFrom(t, s, "/login", loginRequest{Subject: "alice", Credential: "x"}, "10.0.0.1"); rr.Code == http.StatusTooManyRequests {
		t.Fatalf("the first attempt was throttled: %d", rr.Code)
	}
	if rr := postFrom(t, s, "/login", loginRequest{Subject: "bob", Credential: "x"}, "10.0.0.2"); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("a second attempt claiming a fresh address = %d, want 429 — an unconfigured "+
			"deployment must attribute every caller to its peer rather than to whatever it "+
			"claims", rr.Code)
	}
}

func TestAServerCannotBeBuiltWithoutAnIssuer(t *testing.T) {
	if _, err := New(&fakeStore{}, &fakeMinter{}, &allowN{n: 1}, func() any { return nil }, "", quiet()); err == nil {
		t.Fatal("New accepted an empty issuer — the discovery document would advertise one that " +
			"matches nothing, and every OIDC verifier refuses a document whose issuer disagrees " +
			"with the URL it was discovered from")
	}
}

// THE GATEWAY CAN VERIFY WHAT THIS SERVICE ISSUES, DISCOVERED FROM THE ISSUER
// ALONE (#364).
//
// This is the contract that makes login mean anything: a token is only useful if
// the api-gateway accepts it. Every previous test here used a fake minter, so
// none of them could have caught a real mismatch in algorithm, key id, curve,
// claim names, issuer, audience — or a discovery document the verifier cannot
// follow.
//
// It deliberately configures the verifier with NOTHING BUT THE ISSUER, which is
// the ordinary way an IdP is wired. Without the discovery endpoint this fails at
// the first fetch, which is exactly the trap an operator would otherwise hit.
func TestTheGatewayVerifiesATokenThisServiceIssues(t *testing.T) {
	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	// The issuer must equal the URL the document is served from, so the server is
	// built after the listener exists and the mux is filled in place.
	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	signer, err := identity.NewSigner(key, ts.URL, "kanz-api", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	redeemed := userWith(t, "user:alice", "pw", identity.StatusActive)
	st := &fakeStore{redeemed: redeemed}
	s, err := New(st, signer, &allowN{n: 10}, func() any { return signer.JWKS() }, ts.URL, quiet())
	if err != nil {
		t.Fatal(err)
	}
	s.Routes(mux)

	// Obtain a token the way a browser does.
	body, _ := json.Marshal(redeemRequest{Token: "an-invite", Credential: "a-valid-passphrase"})
	resp, err := ts.Client().Post(ts.URL+"/invites/redeem", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("redeem = %d, want 200", resp.StatusCode)
	}
	var issued tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&issued); err != nil {
		t.Fatal(err)
	}

	// The gateway's verifier, given only the issuer.
	authn, err := auth.NewOIDCAuthenticator(auth.OIDCConfig{
		Issuer:     ts.URL,
		Audience:   "kanz-api",
		HTTPClient: ts.Client(),
	})
	if err != nil {
		t.Fatalf("NewOIDCAuthenticator: %v", err)
	}
	p, err := authn.Authenticate(context.Background(), issued.Token)
	if err != nil {
		t.Fatalf("the gateway REFUSED a token this service just issued: %v\n\n"+
			"Login would succeed and every API call would 401 — the least debuggable "+
			"failure this pair can have.", err)
	}

	if p.Subject != "user:alice" || p.Tenant != "acme" {
		t.Errorf("principal = %s/%s, want user:alice/acme", p.Subject, p.Tenant)
	}
	if !p.HasRole("kanz-trader") {
		t.Errorf("roles = %v, want to carry kanz-trader", p.Roles)
	}
	// The portfolio scope is what the OMS authorizes cancels and amends against;
	// dropping it silently is #225's defect.
	if len(p.Portfolios) != 1 || p.Portfolios[0] != "pf-1" {
		t.Errorf("portfolios = %v, want [pf-1] — without this the OMS refuses every "+
			"cancel and amend this caller issues", p.Portfolios)
	}
}

// A TOO-SHORT CREDENTIAL IS REFUSED, AND THE INVITATION SURVIVES (#364).
//
// The browser form checks the same rule, but that is a courtesy — anyone can
// POST straight at this route, so the policy has to hold here.
//
// The second half is the part worth asserting: the check runs BEFORE the store
// is touched. An invitation is single-use, so burning it on a rejected password
// would leave the invitee with no account and no way to make one, needing an
// operator to issue a fresh invitation because they typed something short.
func TestATooShortCredentialIsRefusedWithoutBurningTheInvitation(t *testing.T) {
	st := &fakeStore{redeemed: userWith(t, "user:alice", "irrelevant", identity.StatusActive)}
	s, minter := testServer(t, st, &allowN{n: 10})

	rr := postFrom(t, s, "/invites/redeem", redeemRequest{Token: "an-invite", Credential: "short"}, "10.0.0.1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("redeem with a 5-character credential = %d, want 400", rr.Code)
	}
	if minter.minted != 0 {
		t.Errorf("a token was minted for a refused credential")
	}
	if st.redeemCalls != 0 {
		t.Fatalf("the store was asked to redeem (%d calls) despite the credential being refused.\n\n"+
			"The invitation is single-use: burning it here leaves the invitee unable to create an "+
			"account at all, and needing an operator to issue a new one, because they typed "+
			"something short.", st.redeemCalls)
	}

	// And a credential that meets the policy still goes through.
	ok := postFrom(t, s, "/invites/redeem",
		redeemRequest{Token: "an-invite", Credential: "a-perfectly-fine-passphrase"}, "10.0.0.1")
	if ok.Code != http.StatusOK {
		t.Fatalf("redeem with an acceptable credential = %d, want 200", ok.Code)
	}
}
