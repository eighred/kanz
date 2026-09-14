package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/copilot/internal/agent"
	"github.com/eighred/kanz/services/copilot/internal/llm"
)

type boundaryModel struct{ calls int }

func (m *boundaryModel) Complete(context.Context, llm.Request) (llm.Response, error) {
	m.calls++
	return llm.Response{}, errors.New("TEST-ONLY provider diagnostic that must remain private")
}

func TestQuestionBoundaryBeforeModel(t *testing.T) {
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"question":""}`, 400},
		{`{"question":"   "}`, 400},
		{`{"question":"test","unexpected":true}`, 400},
		{`{"question":"test"} {}`, 400},
		{`{"question":"test"} trailing`, 400},
		{`{"question":"` + strings.Repeat("x", agent.MaxQuestionBytes+1) + `"}`, 413},
		{`{"question":"test"}` + strings.Repeat(" ", 128*1024), 413},
	} {
		m := &boundaryModel{}
		s := newServerWithModel(t, m)
		req := httptest.NewRequest(http.MethodPost, "/v1/ask", strings.NewReader(tc.body))
		auth.SetPrincipalHeaders(req.Header, "test-actor", "test-tenant", []string{"analyst"})
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != tc.status || m.calls != 0 || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("status=%d calls=%d headers=%v", rec.Code, m.calls, rec.Header())
		}
	}
}

func TestProviderFailureIsUnavailableWithoutDiagnostic(t *testing.T) {
	m := &boundaryModel{}
	s := newServerWithModel(t, m)
	req := httptest.NewRequest(http.MethodPost, "/v1/ask", strings.NewReader(`{"question":"test investigation"}`))
	auth.SetPrincipalHeaders(req.Header, "test-actor", "test-tenant", []string{"analyst"})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 503 || m.calls != 1 || strings.Contains(rec.Body.String(), "diagnostic") || !strings.Contains(rec.Body.String(), `"code":"unavailable"`) {
		t.Fatalf("unsafe failure: %d %s", rec.Code, rec.Body.String())
	}
}
