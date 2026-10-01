package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/optimization"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	"github.com/eighred/kanz/pkg/auth"
)

const exactBody = `{"portfolio_id":"PF","instruments":["A"],"expected_returns":[0.1],"objective":{"Type":0},"current_weights":{},"nav":"9007199254740993","prices":{"A":"0.000000000001"},"threshold":"0","currency":"USD"}`

func TestExactProposalRealHTTPPreservesValuesAndNeverPublishes(t *testing.T) {
	s := newTestServer()
	s.materializerFor = func(string) Materializer { t.Error("read-only route reached materialization"); return nil }
	upstream := httptest.NewServer(s)
	defer upstream.Close()
	req, err := http.NewRequest(http.MethodPost, upstream.URL+"/v2/propose", strings.NewReader(exactBody))
	if err != nil {
		t.Fatal(err)
	}
	auth.SetPrincipalHeaders(req.Header, "user:pm", "acme", []string{"pm"})
	response, err := upstream.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		t.Fatalf("%d %s", response.StatusCode, body)
	}
	var p optimization.ExactProposal
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	if p.SchemaVersion != 2 || !p.ReadOnly || p.Trades[0].Notional != "9007199254740993" || p.Trades[0].Quantity != "9007199254740993000000000000" {
		t.Fatalf("invalid response: %s", body)
	}
}

func TestLegacyProposalExplicitlyRetired(t *testing.T) {
	r := asPrincipal(t, newTestServer(), http.MethodPost, "/v1/propose", exactBody, "pm")
	if r.Code != http.StatusGone || !strings.Contains(r.Body.String(), "/v2/propose") {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
}

func TestExactProposalBoundaryRefusesNumbersUnknownsAndExtraDocuments(t *testing.T) {
	for _, body := range []string{
		strings.Replace(exactBody, `"9007199254740993"`, `9007199254740993`, 1),
		strings.Replace(exactBody, `"0.000000000001"`, `0.000000000001`, 1),
		strings.Replace(exactBody, `"threshold":"0"`, `"threshold":null`, 1),
		strings.Replace(exactBody, `"current_weights":{}`, `"current_weights":null`, 1),
		strings.Replace(exactBody, `"currency":"USD"`, `"currency":"USD","approved":true`, 1),
		exactBody + ` {}`,
		strings.Replace(exactBody, `"nav":"9007199254740993"`, `"nav":"1","NAV":"2"`, 1),
	} {
		r := asPrincipal(t, newTestServer(), http.MethodPost, "/v2/propose", body, "pm")
		if r.Code != http.StatusBadRequest {
			t.Fatalf("accepted invalid request: %d %s", r.Code, r.Body)
		}
	}
}

func TestExactProposalCapacityIsBounded(t *testing.T) {
	s := newTestServer()
	for i := 0; i < cap(s.solves); i++ {
		s.solves <- struct{}{}
	}
	r := asPrincipal(t, s, http.MethodPost, "/v2/propose", exactBody, "pm")
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("unbounded solve: %d", r.Code)
	}
}

func TestExactProposalRefusesMismatchedMandateIdentity(t *testing.T) {
	s := gatedServer(t, staticMandates{byTenant: map[string]*compliancepb.Mandate{"acme": concentrationMandate("another-tenant", "PF", 100)}})
	r := asPrincipal(t, s, http.MethodPost, "/v2/propose", exactBody, "pm")
	if r.Code != http.StatusUnprocessableEntity {
		t.Fatalf("mismatched tenant accepted: %d %s", r.Code, r.Body)
	}
}
