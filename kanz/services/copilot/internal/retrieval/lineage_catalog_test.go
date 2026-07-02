package retrieval_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kanz-eng/kanz/pkg/auth"
	"github.com/kanz-eng/kanz/services/copilot/internal/retrieval"
)

func TestLineageCatalogResolvesDatasetNode(t *testing.T) {
	var gotPath, gotSubject, gotTenant, gotRoles string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotSubject = r.Header.Get("X-Kanz-Principal-Subject")
		gotTenant = r.Header.Get("X-Kanz-Principal-Tenant")
		gotRoles = r.Header.Get("X-Kanz-Principal-Roles")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"target":{"dataset":{"namespace":"risk","name":"var_es_daily"}},"upstream":[]}`))
	}))
	defer srv.Close()

	cat := retrieval.NewLineageCatalog(srv.Client(), srv.URL)
	ctx := auth.WithPrincipal(context.Background(),
		&auth.Principal{Subject: "u1", Tenant: "acme", Roles: []string{"analyst", "pm"}})

	node, ok := cat.Resolve(ctx, "evt-123")
	if !ok {
		t.Fatal("expected resolution to succeed")
	}
	if node != "risk.var_es_daily" {
		t.Errorf("node = %q want risk.var_es_daily", node)
	}
	// The event id is path-escaped into the lineage endpoint.
	if gotPath != "/v1/lineage/event/evt-123" {
		t.Errorf("path = %q want /v1/lineage/event/evt-123", gotPath)
	}
	// The END-USER principal is propagated as the trusted mesh headers so PII
	// governance applies to the user, not the copilot SVID.
	if gotSubject != "u1" || gotTenant != "acme" || gotRoles != "analyst,pm" {
		t.Errorf("propagated principal = (%q,%q,%q) want (u1,acme,analyst,pm)", gotSubject, gotTenant, gotRoles)
	}
}

// A 403 (PII lineage denied) or 404 (unknown) resolves to ok=false so the
// citation falls back to the raw event id — never breaking the answer.
func TestLineageCatalogDeniedOrUnknownFallsBack(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":"x"}`))
		}))
		if node, ok := retrieval.NewLineageCatalog(srv.Client(), srv.URL).Resolve(context.Background(), "evt-1"); ok || node != "" {
			t.Errorf("status %d: got (%q,%v) want (\"\",false)", code, node, ok)
		}
		srv.Close()
	}
}

func TestLineageCatalogEmptyIDAndUnreachable(t *testing.T) {
	cat := retrieval.NewLineageCatalog(http.DefaultClient, "http://127.0.0.1:0")
	if _, ok := cat.Resolve(context.Background(), ""); ok {
		t.Error("empty event id should not resolve")
	}
	// An unreachable lineage service degrades to unresolved, not an error.
	if _, ok := cat.Resolve(context.Background(), "evt-1"); ok {
		t.Error("unreachable service should degrade to ok=false")
	}
}

// Satisfies the Catalog seam, so it drops into the tool registry in place of
// IdentityCatalog with no agent/tools changes.
func TestLineageCatalogSatisfiesCatalog(t *testing.T) {
	var _ retrieval.Catalog = retrieval.NewLineageCatalog(nil, "http://lineage")
}
