package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/custody"
)

type scopeProofReader struct {
	calls     int
	replaceAt int
}

func (p *scopeProofReader) ProvenPortfolioAssignments(ctx context.Context, subject custody.Subject, _ map[string][]string) ([]custody.AccountAssignment, error) {
	if bus.TenantIDFromContext(ctx) != testTenant || subject.PortfolioID != dualPortfolio || !subject.BusinessDate.Equal(custody.BusinessDay(time.Now())) {
		return nil, custody.ErrUnprovenAssignment
	}
	p.calls++
	version := "v1"
	if p.replaceAt > 0 && p.calls >= p.replaceAt {
		version = "v2"
	}
	return []custody.AccountAssignment{{CustodianID: custodianA, AccountID: "acct", Version: version}}, nil
}

func TestAdHocReconciliationRefusesAnUnprovenOrReplacedAccount(t *testing.T) {
	s, _ := newServer(t)
	ctx := bus.WithTenantID(t.Context(), testTenant)
	pairs := []custody.Subject{{PortfolioID: dualPortfolio, CustodianID: custodianA}}
	body := `{"custodian_id":"CUST-A","positions":{},"cash":{}}`
	derived, err := custody.NewBookScope(pairs, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.custodyScope, err = derived.AdmitAssignments(ctx, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res := reconcileAs(t, s, dualPortfolio, body); res.Code != http.StatusServiceUnavailable || strings.Contains(res.Body.String(), "breaks") {
		t.Fatalf("returned a financial result for unassigned accounts: %d %s", res.Code, res.Body)
	}
	declared, err := custody.NewBookScope(pairs, map[string]map[string][]string{dualPortfolio: {custodianA: {"acct"}}})
	if err != nil {
		t.Fatal(err)
	}
	probe := new(scopeProofReader)
	s.custodyScope, err = declared.AdmitAssignments(ctx, time.Now(), probe)
	if err != nil {
		t.Fatal(err)
	}
	if res := reconcileAs(t, s, dualPortfolio, body); res.Code != http.StatusOK || probe.calls != 3 {
		t.Fatalf("authorized current-date proof unavailable: %d %s calls=%d", res.Code, res.Body, probe.calls)
	}
	probe.replaceAt = probe.calls + 2
	if res := reconcileAs(t, s, dualPortfolio, body); res.Code != http.StatusServiceUnavailable || strings.Contains(res.Body.String(), "breaks") {
		t.Fatalf("compared across assignment replacement: %d %s", res.Code, res.Body)
	}
}
