package server

// AUTHENTICATED PROVISIONING (#364).
//
// The interesting tests here are not "an operator can create an invite". They
// are the ways a caller who is NOT an operator gets one anyway — because this
// service is reachable without going through the gateway, so every check it
// performs is the only check there is.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"

	"github.com/eighred/kanz/internal/identity"
)

const operatorRole = "kanz-operator"

var provNow = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

// fakeProvisioner records what was stored.
type fakeProvisioner struct {
	created   []*identity.Invite
	listed    []*identity.Invite
	err       error
	forTenant string

	// #525: the account side.
	accounts     map[string]*identity.User
	loadErr      error
	statusErr    error
	statusWrites []statusWrite
}

func (f *fakeProvisioner) CreateInvite(_ context.Context, inv *identity.Invite) error {
	if f.err != nil {
		return f.err
	}
	f.created = append(f.created, inv)
	return nil
}

func (f *fakeProvisioner) InvitesFor(_ context.Context, tenant string) ([]*identity.Invite, error) {
	f.forTenant = tenant
	return f.listed, f.err
}

// --- DEPROVISIONING (#525) ---
//
// Kept on separate error fields from f.err: a test that fails the status write
// must not also fail the load, or "refused before writing" and "the write failed"
// become indistinguishable in the assertion.

func (f *fakeProvisioner) UserBySubject(_ context.Context, subject string) (*identity.User, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	u, ok := f.accounts[subject]
	if !ok {
		return nil, identity.ErrUserNotFound
	}
	return u, nil
}

func (f *fakeProvisioner) SetStatus(
	_ context.Context, subject string, status identity.Status, now time.Time,
) error {
	f.statusWrites = append(f.statusWrites, statusWrite{subject: subject, status: status, at: now})
	if f.statusErr != nil {
		return f.statusErr
	}
	if u, ok := f.accounts[subject]; ok {
		u.Status = status
		u.UpdatedAt = now
	}
	return nil
}

// statusWrite is one call to SetStatus, recorded so a test can assert that a
// REFUSED request made none — the difference between "answered 404" and
// "answered 404 after disabling somebody else's trader".
type statusWrite struct {
	subject string
	status  identity.Status
	at      time.Time
}

// account builds an active account in a tenant.
func account(subject, tenant string) *identity.User {
	return &identity.User{
		Subject: subject, Tenant: tenant, Roles: []string{"kanz-trader"},
		Credential: "hash", Status: identity.StatusActive, CreatedAt: provNow, UpdatedAt: provNow,
	}
}

// fakeVerifier stands in for the real ES256 verifier. The real one's own tests
// (internal/identity/verify_test.go) cover signature, algorithm pinning and
// expiry; what matters HERE is what the handler does with the answer.
type fakeVerifier struct {
	claims   *identity.Claims
	err      error
	sawToken string
}

func (f *fakeVerifier) Verify(raw string, _ time.Time) (*identity.Claims, error) {
	f.sawToken = raw
	if f.err != nil {
		return nil, f.err
	}
	return f.claims, nil
}

func operatorClaims() *identity.Claims {
	return &identity.Claims{
		Subject: "user:olivia", Tenant: "acme",
		Roles:  []string{"kanz-user", operatorRole},
		Expiry: provNow.Add(time.Hour),
	}
}

type provFixture struct {
	s     *Server
	store *fakeProvisioner
	vfy   *fakeVerifier
	audit *fakeAudit
}

// fakeAudit is the auth.DecisionRecorder the status routes write to.
type fakeAudit struct {
	entries []*observationpb.DecisionLog
	err     error
}

func (f *fakeAudit) Record(_ context.Context, e *observationpb.DecisionLog) error {
	f.entries = append(f.entries, e)
	return f.err
}

