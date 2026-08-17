package server

// DEPROVISIONING (#525).
//
// Every test here is ungated — no TEST_POSTGRES_URL — because none of the
// properties is a property of the SQL. They are all properties of the handler:
// who may call it, what an unconfigured deployment answers, which refusals are
// indistinguishable from each other, and whether a refused request wrote anything
// anyway. The one property that IS the SQL — zero rows affected must not read as
// success — lives in internal/identity/postgres_test.go and skips without a
// database.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/identity"
)

// statusFixture wires the login store and the provisioning store over the SAME
// account map, so a disable made through the route is the account /login then
// reads. Two unconnected doubles would let the route "succeed" against a map
// nothing authenticates from.
type statusFixture struct {
	provFixture
	accounts map[string]*identity.User
	minter   *fakeMinter
}

func newStatusServer(t *testing.T, claims *identity.Claims) statusFixture {
	t.Helper()
	accounts := map[string]*identity.User{}
	if claims != nil && claims.Subject != "" {
		accounts[claims.Subject] = account(claims.Subject, claims.Tenant)
	}
	prov := &fakeProvisioner{accounts: accounts}
	vfy := &fakeVerifier{claims: claims}
	audit := &fakeAudit{}
	minter := &fakeMinter{}
	s, err := New(&fakeStore{users: accounts}, minter, &allowN{n: 100},
		func() any { return map[string]any{"keys": []any{}} }, "https://identity.test", quiet(),
		WithProvisioning(Provisioning{
			Verifier: vfy, Store: prov, OperatorRole: operatorRole, Audit: audit,
		}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.now = func() time.Time { return provNow }
	return statusFixture{
		provFixture: provFixture{s: s, store: prov, vfy: vfy, audit: audit},
		accounts:    accounts,
		minter:      minter,
	}
}

// ===== THE ROUTE DOES NOT EXIST UNTIL PROVISIONING IS WIRED =====

// AN UNCONFIGURED DEPLOYMENT ANSWERS 404, NOT 403.
//
// This is the same decision #364 made for POST /invites and it is made again
// rather than inherited: 403 tells a prober that a deprovisioning surface exists
// to be attacked here, and "this deployment has no such surface" is the truthful
// answer. The failure mode is silent — a route registered-and-refusing behaves
// identically in every test that only checks for a non-2xx.
func TestStatus_TheRoutesDoNotExistWithoutProvisioning(t *testing.T) {
	s, _ := testServer(t, &fakeStore{users: map[string]*identity.User{}}, nil)
	mux := http.NewServeMux()
	s.Routes(mux)

	for _, path := range []string{"/users/user:bob/disable", "/users/user:bob/enable"} {
		rec := unprovisioned(mux, http.MethodPost, path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s on an unprovisioned deployment: want 404 got %d (%s)\n"+
				"A 403 would confirm the surface exists; not registering the route says the truth.",
				path, rec.Code, rec.Body.String())
		}
	}
}

// ===== THE HAPPY PATH, AND WHAT IT WRITES =====

func TestStatus_AnOperatorDisablesAnAccountAndLoginThenRefusesIt(t *testing.T) {
	f := newStatusServer(t, operatorClaims())
	f.accounts["user:bob"] = userWith(t, "user:bob", "correct password", identity.StatusActive)

	// Bob can log in.
	if rec := post(t, f.s, "/login", loginRequest{Subject: "user:bob", Credential: "correct password"}); rec.Code != http.StatusOK {
		t.Fatalf("precondition: bob cannot log in (%d %s)", rec.Code, rec.Body.String())
	}

	rec := f.req(t, http.MethodPost, "/users/user:bob/disable", "tok", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: want 200 got %d (%s)", rec.Code, rec.Body.String())
	}

	if n := len(f.store.statusWrites); n != 1 {
		t.Fatalf("SetStatus called %d times, want 1", n)
	}
	w := f.store.statusWrites[0]
	if w.subject != "user:bob" || w.status != identity.StatusDisabled {
		t.Errorf("wrote %s=%s, want user:bob=disabled", w.subject, w.status)
	}

	// AND THE ENFORCEMENT ACTUALLY BITES. Both points the issue names: /login
	// refuses, and no token is minted for the account.
	minted := f.minter.minted
	after := post(t, f.s, "/login", loginRequest{Subject: "user:bob", Credential: "correct password"})
	if after.Code != http.StatusUnauthorized {
		t.Fatalf("login after disable: want 401 got %d (%s) — the write happened and the control "+
			"did not", after.Code, after.Body.String())
	}
	if f.minter.minted != minted {
		t.Error("a token was minted for an account that had just been disabled")
	}
}

// THE INVERSE EXISTS, and it is not a convenience: without it a mistaken disable
// needs the hand-run UPDATE this route was built to eliminate.
func TestStatus_ReEnableRestoresTheAccount(t *testing.T) {
	f := newStatusServer(t, operatorClaims())
	f.accounts["user:bob"] = userWith(t, "user:bob", "correct password", identity.StatusDisabled)

	rec := f.req(t, http.MethodPost, "/users/user:bob/enable", "tok", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable: want 200 got %d (%s)", rec.Code, rec.Body.String())
	}
	if n := len(f.store.statusWrites); n != 1 || f.store.statusWrites[0].status != identity.StatusActive {
		t.Fatalf("writes = %+v, want one active write", f.store.statusWrites)
	}
	if after := post(t, f.s, "/login",
		loginRequest{Subject: "user:bob", Credential: "correct password"}); after.Code != http.StatusOK {
		t.Fatalf("login after re-enable: want 200 got %d (%s)", after.Code, after.Body.String())
	}
}

// ===== THE AUDIT RECORD — the half that makes it a control =====

// WHO DISABLED WHOM, AND WHEN, from the VERIFIED TOKEN and the LOADED ACCOUNT.
func TestStatus_ADisableEmitsAnAuditRecordNamingTheOperator(t *testing.T) {
	f := newStatusServer(t, operatorClaims())
	f.accounts["user:bob"] = account("user:bob", "acme")

	if rec := f.req(t, http.MethodPost, "/users/user:bob/disable", "tok", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("disable: want 200 got %d (%s)", rec.Code, rec.Body.String())
	}

	if n := len(f.audit.entries); n != 1 {
		t.Fatalf("audit entries = %d, want 1 — a disable with no record of who ordered it is the "+
			"unattributable hand-run UPDATE this route replaces", n)
	}
	e := f.audit.entries[0]
	if e.GetDecider() != "operator:user:olivia" {
		t.Errorf("decider = %q, want operator:user:olivia — the deciding party is the named "+
			"operator, not the service that carried it", e.GetDecider())
	}
	attrs := e.GetAttributes()
	for k, want := range map[string]string{
		"action":          "identity.account.disable",
		"account.subject": "user:bob",
		"account.tenant":  "acme",
		"account.status":  "disabled",
		// pkg/authbus reads exactly these two to stamp an envelope's tenant and
		// partition key. Renaming them silently unlands the day a bus recorder
		// replaces the slog one.
		"principal.subject": "user:olivia",
		"principal.tenant":  "acme",
	} {
		if attrs[k] != want {
			t.Errorf("attribute %q = %q, want %q", k, attrs[k], want)
		}
	}
	if attrs["occurred_at"] == "" {
		t.Error("no occurred_at — 'when' is half of what this record exists to say")
	}
	if e.GetSummary() == "" {
		t.Error("no summary — the audit projection renders this for a human reader")
	}
}

// A FAILED AUDIT WRITE MUST NOT BE REPORTED AS A FAILED DISABLE.
//
// The status change is already durable at that point. Answering 500 would tell an
// operator the lockout did not happen when it did, and send them straight to the
// hand-run UPDATE. The record is lost loudly (an ERROR line) rather than the
// change being misreported.
func TestStatus_AnAuditFailureDoesNotHideACompletedDisable(t *testing.T) {
	f := newStatusServer(t, operatorClaims())
	f.accounts["user:bob"] = account("user:bob", "acme")
	f.audit.err = errors.New("audit sink is down")

	rec := f.req(t, http.MethodPost, "/users/user:bob/disable", "tok", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 got %d (%s) — the disable committed; reporting failure sends the "+
			"operator back to the database", rec.Code, rec.Body.String())
	}
	if len(f.store.statusWrites) != 1 {
		t.Fatalf("writes = %d, want 1", len(f.store.statusWrites))
	}
}

