package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/datamaster/internal/pricing"
)

// THE ACTOR ON AN OVERRIDE MUST BE THE AUTHENTICATED CALLER (#410).
//
// A pricing override is "a named human's signed decision to accept a price the
// system flagged" — this service's own words, and the reason #261 forced the
// exception store onto Postgres so the decision survives a restart. It is
// written to an append-only audit trail.
//
// THE NAME CAME OUT OF THE REQUEST BODY. handleOverride decoded `actor` from
// JSON and handed it straight to the store, while the gateway had already
// authenticated the caller and injected X-Kanz-Principal-Subject — which this
// service read for the TENANT (#222) and ignored for the IDENTITY. So any caller
// entitled to override could attribute the decision to ANY NAME THEY TYPED,
// including a colleague's, and the audit trail would record it as that person's
// signature.
//
// An audit record whose actor is self-asserted is not an audit record. It is
// also why dual control cannot be built on this surface as it stands: one person
// could propose as "alice" and approve as "bob" without ever holding a second
// credential.
//
// THE PATTERN IS ALREADY ON THE PLATFORM, one service over —
// services/optimization/internal/server refuses a body-supplied issuer that
// disagrees with the principal, calling it "the forged-issuer defect AUTH-01c
// prevents". This applies the identical rule to the more sensitive surface.

// postAsPrincipal issues a request carrying BOTH mesh headers the gateway
// injects. postAsTenant deliberately still exists and sets only the tenant: it
// is what proves the no-principal path refuses.
func postAsPrincipal(path, tenant, subject, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if tenant != "" {
		req.Header.Set(auth.HeaderPrincipalTenant, tenant)
	}
	if subject != "" {
		req.Header.Set(auth.HeaderPrincipalSubject, subject)
	}
	return req
}

// openExceptionID arbitrates a price so there is a real exception to override,
// and returns its id.
func openExceptionID(t *testing.T, s *Server, exceptions interface {
	Open(context.Context) ([]pricing.Exception, error)
}) string {
	t.Helper()
	if rec := get(t, s, "/v1/prices/INST1"); rec.Code != http.StatusOK {
		t.Fatalf("price: want 200 got %d", rec.Code)
	}
	open, err := exceptions.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range open {
		if e.Kind == pricing.KindPriceTolerance && e.InstrumentID == "INST1" {
			return e.ID
		}
	}
	t.Fatalf("want an open PRICE_TOLERANCE exception for INST1, got %v", open)
	return ""
}

func TestOverrideRecordsTheAuthenticatedSubject(t *testing.T) {
	s, exceptions := newServer(t)
	id := openExceptionID(t, s, exceptions)

	// No `actor` in the body at all — the caller does not get to say who they are.
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, postAsPrincipal("/v1/exceptions/"+id+"/override", testTenant, "alice@kanz",
		`{"reason":"corp action confirmed","chosen_price":"130"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("override: want 200 got %d (%s)", rec.Code, rec.Body.String())
	}

	ex, ok, err := exceptions.Get(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("read-back: ok=%v err=%v", ok, err)
	}
	if len(ex.Overrides) != 1 {
		t.Fatalf("want 1 override, got %d: %+v", len(ex.Overrides), ex)
	}
	if got := ex.Overrides[0].Actor; got != "alice@kanz" {
		t.Fatalf("recorded actor = %q, want %q — the audit trail must name the caller the gateway "+
			"authenticated, not a string the caller chose", got, "alice@kanz")
	}
}

// AN ACTOR NAMING SOMEONE ELSE IS REFUSED, LOUDLY.
//
// Silently overwriting it with the principal would be worse than it looks: a
// client would believe it had recorded Mallory's decision and the trail would
// say Alice, with nothing anywhere reporting the disagreement. Refusing says
// which two names disagreed.
func TestOverrideRefusesAnActorThatIsNotTheCaller(t *testing.T) {
	s, exceptions := newServer(t)
	id := openExceptionID(t, s, exceptions)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, postAsPrincipal("/v1/exceptions/"+id+"/override", testTenant, "alice@kanz",
		`{"actor":"bob@kanz","reason":"looks fine","chosen_price":"130"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("override attributed to another subject: want 400 got %d (%s).\n"+
			"Accepting it lets one credential sign another person's name into an append-only "+
			"compliance record.", rec.Code, rec.Body.String())
	}

	ex, ok, err := exceptions.Get(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("read-back: ok=%v err=%v", ok, err)
	}
	if len(ex.Overrides) != 0 {
		t.Fatalf("a refused override still wrote %d record(s) to the audit trail: %+v",
			len(ex.Overrides), ex.Overrides)
	}
	if ex.Status == pricing.StatusOverridden {
		t.Fatal("a refused override still flipped the exception to OVERRIDDEN")
	}
}

// THE CALLER'S OWN NAME IS STILL ACCEPTED, so a client that echoes the subject
// it authenticated as keeps working. Without this the rule above is satisfied by
// refusing every body that mentions an actor, which would be a breaking change
// dressed as a security fix.
func TestOverrideAcceptsAnActorEqualToTheCaller(t *testing.T) {
	s, exceptions := newServer(t)
	id := openExceptionID(t, s, exceptions)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, postAsPrincipal("/v1/exceptions/"+id+"/override", testTenant, "alice@kanz",
		`{"actor":"alice@kanz","reason":"corp action confirmed","chosen_price":"130"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("override echoing the caller's own subject: want 200 got %d (%s)", rec.Code, rec.Body.String())
	}
	ex, _, _ := exceptions.Get(context.Background(), id)
	if len(ex.Overrides) != 1 || ex.Overrides[0].Actor != "alice@kanz" {
		t.Fatalf("override not recorded against the caller: %+v", ex.Overrides)
	}
}

// NO PRINCIPAL, NO OVERRIDE. An unauthenticated write to a compliance trail has
// no name to put on it, and inventing one ("unknown", "system") would be the
// same defect with a different string.
//
// This is reachable only if the request bypassed the gateway or the gateway is
// misconfigured — both refusals, exactly as the optimization service argues for
// its own command surface.
func TestOverrideRefusesWithNoAuthenticatedPrincipal(t *testing.T) {
	s, exceptions := newServer(t)
	id := openExceptionID(t, s, exceptions)

	rec := httptest.NewRecorder()
	// Tenant header only — the shape callerOwnsThisInstance already accepts.
	s.ServeHTTP(rec, postAsTenant("/v1/exceptions/"+id+"/override", testTenant,
		`{"actor":"alice@kanz","reason":"x","chosen_price":"130"}`))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("override with no principal: want 401 got %d (%s).\n"+
			"The tenant header alone establishes WHICH tenant is asking, never WHO.",
			rec.Code, rec.Body.String())
	}
	ex, _, _ := exceptions.Get(context.Background(), id)
	if len(ex.Overrides) != 0 {
		t.Fatalf("an unauthenticated override wrote %d record(s): %+v", len(ex.Overrides), ex.Overrides)
	}
}
