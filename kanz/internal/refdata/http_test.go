package refdata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/auth"
)

// datamasterStub stands in for the real surface. It asserts the identity
// contract on the way in and replays a scripted body on the way out.
type datamasterStub struct {
	tenant string
	bodies map[string]string
	status map[string]int

	gotPath    string
	gotSubject string
	gotTenant  string
}

func (d *datamasterStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.gotPath = r.URL.Path
		if p, ok := auth.PrincipalFromHeaders(r.Header); ok {
			d.gotSubject, d.gotTenant = p.Subject, p.Tenant
		}
		id := strings.TrimPrefix(r.URL.Path, "/v1/securities/")
		if code, ok := d.status[id]; ok {
			w.WriteHeader(code)
			return
		}
		body, ok := d.bodies[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"instrument not found"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
}

func newStub(t *testing.T) (*datamasterStub, *HTTPSource) {
	t.Helper()
	d := &datamasterStub{tenant: "acme", bodies: map[string]string{}, status: map[string]int{}}
	srv := httptest.NewServer(d.handler())
	t.Cleanup(srv.Close)
	src, err := NewHTTPSource(srv.URL, "acme", "svc:oms")
	if err != nil {
		t.Fatalf("NewHTTPSource: %v", err)
	}
	return d, src
}

const goldenAAPL = `{
  "instrument_id": "AAPL",
  "asset_class": "EQUITY",
  "currency_code": "USD",
  "description": "Apple Inc. Common Stock",
  "issuer_id": "LEI-APPLE",
  "sector": {"taxonomy": "GICS", "code": "45", "name": "Information Technology"},
  "as_of": "2026-08-01T00:00:00Z",
  "identifiers": {"isin": "US0378331005"},
  "provenance": {"sector": "BLOOMBERG"}
}`

func TestFetchReadsTheClassificationDatamasterServes(t *testing.T) {
	d, src := newStub(t)
	d.bodies["AAPL"] = goldenAAPL

	rec, found, err := src.Fetch(context.Background(), "AAPL")
	if err != nil || !found {
		t.Fatalf("Fetch = (%+v, %v, %v), want a found record", rec, found, err)
	}
	if rec.AssetClass != "EQUITY" {
		t.Errorf("AssetClass = %q, want EQUITY", rec.AssetClass)
	}
	if rec.IssuerID != "LEI-APPLE" {
		t.Errorf("IssuerID = %q, want LEI-APPLE — the field #640 added to the whole chain", rec.IssuerID)
	}
	if rec.Sector.Key() != "GICS:45" {
		t.Errorf("Sector.Key = %q, want GICS:45", rec.Sector.Key())
	}
	if want := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC); !rec.AsOf.Equal(want) {
		t.Errorf("AsOf = %v, want %v — without it no point-in-time question can be refused", rec.AsOf, want)
	}
}