func newProvServer(t *testing.T, claims *identity.Claims, verifyErr error) provFixture {
	t.Helper()
	store := &fakeProvisioner{accounts: map[string]*identity.User{}}
	// THE OPERATOR HAS AN ACTIVE ACCOUNT, because since #525 operator() reads one:
	// a token alone no longer provisions. Seeded here rather than per-test so the
	// #364 tests keep asserting what they were written to assert.
	if claims != nil && claims.Subject != "" {
		store.accounts[claims.Subject] = account(claims.Subject, claims.Tenant)
	}
	vfy := &fakeVerifier{claims: claims, err: verifyErr}
	audit := &fakeAudit{}
	s, err := New(&fakeStore{}, &fakeMinter{}, &allowN{n: 100},
		func() any { return map[string]any{"keys": []any{}} }, "https://identity.test", quiet(),
		WithProvisioning(Provisioning{
			Verifier: vfy, Store: store, OperatorRole: operatorRole, Audit: audit,
		}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.now = func() time.Time { return provNow }
	return provFixture{s: s, store: store, vfy: vfy, audit: audit}
}

// req issues a request with an optional bearer token and optional raw headers.
func (f provFixture) req(t *testing.T, method, path, token string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = strings.NewReader(string(raw))
	} else {
		rdr = strings.NewReader("")
	}
	r := httptest.NewRequest(method, path, rdr)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	mux := http.NewServeMux()
	f.s.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	return rec
}

func validBody() map[string]any {
	return map[string]any{
		"subject": "user:newhire", "roles": []string{"kanz-user"}, "portfolios": []string{"pf-1"},
	}
}

// An operator creates an invitation, and the token is returned once.
func TestProvision_AnOperatorCreatesAnInvitation(t *testing.T) {
	f := newProvServer(t, operatorClaims(), nil)

	rec := f.req(t, http.MethodPost, "/invites", "tok", validBody(), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201 got %d (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		InviteToken string `json:"invite_token"`
		Tenant      string `json:"tenant"`
		CreatedBy   string `json:"created_by"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.InviteToken == "" {
		t.Error("no invite_token returned — it is stored only as a hash, so this response is the " +
			"only chance to capture it")
	}
	if len(f.store.created) != 1 {
		t.Fatalf("stored %d invites, want 1", len(f.store.created))
	}
	inv := f.store.created[0]

	// THE STORED FORM IS A HASH, NOT THE TOKEN.
	if strings.Contains(inv.TokenHash, out.InviteToken) || inv.TokenHash == out.InviteToken {
		t.Error("the invitation token was stored verbatim — a leaked database would impersonate " +
			"every invitee")
	}
	if inv.TokenHash != identity.InviteTokenHash(out.InviteToken) {
		t.Error("the stored hash is not the hash of the returned token, so the invitation cannot be redeemed")
	}
	// createdBy IS THE VERIFIED SUBJECT.
	if inv.CreatedBy != "user:olivia" || out.CreatedBy != "user:olivia" {
		t.Errorf("created_by = %q/%q, want the token's subject", inv.CreatedBy, out.CreatedBy)
	}
	if inv.Tenant != "acme" || out.Tenant != "acme" {
		t.Errorf("tenant = %q, want the operator's own", inv.Tenant)
	}
}

// ===== THE WAYS A NON-OPERATOR GETS ONE ANYWAY =====

// NO BEARER TOKEN IS 401. The base case.
func TestProvision_RefusesAnUnauthenticatedCaller(t *testing.T) {
	f := newProvServer(t, operatorClaims(), nil)
	rec := f.req(t, http.MethodPost, "/invites", "", validBody(), nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: want 401 got %d (%s)", rec.Code, rec.Body.String())
	}
	if len(f.store.created) != 0 {
		t.Fatal("an unauthenticated caller created an account")
	}
}

// A PRINCIPAL HEADER ALONE AUTHENTICATES NOBODY.
//
// This is the attack this service is uniquely exposed to. Every other upstream
// trusts X-Kanz-Principal-* because a NetworkPolicy makes the gateway its only
// caller; this service must be reachable by people holding no token, so those
// headers are strings the caller typed. test/arch forbids reading them at all —
// this asserts the behaviour that guard protects.
func TestProvision_APrincipalHeaderIsNotAnIdentity(t *testing.T) {
	f := newProvServer(t, operatorClaims(), nil)

	rec := f.req(t, http.MethodPost, "/invites", "", validBody(), map[string]string{
		"X-Kanz-Principal-Subject": "user:olivia",
		"X-Kanz-Principal-Tenant":  "acme",
		"X-Kanz-Principal-Roles":   operatorRole,
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("headers-only caller: want 401 got %d (%s) — anyone who can reach this pod could "+
			"provision an account with any role in any tenant", rec.Code, rec.Body.String())
	}
	if len(f.store.created) != 0 {
		t.Fatal("a caller who merely SET the gateway's headers created an account")
	}
}

// A REJECTED TOKEN IS 401, AND THE REASON IS NOT RETURNED.
//
// "expired" told apart from "signed by something else" is a probing oracle.
func TestProvision_ARejectedTokenIsRefusedWithoutSayingWhy(t *testing.T) {
	f := newProvServer(t, nil, errors.New("token expired at 2026-08-01T00:00:00Z"))

	rec := f.req(t, http.MethodPost, "/invites", "stale", validBody(), nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 got %d", rec.Code)
	}
	for _, leak := range []string{"expired", "2026-08-01"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("the refusal leaks %q to the caller: %s", leak, rec.Body.String())
		}
	}
}

// A VALID TOKEN WITHOUT THE OPERATOR ROLE IS 403.
//
// A trader holds a perfectly good token. It does not make accounts.
func TestProvision_AValidTokenWithoutTheOperatorRoleIsRefused(t *testing.T) {
	trader := &identity.Claims{Subject: "user:trish", Tenant: "acme",
		Roles: []string{"kanz-user", "kanz-trader"}, Expiry: provNow.Add(time.Hour)}
	f := newProvServer(t, trader, nil)

	rec := f.req(t, http.MethodPost, "/invites", "tok", validBody(), nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a trader creating an account: want 403 got %d (%s)", rec.Code, rec.Body.String())
	}
	if len(f.store.created) != 0 {
		t.Fatal("a caller without the operator role created an account")
	}
}

// THE ROLE IS MATCHED EXACTLY. A near-miss is not the operator role.
func TestProvision_TheOperatorRoleIsNotFuzzyMatched(t *testing.T) {
	for _, role := range []string{"KANZ-OPERATOR", "Kanz-Operator", "kanz-operator ", "operator", "kanz-operators"} {
		c := &identity.Claims{Subject: "user:mallory", Tenant: "acme",
			Roles: []string{role}, Expiry: provNow.Add(time.Hour)}
		f := newProvServer(t, c, nil)
		rec := f.req(t, http.MethodPost, "/invites", "tok", validBody(), nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("role %q: want 403 got %d", role, rec.Code)
		}
	}
}

// ===== THE ESCALATION WORTH THE MOST =====

// AN OPERATOR CANNOT PROVISION INTO ANOTHER TENANT.
//
// The tenant is what every RLS policy on this platform keys on, so an operator
// who could name an arbitrary one could mint themselves an account inside any
// fund on the estate. Refused rather than silently corrected: a client that
// believed it was provisioning elsewhere must learn that it was not.
func TestProvision_AnOperatorCannotProvisionIntoAnotherTenant(t *testing.T) {
	f := newProvServer(t, operatorClaims(), nil)

	body := validBody()
	body["tenant"] = "rival-fund"
	rec := f.req(t, http.MethodPost, "/invites", "tok", body, nil)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-tenant provisioning: want 403 got %d (%s)", rec.Code, rec.Body.String())
	}
	if len(f.store.created) != 0 {
		inv := f.store.created[0]
		t.Fatalf("an account was created in tenant %q by an operator of acme", inv.Tenant)
	}
	// And it says which two tenants disagreed, rather than a bare denial.
	if !strings.Contains(rec.Body.String(), "rival-fund") || !strings.Contains(rec.Body.String(), "acme") {
		t.Errorf("the refusal names neither tenant: %s", rec.Body.String())
	}
}

// AN OMITTED TENANT MEANS THE OPERATOR'S OWN — the common case, and it must not
// silently become empty, which NewInvite would refuse and which would otherwise
// read as a validation bug rather than a default.
func TestProvision_AnOmittedTenantMeansTheOperatorsOwn(t *testing.T) {
	f := newProvServer(t, operatorClaims(), nil)
	rec := f.req(t, http.MethodPost, "/invites", "tok", validBody(), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201 got %d (%s)", rec.Code, rec.Body.String())
	}
	if f.store.created[0].Tenant != "acme" {
		t.Errorf("tenant = %q, want acme", f.store.created[0].Tenant)
	}
}

// AN OPERATOR TOKEN CARRYING NO TENANT CANNOT SCOPE AN INVITATION.
func TestProvision_ATenantlessOperatorIsRefused(t *testing.T) {
	c := &identity.Claims{Subject: "user:olivia", Roles: []string{operatorRole}, Expiry: provNow.Add(time.Hour)}
	f := newProvServer(t, c, nil)

	rec := f.req(t, http.MethodPost, "/invites", "tok", validBody(), nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("tenantless operator: want 403 got %d (%s)", rec.Code, rec.Body.String())
	}
	if len(f.store.created) != 0 {
		t.Fatalf("an invitation was created with tenant %q", f.store.created[0].Tenant)
	}
}

// ===== LISTING =====

// THE LISTING IS SCOPED TO THE OPERATOR'S TENANT AND CARRIES NO SECRET.
func TestProvision_ListingIsTenantScopedAndLeaksNoToken(t *testing.T) {
	f := newProvServer(t, operatorClaims(), nil)
	raw, hash, err := identity.NewInviteToken()
	if err != nil {
		t.Fatal(err)
	}
	inv, err := identity.NewInvite("inv-1", hash, "user:newhire", "acme",
		[]string{"kanz-user"}, nil, "user:olivia", provNow, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f.store.listed = []*identity.Invite{inv}

	rec := f.req(t, http.MethodGet, "/invites", "tok", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: want 200 got %d (%s)", rec.Code, rec.Body.String())
	}
	if f.store.forTenant != "acme" {
		t.Errorf("listed tenant %q, want the operator's own acme", f.store.forTenant)
	}
	body := rec.Body.String()
	if strings.Contains(body, raw) {
		t.Error("the listing returned the invitation TOKEN")
	}
	if strings.Contains(body, hash) {
		t.Error("the listing returned the token HASH — a reader could confirm a guessed token offline")
	}
	if !strings.Contains(body, "user:newhire") || !strings.Contains(body, `"redeemable":true`) {
		t.Errorf("the listing does not show the outstanding invitation: %s", body)
	}
}

func TestProvision_ListingRequiresTheOperatorRole(t *testing.T) {
	trader := &identity.Claims{Subject: "user:trish", Tenant: "acme",
		Roles: []string{"kanz-trader"}, Expiry: provNow.Add(time.Hour)}
	f := newProvServer(t, trader, nil)
	if rec := f.req(t, http.MethodGet, "/invites", "tok", nil, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 got %d", rec.Code)
	}
}

// ===== THE ROUTES DO NOT EXIST WHEN PROVISIONING IS NOT WIRED =====
//
// A 404 and a 403 say different things to someone probing, and "this deployment
// has no provisioning surface" is the truthful one.
func TestProvision_RoutesAreAbsentWhenNotConfigured(t *testing.T) {
	s, _ := testServer(t, &fakeStore{}, nil)
	mux := http.NewServeMux()
	s.Routes(mux)

	for _, m := range []string{http.MethodPost, http.MethodGet} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(m, "/invites", strings.NewReader("{}")))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s /invites with provisioning unwired: want 404 got %d", m, rec.Code)
		}
	}
}

// HALF-WIRED PROVISIONING IS REFUSED AT CONSTRUCTION.
//
// Each of these would otherwise produce a route that looks armed. A nil verifier
// is the worst: it authenticates nobody while the routes exist.
func TestProvision_HalfWiredConfigurationIsRefusedAtConstruction(t *testing.T) {
	cases := []struct {
		name string
		p    Provisioning
	}{
		{"no verifier", Provisioning{Store: &fakeProvisioner{}, OperatorRole: operatorRole, Audit: &fakeAudit{}}},
		{"no store", Provisioning{Verifier: &fakeVerifier{}, OperatorRole: operatorRole, Audit: &fakeAudit{}}},
		{"no operator role", Provisioning{Verifier: &fakeVerifier{}, Store: &fakeProvisioner{}, Audit: &fakeAudit{}}},
		{"blank operator role", Provisioning{
			Verifier: &fakeVerifier{}, Store: &fakeProvisioner{}, OperatorRole: "  ", Audit: &fakeAudit{},
		}},
		// #525: an account disabled by nobody-in-particular is the unattributable
		// hand-run UPDATE these routes exist to replace. A deployment that wired
		// the power without the record must not start.
		{"no audit recorder", Provisioning{
			Verifier: &fakeVerifier{}, Store: &fakeProvisioner{}, OperatorRole: operatorRole,
		}},
	}
	for _, c := range cases {
		_, err := New(&fakeStore{}, &fakeMinter{}, &allowN{n: 1},
			func() any { return nil }, "https://identity.test", quiet(), WithProvisioning(c.p))
		if err == nil {
			t.Errorf("%s: New accepted a half-wired provisioning configuration", c.name)
		}
	}
}

// A STORE FAILURE DOES NOT RETURN A TOKEN. The caller must not walk away
// believing it holds a working invitation that was never recorded.
func TestProvision_AStoreFailureReturnsNoToken(t *testing.T) {
	f := newProvServer(t, operatorClaims(), nil)
	f.store.err = errors.New("database is down")

	rec := f.req(t, http.MethodPost, "/invites", "tok", validBody(), nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "invite_token") {
		t.Error("a token was returned for an invitation that was never stored")
	}
	if strings.Contains(rec.Body.String(), "database is down") {
		t.Error("the store's error text was returned to the caller")
	}
}

// A MALFORMED REQUEST IS A 400 THAT SAYS WHAT IS MISSING — it is the caller's
// input, and NewInvite already refuses no-subject and no-roles.
func TestProvision_AMalformedInvitationIsRefusedWithAReason(t *testing.T) {
	f := newProvServer(t, operatorClaims(), nil)
	for _, body := range []map[string]any{
		{"roles": []string{"kanz-user"}},                   // no subject
		{"subject": "user:x"},                              // no roles
		{"subject": "   ", "roles": []string{"kanz-user"}}, // blank subject
	} {
		rec := f.req(t, http.MethodPost, "/invites", "tok", body, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %v: want 400 got %d (%s)", body, rec.Code, rec.Body.String())
		}
	}
	if len(f.store.created) != 0 {
		t.Fatal("a malformed invitation was stored")
	}
}
