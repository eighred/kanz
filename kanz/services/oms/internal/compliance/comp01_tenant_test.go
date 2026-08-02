package compliance

import (
	"context"
	"strings"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/pkg/bus"
)

// tenantMandate is concentrationMandate filed for a named tenant, so two of them
// can collide on one portfolio name the way #243 describes.
func tenantMandate(tenant string, maxPct int64) *compliancepb.Mandate {
	m := concentrationMandate(maxPct)
	m.MandateId = "m-" + tenant
	m.TenantId = tenant
	m.EffectiveAt = timestamppb.New(time.Unix(0, 0))
	return m
}

// AN ORDER WHOSE MANDATE CANNOT BE ATTRIBUTED IS REFUSED UNDER ITS OWN CODE.
//
// It is not MANDATE_MISSING: a mandate exists, and sending a reviewer to write
// one they already wrote is the wrong instruction. It is not a rule code either
// — nothing was evaluated. And it is a *Breach rather than an error, because an
// error redelivers the command forever while the ambiguity never resolves.
func TestCheck_UnresolvableTenantMapsToItsOwnBreachCode(t *testing.T) {
	reg := comp.NewMandateRegistry()
	mustPut(t, reg, tenantMandate("acme", 60))
	mustPut(t, reg, tenantMandate("beta", 10))
	gate := comp.NewPreTradeGate(nil, comp.MapBookSource{}, reg, nil, nil, nil)
	g := NewCOMP01Gate(gate, "USD")

	cmd := unpricedOrder(orderpb.OrderType_ORDER_TYPE_LIMIT)
	breach, err := g.Check(context.Background(), bus.SystemTenant, cmd)
	if err != nil {
		t.Fatalf("a terminal ambiguity must not be returned as a retryable error: %v", err)
	}
	if breach == nil {
		t.Fatal("the order was ADMITTED although the platform could not say whose mandate governs it")
	}
	if breach.Code != "MANDATE_TENANT_UNRESOLVED" {
		t.Fatalf("code %q — MANDATE_MISSING sends a reviewer to write a mandate that already "+
			"exists, and a rule code sends them to find a rule that never ran", breach.Code)
	}
	// The reason reaches the SUBMITTING CLIENT on the ORDER_REJECTED FACT. The
	// other tenants involved are an operator diagnosis and belong in the OMS's
	// log, not in another customer's rejection message.
	for _, leaked := range []string{"acme", "beta"} {
		if strings.Contains(breach.Reason, leaked) {
			t.Fatalf("the rejection reason names another tenant (%q): %q", leaked, breach.Reason)
		}
	}
}

// The ordinary path still works, and it works PER TENANT: the same portfolio
// name under two tenants resolves each tenant's own cap.
func TestCheck_ResolvesTheCallingTenantsMandate(t *testing.T) {
	reg := comp.NewMandateRegistry()
	mustPut(t, reg, tenantMandate("acme", 60))
	mustPut(t, reg, tenantMandate("beta", 10))
	gate := comp.NewPreTradeGate(nil, comp.MapBookSource{}, reg, nil, nil, nil)
	g := NewCOMP01Gate(gate, "USD")

	// An empty book plus a 10-unit buy is 100% of one instrument: it breaches
	// beta's 10% cap and acme's 60% cap alike, so the CODE cannot tell them
	// apart — but a resolution failure would surface as MANDATE_TENANT_UNRESOLVED
	// or MANDATE_MISSING, and neither may appear here.
	for _, tenant := range []string{"acme", "beta"} {
		breach, err := g.Check(context.Background(), tenant, pricedOrder())
		if err != nil {
			t.Fatalf("%s: %v", tenant, err)
		}
		if breach == nil {
			t.Fatalf("%s: a 100%% concentration passed a capped mandate", tenant)
		}
		if breach.Code == "MANDATE_MISSING" || breach.Code == "MANDATE_TENANT_UNRESOLVED" {
			t.Fatalf("%s: its own mandate did not resolve (%s) — a per-tenant lookup must find "+
				"a per-tenant mandate", tenant, breach.Code)
		}
	}
}

func pricedOrder() *orderpb.SubmitOrder {
	cmd := unpricedOrder(orderpb.OrderType_ORDER_TYPE_LIMIT)
	cmd.LimitPrice = &commonpb.Decimal{Coefficient: 1000, Exponent: 0}
	return cmd
}