// THE IDENTITY ON THIS HOP IS THE AUTHORIZATION. datamaster serves one tenant
// and answers a caller from another with a no-oracle 404, which this package
// reads as "no such instrument" — so a missing or wrong tenant header would
// present as an empty security master rather than as a refusal.
func TestFetchSendsTheServicePrincipalDatamasterChecks(t *testing.T) {
	d, src := newStub(t)
	d.bodies["AAPL"] = goldenAAPL

	if _, _, err := src.Fetch(context.Background(), "AAPL"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if d.gotTenant != "acme" {
		t.Errorf("tenant header = %q, want acme", d.gotTenant)
	}
	if d.gotSubject != "svc:oms" {
		t.Errorf("subject header = %q, want svc:oms — a refresh cycle acts on the service's own "+
			"behalf and must say so in datamaster's log", d.gotSubject)
	}
}

func TestA404IsAnAnswerAndEveryOtherFailureIsAnError(t *testing.T) {
	d, src := newStub(t)
	d.status["BOOM"] = http.StatusInternalServerError
	d.status["DENIED"] = http.StatusForbidden

	if _, found, err := src.Fetch(context.Background(), "UNKNOWN"); err != nil || found {
		t.Fatalf("a 404 gave (found=%v, err=%v); it is datamaster's definitive \"not held\" and "+
			"must be a clean not-found so the cache can evict on it", found, err)
	}
	for _, id := range []string{"BOOM", "DENIED"} {
		if _, _, err := src.Fetch(context.Background(), id); err == nil {
			t.Errorf("%s returned no error — a sick or refusing peer must NOT read as \"the master "+
				"does not hold this instrument\", which would withdraw a live classification", id)
		}
	}
}

// A BODY FOR ANOTHER INSTRUMENT IS THE WORST AVAILABLE FAILURE: installed, it
// classifies one instrument under another's sector for as long as the entry
// lives, and nothing anywhere disagrees.
func TestAMisroutedBodyIsRefusedRatherThanInstalled(t *testing.T) {
	d, src := newStub(t)
	d.bodies["AAPL"] = `{"instrument_id":"MSFT","asset_class":"EQUITY","sector":{"taxonomy":"GICS","code":"45"}}`

	_, _, err := src.Fetch(context.Background(), "AAPL")
	if err == nil {
		t.Fatal("accepted a response for a different instrument")
	}
	if !strings.Contains(err.Error(), "answered for") {
		t.Fatalf("error %q does not name the mismatch", err)
	}
}

func TestAnUnparseableAsOfIsRefusedRatherThanZeroed(t *testing.T) {
	d, src := newStub(t)
	d.bodies["AAPL"] = `{"instrument_id":"AAPL","as_of":"last tuesday"}`

	if _, _, err := src.Fetch(context.Background(), "AAPL"); err == nil {
		t.Fatal("an unparseable as_of was accepted — zeroing it would silently turn a wire-format " +
			"break into the legitimate \"undated record\" state, which has its own behaviour")
	}
}

// An instrument id is vendor-supplied: a RIC carries a dot, and a symbol can
// carry a slash. Concatenated into a URL, one of those addresses a different
// route on datamaster.
func TestAnInstrumentIDIsEscapedIntoThePath(t *testing.T) {
	d, src := newStub(t)
	id := "BRK/B"
	d.bodies[id] = `{"instrument_id":"BRK/B","asset_class":"EQUITY"}`

	rec, found, err := src.Fetch(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("Fetch(%q) = (%+v, %v, %v)", id, rec, found, err)
	}
	if d.gotPath != "/v1/securities/BRK/B" {
		t.Fatalf("server saw path %q; the id must arrive escaped and be decoded back to %q",
			d.gotPath, id)
	}
}

func TestConstructionRefusesAnIncompleteIdentity(t *testing.T) {
	for _, tc := range []struct{ name, base, tenant, subject, want string }{
		{"no base URL", "", "acme", "svc:oms", "base URL"},
		{"a bare host", "datamaster:8080", "acme", "svc:oms", "not a usable"},
		{"no tenant", "http://datamaster:8080", "", "svc:oms", "needs the tenant"},
		{"no subject", "http://datamaster:8080", "acme", "", "needs a subject"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewHTTPSource(tc.base, tc.tenant, tc.subject)
			if err == nil {
				t.Fatal("accepted an incomplete configuration")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name the problem (%q)", err, tc.want)
			}
		})
	}
}

func TestATrailingSlashOnTheBaseURLDoesNotDoubleUp(t *testing.T) {
	d := &datamasterStub{tenant: "acme", bodies: map[string]string{"AAPL": goldenAAPL}, status: map[string]int{}}
	srv := httptest.NewServer(d.handler())
	t.Cleanup(srv.Close)

	src, err := NewHTTPSource(srv.URL+"/", "acme", "svc:oms")
	if err != nil {
		t.Fatalf("NewHTTPSource: %v", err)
	}
	if _, found, err := src.Fetch(context.Background(), "AAPL"); err != nil || !found {
		t.Fatalf("Fetch = (%v, %v) — a trailing slash in the configured URL must not become //", found, err)
	}
	if d.gotPath != "/v1/securities/AAPL" {
		t.Fatalf("path = %q, want /v1/securities/AAPL", d.gotPath)
	}
}

// The refresh cycle's context bounds the cycle; this bounds the ONE call, so a
// single unresponsive lookup cannot spend the whole cycle's budget.
func TestAHungPeerIsAbandonedRatherThanWaitedOn(t *testing.T) {
	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-blocked:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(blocked); srv.Close() })

	src, err := NewHTTPSource(srv.URL, "acme", "svc:oms")
	if err != nil {
		t.Fatalf("NewHTTPSource: %v", err)
	}
	src.timeout = 50 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, _, err := src.Fetch(context.Background(), "AAPL")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a hung peer returned success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Fetch did not honour its own deadline — the caller's context has no deadline of " +
			"its own here, so this is the only bound")
	}
}
