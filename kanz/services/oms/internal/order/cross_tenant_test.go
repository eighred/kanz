package order

import (
	"context"
	"strings"
	"testing"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/platform/halt"
)

// The OMS resolved the venue account from the ENVELOPE's tenant and stored the
// order under its OWN (#223).
//
// handleSubmit calls resolveAccount(env.GetTenantId(), …) while postgres.go
// inserts `VALUES (current_setting('app.tenant_id'), …)` — the GUC
// pg.NewTenantPool pins to cfg.Tenant. Nothing compared them, so an order
// admitted from an acme envelope was stored as __system__, and every tenant's
// book collapsed into one RLS partition.
//
// The check sits at Handle, before the dispatch switch, because handleCancel and
// handleAmend do not even receive the envelope — a per-handler check would have
// to thread it through and would be forgotten by the next command added.

func serviceServing(t *testing.T, tenant string) *Service {
	t.Helper()
	svc, err := NewService(tenant, NewMemoryStore(), NewEmitter(&fakeBus{}), nil, nil, nil, nil, WithHaltGate(halt.OpenGate(nil)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func TestHandleRefusesAnotherTenantsCommand(t *testing.T) {
	svc := serviceServing(t, "acme")

	for _, evt := range []string{SubjectSubmit, SubjectCancel, SubjectAmend} {
		err := svc.Handle(context.Background(),
			&envelopepb.Envelope{EventType: evt, TenantId: "beta"}, nil)

		if err == nil {
			t.Errorf("%s: accepted an envelope from tenant beta on an OMS serving acme — "+
				"the order would be admitted from beta's command and stored as acme's", evt)
			continue
		}
		if !strings.Contains(err.Error(), "cross-tenant event") {
			t.Errorf("%s: error = %q, want it to name the cross-tenant refusal so the DLQ entry "+
				"says why", evt, err.Error())
		}
	}
}

// An untenanted envelope cannot be proven to belong here either.
func TestHandleRefusesAnUntenantedCommand(t *testing.T) {
	svc := serviceServing(t, "acme")

	err := svc.Handle(context.Background(),
		&envelopepb.Envelope{EventType: SubjectSubmit, TenantId: ""}, nil)
	if err == nil {
		t.Fatal("accepted an untenanted envelope on a tenant-dedicated OMS")
	}
	if !strings.Contains(err.Error(), "untenanted event") {
		t.Errorf("error = %q, want it to name the untenanted refusal", err.Error())
	}
}

// THE SHARED DEPLOYMENT MUST KEEP WORKING. The api-gateway stamps the CALLER's
// tenant on the command (orders.go: `TenantID: p.Tenant`), while the shipped OMS
// runs OMS_TENANT="__system__" — so on today's estate every genuine order is
// already a tenant mismatch. A blanket refusal would have rejected all of them.
//
// This is the branch that makes the change safe to land before #97, and it is
// the one that must be deleted when per-tenant compute arrives.
func TestSystemTenantOMSStillAcceptsARealTenantsCommand(t *testing.T) {
	svc := serviceServing(t, "__system__")

	err := svc.Handle(context.Background(),
		&envelopepb.Envelope{EventType: SubjectSubmit, TenantId: "acme"}, nil)

	// The command is malformed (nil payload), so an error is expected — but it
	// must be a DECODE failure, not the tenant refusal. Asserting on which error
	// is the point: a tenant refusal here would mean the shared estate is broken.
	if err != nil && strings.Contains(err.Error(), "cross-tenant event") {
		t.Fatalf("the shared __system__ OMS refused a real tenant's command (%v) — that is "+
			"every order on the current deployment", err)
	}
}

// A service that cannot say whose book it writes must not be constructible.
func TestNewServiceRefusesAnEmptyTenant(t *testing.T) {
	_, err := NewService("", NewMemoryStore(), NewEmitter(&fakeBus{}), nil, nil, nil, nil, WithHaltGate(halt.OpenGate(nil)))
	if err == nil {
		t.Fatal("NewService accepted an empty tenant — every order it stored would be scoped " +
			"to nothing, and it could not tell its own events from another tenant's")
	}
}
