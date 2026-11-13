package server

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/optimization"
	"github.com/eighred/kanz/pkg/auth"
)

func TestBlackLittermanProposalRealHTTPVarianceRefusalAndControls(t *testing.T) {
	s := newTestServer()
	s.materializerFor = func(string) Materializer { t.Error("read-only proposal reached materialization"); return nil }
	upstream := httptest.NewServer(s)
	defer upstream.Close()
	for _, tc := range []struct {
		name, variance string
		status         int
		want           float64
	}{{"negative_variance", "-1", 400, 0}, {"zero_default", "0", 200, 2}, {"unit_variance", "1", 200, 2}, {"variance_three", "3", 200, 1.5}} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"portfolio_id":"PF","instruments":["A"],"covariance":[[1]],"objective":{"Type":0},"current_weights":{},"nav":"100","prices":{"A":"10"},"threshold":"0","currency":"USD","black_litterman":{"market_weights":[1],"risk_aversion":1,"tau":1,"views":[{"p":[1],"q":3,"omega":%s}]}}`, tc.variance)
			req, err := http.NewRequest(http.MethodPost, upstream.URL+"/v2/propose", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			auth.SetPrincipalHeaders(req.Header, "user:pm", "acme", []string{"pm"})
			response, err := upstream.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			data, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != tc.status {
				t.Fatalf("status=%d body=%s err=%v", response.StatusCode, data, err)
			}
			if tc.status == 400 {
				var refusal map[string]json.RawMessage
				if err := json.Unmarshal(data, &refusal); err != nil || len(refusal) != 1 || refusal["error"] == nil {
					t.Fatalf("refusal carried proposal fields: %s err=%v", data, err)
				}
				return
			}
			var proposal optimization.ExactProposal
			if err := json.Unmarshal(data, &proposal); err != nil {
				t.Fatal(err)
			}
			if !proposal.ReadOnly || proposal.SchemaVersion != 2 || math.IsNaN(proposal.ExpectedReturn) || math.IsInf(proposal.ExpectedReturn, 0) || math.Abs(proposal.ExpectedReturn-tc.want) > 1e-12 {
				t.Fatalf("invalid proposal: %s", data)
			}
		})
	}
}
