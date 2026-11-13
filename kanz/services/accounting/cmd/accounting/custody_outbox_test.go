package main

import (
	"testing"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// Embedding only the public Store surface deliberately hides the relay queue.
type journalWithoutOutbox struct{ ledger.Store }

func TestCustodySharesAccountingMemoryRelay(t *testing.T) {
	journal := ledger.NewMemoryStore()
	st, err := newCustodyStore(nil, journal)
	if err != nil || st.Outbox() != journal.Outbox() {
		t.Fatalf("custody completion queue is detached from accounting relay: %v", err)
	}
	if _, err := newCustodyStore(nil, journalWithoutOutbox{journal}); err == nil {
		t.Fatal("accepted a journal without a recovery relay")
	}
}

func TestCustodyScheduledTenantFallbackPreservesResolvedTenant(t *testing.T) {
	if got := bus.TenantIDFromContext(custodyRunContext(t.Context(), "dedicated")); got != "dedicated" {
		t.Fatalf("scheduled scope=%q", got)
	}
	ctx := bus.WithTenantID(t.Context(), "customer")
	if got := bus.TenantIDFromContext(custodyRunContext(ctx, bus.SystemTenant)); got != "customer" {
		t.Fatalf("resolved customer tenant overwritten: %q", got)
	}
}