// ===== THE WAYS SOMEBODY DISABLES AN ACCOUNT THEY MAY NOT =====

func TestStatus_RefusesAnUnauthenticatedCaller(t *testing.T) {
	f := newStatusServer(t, operatorClaims())
	f.accounts["user:bob"] = account("user:bob", "acme")

	rec := f.req(t, http.MethodPost, "/users/user:bob/disable", "", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 got %d (%s)", rec.Code, rec.Body.String())
	}
	if len(f.store.statusWrites) != 0 {
		t.Fatal("an unauthenticated caller disabled an account")
	}
}

// A PRINCIPAL HEADER IS NOT AN IDENTITY HERE, for the reason provision.go states:
// this service must be reachable without a token, so it cannot sit behind the
// NetworkPolicy that makes those headers trustworthy anywhere else.
func TestStatus_APrincipalHeaderIsNotAnIdentity(t *testing.T) {
	f := newStatusServer(t, operatorClaims())
	f.accounts["user:bob"] = account("user:bob", "acme")

	rec := f.req(t, http.MethodPost, "/users/user:bob/disable", "", nil, map[string]string{
		"X-Kanz-Principal-Subject": "user:olivia",
		"X-Kanz-Principal-Tenant":  "acme",
		"X-Kanz-Principal-Roles":   operatorRole,
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("headers-only caller: want 401 got %d (%s) — anyone reaching this pod could "+
			"lock out any trader", rec.Code, rec.Body.String())
	}
	if len(f.store.statusWrites) != 0 {
		t.Fatal("a header-only caller disabled an account")
	}
}

func TestStatus_RefusesACallerWithoutTheOperatorRole(t *testing.T) {
	claims := &identity.Claims{
		Subject: "user:trent", Tenant: "acme",
		Roles: []string{"kanz-trader"}, Expiry: provNow.Add(time.Hour),
	}
	f := newStatusServer(t, claims)
	f.accounts["user:bob"] = account("user:bob", "acme")

	rec := f.req(t, http.MethodPost, "/users/user:bob/disable", "tok", nil, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 got %d (%s)", rec.Code, rec.Body.String())
	}
	if len(f.store.statusWrites) != 0 {
		t.Fatal("a non-operator disabled an account")
	}
}

// A TARGET IN ANOTHER TENANT IS THE SAME 404 AS A TARGET THAT DOES NOT EXIST.
//
// identity's pool is UNSCOPED by necessity, so no RLS policy stands behind this
// route — the tenant check IS the boundary. And the two refusals must be
// byte-identical: a 403 for "exists, elsewhere" versus a 404 for "does not exist"
// hands any operator on the platform a probe for other funds' staff lists.
func TestStatus_ACrossTenantTargetAndAnUnknownSubjectAnswerIdentically(t *testing.T) {
	f := newStatusServer(t, operatorClaims())
	f.accounts["user:carol"] = account("user:carol", "globex")

	other := f.req(t, http.MethodPost, "/users/user:carol/disable", "tok", nil, nil)
	unknown := f.req(t, http.MethodPost, "/users/user:nobody/disable", "tok", nil, nil)

	if other.Code != http.StatusNotFound || unknown.Code != http.StatusNotFound {
		t.Fatalf("codes = %d / %d, want 404 / 404 (bodies %s / %s)",
			other.Code, unknown.Code, other.Body.String(), unknown.Body.String())
	}
	if other.Body.String() != unknown.Body.String() {
		t.Fatalf("bodies differ:\n  other tenant: %s  unknown     : %s\n"+
			"Any difference enumerates another tenant's accounts.",
			other.Body.String(), unknown.Body.String())
	}
	if len(f.store.statusWrites) != 0 {
		t.Fatalf("an operator of acme disabled %d account(s) it does not own", len(f.store.statusWrites))
	}
}

// A STORE FAILURE IS NOT A COMPLETED DISABLE. The operator must not be told the
// account is locked out when the write did not land.
func TestStatus_AStoreFailureIsNotReportedAsSuccess(t *testing.T) {
	f := newStatusServer(t, operatorClaims())
	f.accounts["user:bob"] = account("user:bob", "acme")
	f.store.statusErr = errors.New("database is down")

	rec := f.req(t, http.MethodPost, "/users/user:bob/disable", "tok", nil, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 got %d (%s)", rec.Code, rec.Body.String())
	}
	if len(f.audit.entries) != 0 {
		t.Error("an audit record was written for a disable that did not happen — the record is " +
			"then evidence of a lockout that never occurred")
	}
}

// A SUBJECT THAT MATCHED NO ROW IS ErrUserNotFound, NOT SUCCESS. This is the
// handler half of the store guarantee: SetStatus reports zero rows affected as an
// error, and the handler must not answer 200 to it.
func TestStatus_ARowThatVanishedMidRequestIsNotReportedAsDisabled(t *testing.T) {
	f := newStatusServer(t, operatorClaims())
	f.accounts["user:bob"] = account("user:bob", "acme")
	f.store.statusErr = identity.ErrUserNotFound

	rec := f.req(t, http.MethodPost, "/users/user:bob/disable", "tok", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 got %d (%s)", rec.Code, rec.Body.String())
	}
	if len(f.audit.entries) != 0 {
		t.Error("an audit record was written for an account that was not there")
	}
}

// ===== THE OPERATOR'S OWN ACCOUNT (#525, the residual-token half) =====

// A DISABLED OPERATOR'S OUTSTANDING TOKEN MUST NOT PROVISION.
//
// This is the case that passed before the check existed: the token's signature,
// expiry, issuer, audience and role are all still exactly as minted, because they
// were frozen when it was issued. Nothing above the status check reads the
// account, so a person the estate had already locked out kept the highest
// privilege on the platform for the rest of the TTL — from the one process that
// holds the identity database pool.
func TestOperator_ADisabledOperatorsValidTokenIsRefused(t *testing.T) {
	f := newStatusServer(t, operatorClaims())
	f.accounts["user:bob"] = account("user:bob", "acme")
	f.accounts["user:olivia"].Status = identity.StatusDisabled

	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/users/user:bob/disable"},
		{http.MethodPost, "/invites"},
		{http.MethodGet, "/invites"},
	} {
		var body any
		if c.path == "/invites" && c.method == http.MethodPost {
			body = validBody()
		}
		rec := f.req(t, c.method, c.path, "tok", body, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with a disabled operator's token: want 401 got %d (%s)",
				c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	if len(f.store.statusWrites) != 0 || len(f.store.created) != 0 {
		t.Fatal("a disabled operator provisioned")
	}
}

// AND THE REFUSAL IS INDISTINGUISHABLE FROM AN EXPIRED TOKEN. Telling a disabled
// operator "disabled" confirms their subject is still a known account.
func TestOperator_ADisabledOperatorAndARejectedTokenAnswerIdentically(t *testing.T) {
	disabled := newStatusServer(t, operatorClaims())
	disabled.accounts["user:olivia"].Status = identity.StatusDisabled
	rejected := newProvServer(t, operatorClaims(), errors.New("token expired"))

	a := disabled.req(t, http.MethodGet, "/invites", "tok", nil, nil)
	b := rejected.req(t, http.MethodGet, "/invites", "tok", nil, nil)

	if a.Code != b.Code || a.Body.String() != b.Body.String() {
		t.Fatalf("disabled operator answered %d %s; rejected token answered %d %s — the "+
			"difference confirms the account exists", a.Code, a.Body.String(), b.Code, b.Body.String())
	}
}

// A STATUS THAT COULD NOT BE CHECKED IS NOT A STATUS THAT WAS CHECKED.
//
// It fails CLOSED — the request is refused — but with its own outcome, because
// "checked, and fine" and "could not check" must never look the same. 503 is
// retryable and says the service could not answer; a 401 here would report a
// store outage as a wave of disabled operators.
func TestOperator_AStatusThatCannotBeCheckedFailsClosedAndSaysSo(t *testing.T) {
	f := newStatusServer(t, operatorClaims())
	f.accounts["user:bob"] = account("user:bob", "acme")
	f.store.loadErr = errors.New("connection refused")

	rec := f.req(t, http.MethodPost, "/users/user:bob/disable", "tok", nil, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 got %d (%s) — an unreachable store must neither allow the request "+
			"nor be reported as a disabled account", rec.Code, rec.Body.String())
	}
	if len(f.store.statusWrites) != 0 {
		t.Fatal("the request proceeded despite an unreadable account status")
	}
}

// A VALIDLY-SIGNED TOKEN NAMING NO ACCOUNT IS REFUSED, and identically. The
// account was deleted, or the token was minted by something that should not have.
func TestOperator_ATokenNamingNoAccountIsRefused(t *testing.T) {
	claims := &identity.Claims{
		Subject: "user:ghost", Tenant: "acme",
		Roles: []string{operatorRole}, Expiry: provNow.Add(time.Hour),
	}
	f := newStatusServer(t, claims)
	delete(f.accounts, "user:ghost")
	f.accounts["user:bob"] = account("user:bob", "acme")

	rec := f.req(t, http.MethodPost, "/users/user:bob/disable", "tok", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 got %d (%s)", rec.Code, rec.Body.String())
	}
	if len(f.store.statusWrites) != 0 {
		t.Fatal("a token naming no account disabled somebody")
	}
}

// ===== THE STATUS IS NEVER THE CALLER'S TO NAME =====

// Both handlers pass a CONSTANT to SetStatus. This asserts the store never sees
// anything else, which is what makes identity.Status.Validate a guard for a
// future caller rather than for this one.
func TestStatus_OnlyTheTwoEnumeratedStatusesReachTheStore(t *testing.T) {
	f := newStatusServer(t, operatorClaims())
	f.accounts["user:bob"] = account("user:bob", "acme")

	f.req(t, http.MethodPost, "/users/user:bob/disable", "tok", nil, nil)
	f.req(t, http.MethodPost, "/users/user:bob/enable", "tok", nil, nil)

	if len(f.store.statusWrites) != 2 {
		t.Fatalf("writes = %d, want 2", len(f.store.statusWrites))
	}
	for _, w := range f.store.statusWrites {
		if err := w.status.Validate(); err != nil {
			t.Errorf("the handler sent %q to the store: %v", w.status, err)
		}
	}
}

// unprovisioned drives a mux built WITHOUT provisioning, where there is no
// provFixture to hang a request on.
func unprovisioned(mux *http.ServeMux, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader("")))
	return rec
}
