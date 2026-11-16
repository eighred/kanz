package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/lineage/internal/governance"
	"github.com/eighred/kanz/services/lineage/internal/graph"
	"github.com/eighred/kanz/services/lineage/internal/query"
	"github.com/eighred/kanz/services/lineage/internal/server"
)

// THE HTTP SURFACE HAD NO TESTS AT ALL, WHICH IS HOW #268 SURVIVED.
//
// Every governance assertion in this service was written against
// query.Service/Governor with a principal handed in as an argument — a shape the
// deployed server never produced, because nothing turned the api-gateway's
// identity headers back into one. These tests drive the SERVER, through
// SetPrincipalHeaders, so the transport is inside the assertion rather than
// assumed by it.

var (
	piiDS   = graph.DatasetID{Namespace: "kanz.customer", Name: "PersonProfile"}
	derived = graph.DatasetID{Namespace: "kanz.risk", Name: "CustomerExposure"}
)

// newServer builds the real wiring: PersonProfile (PII) → CustomerExposure
// (public), with only the "steward" role granted lineage.pii.read.
func newServer(t *testing.T) *server.Server {
	t.Helper()
	g := newMemory(t)
	now := time.Now()
	observe(t, g, "e1", piiDS, "customer", "customer.v1.PersonProfile:1", now, "")
	observe(t, g, "e2", derived, "risk", "risk.v1.CustomerExposure:1", now, "e1")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	authz := auth.NewAuditedAuthorizer(
		auth.NewPolicyAuthorizer(&auth.Policy{Roles: map[string][]auth.Action{
			"steward": {governance.ActionPIIRead},
		}}),
		auth.NewSlogRecorder(logger), "lineage", logger)
	gov := governance.NewGovernor(
		governance.NewClassifier(&governance.Config{Datasets: []string{"PersonProfile"}}), authz)

	readiness := &server.Readiness{}
	readiness.Set(true)
	return server.New(readiness, logger, server.WithLineage(query.NewService(g, gov)))
}

