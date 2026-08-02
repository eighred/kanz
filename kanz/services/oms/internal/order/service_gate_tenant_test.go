package order

import (
	"context"
	"testing"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/services/oms/internal/compliance"
)

// tenantSpyGate records the tenant the handler scoped the pre-trade check to.
type tenantSpyGate struct{ saw []string }

func (g *tenantSpyGate) Check(_ context.Context, tenantID string, _ *orderpb.SubmitOrder) (*compliance.Breach, error) {
	g.saw = append(g.saw, tenantID)
	return nil, nil
}

// THE PRE-TRADE GATE IS SCOPED TO THE ORDER'S TENANT, NOT THIS OMS'S (#243).
//
// SubmitOrder carries no tenant of its own and portfolio_id is a caller-chosen
// string, so the mandate registry had only "growth" to key on and handed one
// tenant's order another tenant's concentration limits. The tenant that fixes
// that is the ENVELOPE's — the api-gateway stamps the authenticated caller's
// tenant and bus.Validate requires it non-empty on the live path.
//
// s.tenant is the wrong answer and it is not a hypothetical wrong answer: the
// shipped OMS runs OMS_TENANT="__system__" (infra/deploy/oms-deploy.yaml), so
// scoping to it would ask for the platform's mandate for every customer order —
// finding none, and admitting them unconstrained under OMS_REQUIRE_MANDATE=false.
func TestThePreTradeGateIsScopedToTheEnvelopesTenantNotTheServices(t *testing.T) {
	fb := &fakeBus{}
	spy := &tenantSpyGate{}
	svc, _ := newService(t, fb, spy)

	env := &envelopepb.Envelope{EventType: SubjectSubmit, TenantId: "acme"}
	if err := svc.Handle(testCtx(), env, mustMarshal(t, limitOrder(d(1, 0), d(1000, -2)))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(spy.saw) != 1 {
		t.Fatalf("the gate was called %d time(s), want 1", len(spy.saw))
	}
	if spy.saw[0] == testTenant {
		t.Fatalf("the gate was scoped to the OMS's own tenant %q — every customer order would "+
			"resolve the platform's mandate instead of the customer's (#243)", testTenant)
	}
	if spy.saw[0] != "acme" {
		t.Fatalf("the gate was scoped to %q, want the envelope's tenant %q", spy.saw[0], "acme")
	}
}
