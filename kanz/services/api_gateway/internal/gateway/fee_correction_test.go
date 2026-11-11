package gateway_test

import (
	"net/http"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

func TestFeeCorrectionReviewChecksPortfolioAndOwnerBeforeDisclosure(t *testing.T) {
	for _, tc := range []struct {
		name, scope, owner, portfolio string
		want                          int
		called                        bool
	}{
		{"allowed", "fund", testTenant, "fund", 200, true},
		{"wrong scope", "other", testTenant, "fund", 404, false},
		{"wrong owner", "fund", "other", "fund", 404, true},
		{"wrong response portfolio", "fund", testTenant, "other", 404, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeClient{feeResp: &orderpb.GetExecutionFeeCorrectionResponse{OwnerTenant: tc.owner, Proposal: &orderpb.ExecutionFeeCorrectionProposed{CaseId: "case", PortfolioId: tc.portfolio}}}
			srv := serveScoped(t, fc, tc.scope)
			resp, err := http.Get(srv.URL + "/v1/portfolios/fund/execution-recoveries/case/fee-correction")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.want || (fc.gotFee != nil) != tc.called {
				t.Fatalf("status=%d request=%v", resp.StatusCode, fc.gotFee)
			}
		})
	}
}