func get(t *testing.T, s *server.Server, path string, set func(http.Header)) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if set != nil {
		set(r.Header)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// The defect, pinned. A request that did not come through the gateway carries no
// principal, and this service answered it: governance.CheckAccess short-circuits
// every non-PII dataset to "public dataset — allow" before it looks at the
// caller, and the catalog listing never asked for one. An unauthenticated
// stranger and an authorized steward got the same 200 on the whole non-PII graph.
func TestGovernedReadsRefuseARequestWithNoPrincipal(t *testing.T) {
	s := newServer(t)
	for _, path := range []string{
		"/v1/catalog/datasets",
		"/v1/lineage/event/e2",
		"/v1/lineage/dataset/kanz.risk/CustomerExposure",
	} {
		w := get(t, s, path, nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s with no identity headers: want 401, got %d (%s)", path, w.Code, w.Body.String())
		}
	}
}

// The issue's "verified when", end to end: the three headers the gateway injects
// arrive, the principal is reconstructed, and the governance check runs against
// the CALLER's roles — so the steward gets the PII node intact.
//
// TWO roles, with the granting one second, on purpose. A single-role header
// survives a reconstruction that never splits on the comma at all, so a one-role
// case would pass against a reader that hands the whole header back as one role
// name — the exact half-correct implementation this is here to reject.
func TestStewardReachesPIILineageThroughTheInjectedHeaders(t *testing.T) {
	s := newServer(t)
	w := get(t, s, "/v1/lineage/event/e2", func(h http.Header) {
		auth.SetPrincipalHeaders(h, "s1", "acme", []string{"viewer", "steward"})
	})
	if w.Code != http.StatusOK {
		t.Fatalf("steward: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	var prov query.Provenance
	if err := json.Unmarshal(w.Body.Bytes(), &prov); err != nil {
		t.Fatalf("decode provenance: %v", err)
	}
	if len(prov.Upstream) != 1 {
		t.Fatalf("upstream = %+v, want the one PersonProfile node", prov.Upstream)
	}
	if u := prov.Upstream[0]; u.Redacted || u.SchemaRef == "" {
		t.Errorf("the steward's own grant did not reach the governor — PII node came back "+
			"redacted: %+v. The roles header is on the wire but was not read back", u)
	}
}

// The mirror image, and the reason a non-nil principal is not enough on its own:
// the SAME transport carrying a role that lacks lineage.pii.read must still be
// denied. A reconstruction that dropped or mangled the roles header would pass
// the test above by accident and fail here.
func TestViewerIsStillDeniedThePIITarget(t *testing.T) {
	s := newServer(t)
	w := get(t, s, "/v1/lineage/dataset/kanz.customer/PersonProfile", func(h http.Header) {
		auth.SetPrincipalHeaders(h, "v1", "acme", []string{"viewer"})
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer on a PII target: want 403, got %d (%s)", w.Code, w.Body.String())
	}
}

// A principal reconstructed from the headers reads its own tenant's public
// lineage. Without this the fail-closed middleware would be indistinguishable
// from a service that refuses everything.
func TestAuthenticatedCallerReadsPublicLineage(t *testing.T) {
	s := newServer(t)
	w := get(t, s, "/v1/catalog/datasets", func(h http.Header) {
		auth.SetPrincipalHeaders(h, "v1", "acme", []string{"viewer"})
	})
	if w.Code != http.StatusOK {
		t.Fatalf("authenticated catalog read: want 200, got %d (%s)", w.Code, w.Body.String())
	}
}

// #244 AT THE TRANSPORT, WHICH IS WHERE IT HAS TO HOLD.
//
// The distinction is worthless if it lives only in the body: a dashboard, an
// alert rule or `curl -f` reads the status code and nothing else, and a 404 tells
// all of them the event does not exist. This service's index is bounded and
// starts empty after every restart, so it has no standing to say that — it gets
// 410, and only a graph that covers the whole history gets 404.
func TestABoundedIndexAnswers410NotFound404(t *testing.T) {
	s := newServer(t)
	w := get(t, s, "/v1/lineage/event/never-seen", func(h http.Header) {
		auth.SetPrincipalHeaders(h, "s1", "acme", []string{"steward"})
	})
	if w.Code == http.StatusNotFound {
		t.Fatalf("a miss on a bounded, restart-emptied index came back 404 — " +
			"'I have no record' served as 'there is no record'")
	}
	if w.Code != http.StatusGone {
		t.Fatalf("want 410, got %d (%s)", w.Code, w.Body.String())
	}

	var u query.Unresolved
	if err := json.Unmarshal(w.Body.Bytes(), &u); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if u.Status != graph.LookupUnknown {
		t.Errorf("status = %q, want %q", u.Status, graph.LookupUnknown)
	}
	if u.Detail == "" {
		t.Error("the refusal must say what it does NOT establish")
	}
	if u.Coverage.Capacity <= 0 {
		t.Errorf("coverage absent from the refusal: %+v", u.Coverage)
	}
	if u.Coverage.Complete {
		t.Error("coverage.complete true on a graph nothing declared complete")
	}
}

// The mirror image, or 410 would just be the new 404. A graph that has observed
// all of history and evicted nothing answers conclusively — a caller CAN still
// get a definite "no such event", when one is warranted.
func TestACompleteIndexStillAnswers404(t *testing.T) {
	g := newMemory(t, graph.WithCompleteHistory("acme"))
	observe(t, g, "e1", piiDS, "customer", "customer.v1.PersonProfile:1", time.Now(), "")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gov := governance.NewGovernor(governance.NewClassifier(nil),
		auth.NewPolicyAuthorizer(&auth.Policy{}))
	readiness := &server.Readiness{}
	readiness.Set(true)
	s := server.New(readiness, logger, server.WithLineage(query.NewService(g, gov)))

	w := get(t, s, "/v1/lineage/event/never-seen", func(h http.Header) {
		auth.SetPrincipalHeaders(h, "s1", "acme", []string{"steward"})
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404 from a complete index, got %d (%s)", w.Code, w.Body.String())
	}
	var u query.Unresolved
	if err := json.Unmarshal(w.Body.Bytes(), &u); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if u.Status != graph.LookupNotObserved {
		t.Errorf("status = %q, want %q", u.Status, graph.LookupNotObserved)
	}
}

// The catalog declares its coverage too: an empty catalog after a restart is
// this pod's index, not the estate's data model.
func TestCatalogDeclaresItsCoverage(t *testing.T) {
	s := newServer(t)
	w := get(t, s, "/v1/catalog/datasets", func(h http.Header) {
		auth.SetPrincipalHeaders(h, "v1", "acme", []string{"viewer"})
	})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var body struct {
		Count    int            `json:"count"`
		Coverage graph.Coverage `json:"coverage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Coverage.Capacity <= 0 || body.Coverage.Complete {
		t.Errorf("catalog coverage = %+v", body.Coverage)
	}
}

// The kubelet does not go through the gateway. A 401 here is a pod that never
// becomes ready.
func TestProbesAreReachableWithoutAPrincipal(t *testing.T) {
	s := newServer(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		if w := get(t, s, path, nil); w.Code != http.StatusOK {
			t.Errorf("%s: want 200, got %d (%s)", path, w.Code, w.Body.String())
		}
	}
}

func newMemory(t *testing.T, opts ...graph.Option) *graph.Memory {
	t.Helper()
	g, err := graph.NewMemory(graph.Limits{EventIndexMax: graph.DefaultEventIndexCapacity, DatasetMax: 128, EdgeMax: 512, StringBytesMax: 64 << 20}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return g
}
func observe(t *testing.T, g *graph.Memory, id string, ds graph.DatasetID, domain, schema string, at time.Time, cause string) graph.ObservationSnapshot {
	t.Helper()
	if schema == "" {
		schema = "fixture.v1." + ds.Name + ":1"
	}
	snapshot, err := g.Observe(graph.Observation{Owner: "acme", EventID: id, Dataset: ds, Domain: domain, SchemaRef: schema,
		At: at, ParentEventID: cause, Source: domain, CorrelationID: "corr-" + id})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestHTTPAdversarialEqualIdentityIsOwnerScoped(t *testing.T) {
	g := newMemory(t)
	// This deliberately violates global-ID uniqueness to test storage/HTTP
	// defense; the canonical authenticated broker test uses DISTINCT UUIDv7s.
	for _, owner := range []graph.Owner{"acme", "other"} {
		_, err := g.Observe(graph.Observation{Owner: owner, EventID: "shared", Dataset: derived, Domain: "risk",
			SchemaRef: "risk.v1." + string(owner) + ":1", At: time.Unix(1, 0), Source: "risk-engine", CorrelationID: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := g.Observe(graph.Observation{Owner: "other", EventID: "foreign-only", Dataset: piiDS, Domain: "customer",
		SchemaRef: "customer.v1.PersonProfile:1", At: time.Unix(2, 0), Source: "customer", CorrelationID: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gov := governance.NewGovernor(governance.NewClassifier(nil), auth.NewPolicyAuthorizer(&auth.Policy{Roles: map[string][]auth.Action{"admin": {"*"}}}))
	s := server.New(&server.Readiness{}, logger, server.WithLineage(query.NewService(g, gov)))
	for _, owner := range []string{"acme", "other"} {
		header := func(h http.Header) { auth.SetPrincipalHeaders(h, "equal-user", owner, []string{"admin"}) }
		w := get(t, s, "/v1/lineage/event/shared", header)
		var prov query.Provenance
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &prov) != nil || prov.Target.SchemaRef != "risk.v1."+owner+":1" {
			t.Fatal("HTTP selected a foreign stored owner")
		}
		catalog := get(t, s, "/v1/catalog/datasets", header)
		var body struct {
			Datasets []graph.Info   `json:"datasets"`
			Coverage graph.Coverage `json:"coverage"`
		}
		if catalog.Code != http.StatusOK || json.Unmarshal(catalog.Body.Bytes(), &body) != nil {
			t.Fatal("catalog refused legitimate owner")
		}
		for _, info := range body.Datasets {
			if info.ID == derived && info.SchemaRef != "risk.v1."+owner+":1" {
				t.Fatal("foreign catalog schema disclosed")
			}
		}
		if owner == "acme" && body.Coverage.Retained != 1 {
			t.Fatal("foreign activity escaped HTTP coverage")
		}
	}
	header := func(h http.Header) { auth.SetPrincipalHeaders(h, "equal-user", "acme", []string{"admin"}) }
	foreign := get(t, s, "/v1/lineage/event/foreign-only", header)
	absent := get(t, s, "/v1/lineage/event/absent", header)
	if foreign.Code != http.StatusGone || absent.Code != foreign.Code || !bytes.Equal(foreign.Body.Bytes(), absent.Body.Bytes()) {
		t.Fatal("cross-tenant existence oracle")
	}
	catalogForeign := get(t, s, "/v1/lineage/dataset/kanz.customer/PersonProfile", header)
	catalogAbsent := get(t, s, "/v1/lineage/dataset/kanz.customer/Absent", header)
	if catalogForeign.Code != http.StatusGone || !bytes.Equal(catalogForeign.Body.Bytes(), catalogAbsent.Body.Bytes()) {
		t.Fatal("cross-tenant dataset existence oracle")
	}
}

func TestEmptyAndForeignOnlyCatalogKeepAnEmptyArray(t *testing.T) {
	g := newMemory(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gov := governance.NewGovernor(governance.NewClassifier(nil), auth.NewPolicyAuthorizer(&auth.Policy{}))
	s := server.New(&server.Readiness{}, logger, server.WithLineage(query.NewService(g, gov)))
	header := func(h http.Header) { auth.SetPrincipalHeaders(h, "u", "acme", []string{"viewer"}) }
	empty := get(t, s, "/v1/catalog/datasets", header)
	if _, err := g.Observe(graph.Observation{Owner: "other", EventID: "foreign", Dataset: derived, Domain: "risk", SchemaRef: "risk.v1.ExposureSet:1",
		At: time.Unix(1, 0), Source: "risk-engine", CorrelationID: "fixture"}); err != nil {
		t.Fatal(err)
	}
	foreign := get(t, s, "/v1/catalog/datasets", header)
	if empty.Code != http.StatusOK || foreign.Code != http.StatusOK || !bytes.Equal(empty.Body.Bytes(), foreign.Body.Bytes()) {
		t.Fatal("foreign-only catalog differed from empty scoped catalog")
	}
	var body struct {
		Datasets json.RawMessage `json:"datasets"`
	}
	if err := json.Unmarshal(foreign.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body.Datasets, []byte("[]")) {
		t.Fatal("empty catalog array changed to null/missing")
	}
}
