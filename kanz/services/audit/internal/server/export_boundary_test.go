package server

import (
	"context"
	"encoding/json"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/audit/internal/audit"
	"net/http/httptest"
	"testing"
	"time"
)

type exportSpy struct {
	filterSpy
	scans, heads int
}

func (s *exportSpy) Scan(context.Context, func(*audit.Record) error) error { s.scans++; return nil }
func (s *exportSpy) Head(context.Context) (audit.Head, error)              { s.heads++; return audit.Head{}, nil }
func (s *exportSpy) Query(_ context.Context, f audit.Filter) ([]*audit.Record, error) {
	s.got = f
	if f.ThroughSeq != nil && *f.ThroughSeq == 0 {
		return nil, nil
	}
	return []*audit.Record{{Seq: 9007199254740993, EventID: "a", TenantID: f.Tenant, OccurredAt: time.Unix(1, 0)}, {Seq: 9007199254740994, EventID: "b", TenantID: f.Tenant, OccurredAt: time.Unix(2, 0)}}, nil
}
func TestTenantReportDoesNotScanOrDiscloseGlobalIntegrity(t *testing.T) {
	spy := &exportSpy{}
	srv := newAuditServer(spy)
	srv.verifyRoles = []string{"estate-verifier"}
	req := httptest.NewRequest("GET", "/v2/audit/reports/full-log?limit=1&tenant=victim", nil)
	auth.SetPrincipalHeaders(req.Header, "test:reader", "acme", []string{"tenant-auditor"})
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != 200 || spy.scans != 0 || spy.heads != 0 || spy.got.Tenant != "acme" || spy.got.Limit != 2 {
		t.Fatalf("unbounded or unscoped report: %d %+v %s", rr.Code, spy, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	integrity := body["integrity"].(map[string]any)
	if len(integrity) != 1 || integrity["state"] != "not_requested" {
		t.Fatal(integrity)
	}
	if body["complete"] != false || body["next_cursor"] != "9007199254740993" || body["records"].([]any)[0].(map[string]any)["seq"] != "9007199254740993" {
		t.Fatal(body)
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("cacheable evidence")
	}
	for _, url := range []string{"/v1/audit/reports/full-log", "/v1/audit/verify"} {
		req := httptest.NewRequest("GET", url, nil)
		auth.SetPrincipalHeaders(req.Header, "test:reader", "acme", []string{"tenant-auditor"})
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		if rr.Code != 403 || spy.scans != 0 || spy.heads != 0 {
			t.Fatal("verification authorization bypass", url, rr.Code)
		}
	}
	req = httptest.NewRequest("GET", "/v1/audit/reports/full-log", nil)
	auth.SetPrincipalHeaders(req.Header, "test:operator", "acme", []string{"estate-verifier"})
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != 200 || spy.scans != 1 || spy.heads != 0 || spy.got.ThroughSeq == nil || *spy.got.ThroughSeq != 0 {
		t.Fatal("explicit verifier refused", rr.Code, spy)
	}
}
func TestEvidenceRejectsInvalidWindowsBeforeStoreRead(t *testing.T) {
	for _, q := range []string{"", "?from=invalid", "?from=2026-01-01T00:00:00Z&to=0001-01-01T00:00:00Z", "?from=2026-01-01T00:00:00Z&to=invalid", "?from=2026-02-01T00:00:00Z&to=2026-01-01T00:00:00Z"} {
		spy := &filterSpy{}
		srv := newAuditServer(spy)
		req := httptest.NewRequest("GET", "/v2/soc2/evidence"+q, nil)
		auth.SetPrincipalHeaders(req.Header, "test:reader", "acme", nil)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		if rr.Code != 400 || spy.got.Tenant != "" {
			t.Fatalf("invalid window read data: %s %d %+v", q, rr.Code, spy.got)
		}
	}
}
