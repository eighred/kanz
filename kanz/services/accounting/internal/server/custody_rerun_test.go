package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/custody"
)

func TestCustodyRerunAuthorityAndReceipt(t *testing.T) {
	subject := custody.Subject{PortfolioID: "PF1", CustodianID: "CUST-A", BusinessDate: time.Date(2020, 2, 4, 0, 0, 0, 0, time.UTC)}
	scope, err := custody.NewBookScope([]custody.Subject{subject}, nil)
	if err != nil {
		t.Fatal(err)
	}
	const requestID = "b41e53d7-b3b2-4ee4-9b8e-45b8cab29289"
	const body = `{"request_id":"b41e53d7-b3b2-4ee4-9b8e-45b8cab29289","business_date":"2020-02-04","custodian_id":"CUST-A"}`
	calls := 0
	var fail error
	var historical bool
	compare := func(ctx context.Context, s custody.Subject, id, actor string) (custody.Run, error) {
		calls++
		if bus.TenantIDFromContext(ctx) != "fixture" || s != subject || id != requestID || actor != "operator" {
			t.Fatal("wrong authenticated comparison binding")
		}
		if historical {
			return custody.Run{RequestID: id, Actor: actor, RunID: "failed-run", Subject: s, Outcome: custody.OutcomeFailed, CompletedAt: time.Now().UTC()}, custody.ErrHistoricalLifecycle
		}
		if fail != nil {
			return custody.Run{}, fail
		}
		return custody.Run{RequestID: id, Actor: actor, RunID: "persisted-run", Subject: s, Outcome: custody.OutcomeNoStatement, CompletedAt: time.Now().UTC()}, nil
	}
	options := []Option{WithTenant("fixture"), WithBreakStore(custody.NewMemoryStore()), WithCustodyBookScope(scope), WithCustodyReruns("rerunner", compare)}
	srv := New(&Readiness{}, nil, nil, "USD", options...)
	post := func(server *Server, tenant, role, portfolio, payload string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", "/v1/portfolios/PF1/custody/reruns", strings.NewReader(payload))
		auth.SetPrincipalHeaders(req.Header, "operator", tenant, []string{role})
		if err := auth.SetPrincipalPortfolios(req.Header, []string{portfolio}); err != nil {
			t.Fatal(err)
		}
		out := httptest.NewRecorder()
		server.ServeHTTP(out, req)
		return out
	}
	for _, tc := range []struct {
		tenant, role, portfolio, payload string
		status                           int
	}{
		{"foreign", "rerunner", "PF1", body, 404}, {"fixture", "fund", "PF1", body, 403}, {"fixture", "rerunner", "PF2", body, 404},
		{"fixture", "rerunner", "PF1", strings.Replace(body, "CUST-A", "CUST-X", 1), 404},
		{"fixture", "rerunner", "PF1", strings.Replace(body, "2020-02-04", "2099-02-04", 1), 400},
		{"fixture", "rerunner", "PF1", strings.Replace(body, "2020-02-04", "2020-02-31", 1), 400},
		{"fixture", "rerunner", "PF1", strings.Replace(body, "}", `,"actor":"forged"}`, 1), 400},
		{"fixture", "rerunner", "PF1", body + body, 400},
	} {
		if got := post(srv, tc.tenant, tc.role, tc.portfolio, tc.payload); got.Code != tc.status {
			t.Fatalf("want %d, got %d", tc.status, got.Code)
		}
	}
	readonly := New(&Readiness{}, nil, nil, "USD", append(options, WithReadOnlyCustodyReview())...)
	if got := post(readonly, "fixture", "rerunner", "PF1", body); got.Code != 404 || calls != 0 {
		t.Fatal("refused request reached comparator")
	}
	got := post(srv, "fixture", "rerunner", "PF1", body)
	var receipt rerunReceipt
	if got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &receipt) != nil || receipt.Outcome != "no_statement" || receipt.StatementID != "" || receipt.RequestID != requestID || receipt.Actor != "operator" {
		t.Fatalf("unexpected receipt %s", got.Body.String())
	}
	for _, tc := range []struct {
		err    error
		status int
	}{{custody.ErrInvalidManualRequest, 400}, {custody.ErrManualRequestConflict, 409}, {custody.ErrPublicationPending, 503}, {custody.ErrHistoricalLifecycle, 409}, {errors.New("private failure"), 503}} {
		fail = tc.err
		got := post(srv, "fixture", "rerunner", "PF1", body)
		if got.Code != tc.status || strings.Contains(got.Body.String(), "private failure") {
			t.Fatalf("unexpected refusal: %s", got.Body.String())
		}
	}
	historical = true
	got = post(srv, "fixture", "rerunner", "PF1", body)
	if got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &receipt) != nil || receipt.RunID != "failed-run" || receipt.Outcome != "failed" || receipt.FailureCode != "historical_lifecycle" {
		t.Fatal("committed historical failure receipt lost")
	}

}
