package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/optimization"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	"github.com/eighred/kanz/pkg/auth"
)

type proposalScopeSource struct {
	calls  atomic.Int64
	source compliance.MandateSource
}

func (s *proposalScopeSource) Mandate(ctx context.Context, tenant, pf string, at time.Time) (*compliancepb.Mandate, compliance.Governance, error) {
	s.calls.Add(1)
	return s.source.Mandate(ctx, tenant, pf, at)
}

type proposalScopeResolver func(context.Context, string, string, time.Time) (*compliancepb.Mandate, compliance.Governance, error)

func (f proposalScopeResolver) Mandate(ctx context.Context, tenant, pf string, at time.Time) (*compliancepb.Mandate, compliance.Governance, error) {
	return f(ctx, tenant, pf, at)
}

func scopeProposalBody(pf string) string {
	encoded, _ := json.Marshal(pf)
	return strings.Replace(exactBody, `"PF"`, string(encoded), 1)
}

func scopeProposalRequest(t *testing.T, endpoint, tenant, pf string, claims []string, roles ...string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, endpoint+"/v2/propose", strings.NewReader(scopeProposalBody(pf)))
	if err != nil {
		t.Fatal(err)
	}
	auth.SetPrincipalHeaders(r.Header, "user:scope-reader", tenant, roles)
	if claims != nil {
		if err := auth.SetPrincipalPortfolios(r.Header, claims); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func scopeProposalResponse(t *testing.T, client *http.Client, req *http.Request) (int, []byte) {
	t.Helper()
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, data
}

func requireScopeExactProposal(t *testing.T, data []byte, pf string, verdict optimization.MandateStatus) {
	t.Helper()
	var p optimization.ExactProposal
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	if p.PortfolioID != pf || p.SchemaVersion != 2 || !p.ReadOnly || p.MandateStatus != verdict || len(p.Trades) != 1 ||
		p.Trades[0].Notional != "9007199254740993" || p.Trades[0].Quantity != "9007199254740993000000000000" {
		t.Fatalf("scope changed exact hypothetical proposal: %s", data)
	}
}

func TestProposalPortfolioScopeHTTPBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, target, raw, role string
		status                  int
		calls                   int64
	}{
		{"foreign", "PF-B", `["PF-A"]`, "pm", 403, 0},
		{"unknown", "not-present", `["PF-A"]`, "pm", 403, 0},
		{"admin", "PF-B", `["PF-A"]`, "admin", 403, 0},
		{"prefix", "PF-A-1", `["PF-A"]`, "pm", 403, 0},
		{"case", "pf-a", `["PF-A"]`, "pm", 403, 0},
		{"escaped_alias", "PF%2FA", `["PF/A"]`, "pm", 403, 0},
		{"padded_target", " PF-A ", `["PF-A"]`, "pm", 403, 0},
		{"unicode_distinct", "家", `["家庭"]`, "pm", 403, 0},
		{"listed", "PF-A", `["PF-B","PF-A"]`, "pm", 200, 1},
		{"absent", "PF-B", "", "pm", 200, 1},
		{"empty", "PF-B", `[]`, "pm", 200, 1},
		{"literal_path", "PF/A", `["PF/A"]`, "pm", 200, 1},
		{"literal_unicode", "家庭", `["家庭"]`, "pm", 200, 1},
		{"null", "PF-A", `null`, "pm", 401, 0},
		{"object", "PF-A", `{}`, "pm", 401, 0},
		{"mixed", "PF-A", `["PF-A",1]`, "pm", 401, 0},
		{"empty_id", "PF-A", `[""]`, "pm", 401, 0},
		{"padded_id", "PF-A", `[" PF-A "]`, "pm", 401, 0},
		{"repeated", "PF-A", `["PF-A"]`, "repeat", 401, 0},
		{"missing_principal", "PF-A", `["PF-A"]`, "anonymous", 401, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &proposalScopeSource{source: proposalScopeResolver(func(_ context.Context, tenant, pf string, _ time.Time) (*compliancepb.Mandate, compliance.Governance, error) {
				return concentrationMandate(tenant, pf, 100), compliance.Governed, nil
			})}
			s := gatedServer(t, src)
			s.materializerFor = func(string) Materializer { t.Error("read scope reached materialization"); return nil }
			upstream := httptest.NewServer(s)
			defer upstream.Close()
			r := scopeProposalRequest(t, upstream.URL, "scope-tenant", tc.target, nil, tc.role)
			if tc.raw != "" {
				r.Header.Set(auth.HeaderPrincipalPortfolios, tc.raw)
			}
			if tc.role == "repeat" {
				r.Header.Add(auth.HeaderPrincipalPortfolios, tc.raw)
			}
			if tc.role == "anonymous" {
				r.Header.Del(auth.HeaderPrincipalSubject)
			}
			status, data := scopeProposalResponse(t, upstream.Client(), r)
			if status != tc.status || src.calls.Load() != tc.calls || len(s.solves) != 0 {
				t.Fatalf("status=%d reads=%d solver=%d body=%s", status, src.calls.Load(), len(s.solves), data)
			}
			if status == 200 {
				requireScopeExactProposal(t, data, tc.target, optimization.MandateFeasible)
			}
			if status == 403 && string(data) != "{\"error\":\"portfolio outside authenticated scope\"}\n" {
				t.Fatalf("resource discovery in refusal: %s", data)
			}
		})
	}
}

