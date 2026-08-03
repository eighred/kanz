package server_test

import (
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
	g := graph.NewMemory()
	now := time.Now()
	g.Observe("e1", piiDS, "customer", "customer.v1.PersonProfile:1", now, "")
	g.Observe("e2", derived, "risk", "risk.v1.CustomerExposure:1", now, "e1")

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
	return server.New(readiness, logger, server.WithLineage(g, query.NewService(g, gov)))
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
