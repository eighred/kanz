package server

import (
	"github.com/eighred/kanz/services/accounting/internal/collateralops"
	"net/http/httptest"
	"testing"
)

func TestCollateralRoutesRefuseMissingIdentityBeforeStore(t *testing.T) {
	s := New(&Readiness{}, nil, nil, "USD", WithTenant("tenant-A"), WithCollateral(collateralops.New(nil)))
	for _, method := range []string{"GET", "POST"} {
		path := "/v1/portfolios/PF1/collateral/workflow-1"
		if method == "POST" {
			path = "/v1/portfolios/PF1/collateral/actions"
		}
		r := httptest.NewRequest(method, path, nil)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatalf("anonymous route %s: %d", method, w.Code)
		}
	}
}