func TestProposalPortfolioScopePrecedesCapacityAndSource(t *testing.T) {
	for _, name := range []string{"denied_full", "allowed_full", "denied_source_error", "allowed_source_error", "denied_unwired", "allowed_unwired"} {
		t.Run(name, func(t *testing.T) {
			src := &proposalScopeSource{source: proposalScopeResolver(func(context.Context, string, string, time.Time) (*compliancepb.Mandate, compliance.Governance, error) {
				return nil, compliance.GovernanceUnspecified, errors.New("secret-other-portfolio-source")
			})}
			s := gatedServer(t, src)
			if strings.HasSuffix(name, "unwired") {
				s.gate.Mandates = nil
			}
			if strings.HasSuffix(name, "full") {
				for range cap(s.solves) {
					s.solves <- struct{}{}
				}
			}
			before := len(s.solves)
			rec := httptest.NewRecorder()
			r := scopeProposalRequest(t, "http://scope.invalid", "acme", "PF", []string{"PF"}, "pm")
			denied := strings.HasPrefix(name, "denied")
			if denied {
				_ = auth.SetPrincipalPortfolios(r.Header, []string{"other"})
			}
			s.ServeHTTP(rec, r)
			want, reads := 503, int64(0)
			if name == "allowed_source_error" {
				reads = 1
			}
			if name == "allowed_unwired" {
				want = 200
			}
			if denied {
				want = 403
			}
			if rec.Code != want || src.calls.Load() != reads || len(s.solves) != before {
				t.Fatalf("status=%d reads=%d solver=%d body=%s", rec.Code, src.calls.Load(), len(s.solves), rec.Body.String())
			}
			if denied && strings.Contains(rec.Body.String(), "secret") {
				t.Fatal("source error escaped scope boundary")
			}
			if want == 200 {
				requireScopeExactProposal(t, rec.Body.Bytes(), "PF", optimization.MandateUnchecked)
			}
		})
	}
}

func TestProposalPortfolioScopeReauthorizesEveryRequest(t *testing.T) {
	src := &proposalScopeSource{source: proposalScopeResolver(func(_ context.Context, tenant, pf string, _ time.Time) (*compliancepb.Mandate, compliance.Governance, error) {
		return concentrationMandate(tenant, pf, 100), compliance.Governed, nil
	})}
	s := gatedServer(t, src)
	upstream := httptest.NewServer(s)
	defer upstream.Close()
	for _, phase := range []string{"authorized", "revoked", "restored", "other_tenant"} {
		t.Run(phase, func(t *testing.T) {
			claims, tenant, want := []string{"PF"}, "acme", 200
			if phase == "revoked" {
				claims, want = []string{"other"}, 403
			}
			if phase == "other_tenant" {
				tenant = "globex"
			}
			before := src.calls.Load()
			status, data := scopeProposalResponse(t, upstream.Client(), scopeProposalRequest(t, upstream.URL, tenant, "PF", claims, "pm"))
			reads := int64(1)
			if want == 403 {
				reads = 0
			}
			if status != want || src.calls.Load()-before != reads {
				t.Fatalf("%d %s", status, data)
			}
			if want == 200 {
				requireScopeExactProposal(t, data, "PF", optimization.MandateFeasible)
			}
		})
	}
}
