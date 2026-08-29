package order

import (
	"context"
	"testing"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/services/oms/internal/compliance"
)

// THE AMEND CONTROLS, AGAINST THE DURABLE STORE (#799). Gated on
// TEST_POSTGRES_URL — it skips without a database.
//
// WHY THIS EXISTS BESIDE service_amend_controls_test.go, WHICH ALREADY PROVES
// THE REFUSAL. Those run on MemoryStore, where "the amend was refused" and "the
// row did not change" are the same fact by construction: nothing is written, so
// nothing can be half-written. The durable path is where they come apart.
// handleAmend builds an outcome FACT, commits it WITH the amended state through
// Save's one transaction (#292), and flushes the relay — so a refusal placed on
// the wrong side of that sequence would publish REJECTED and leave the larger
// quantity committed, or commit the amendment and answer REJECTED. Neither is
// visible without a database.
//
// It also proves the refusal survives the CAS: Save is compare-and-swap on the
// version, so a control that refused after a write would show up here as a
// version that moved.
func TestAmend_RefusalLeavesTheDurableOrderAndItsOutboxUntouched(t *testing.T) {
	pool := newPool(t)
	fb := &fakeBus{}
	gate := &stagedGate{breach: &compliance.Breach{Code: "CONCENTRATION", Reason: "over sector cap"}}

	store := NewPostgres(pool)
	svc, err := NewService(testTenant, store, NewEmitter(fb), gate, nil, nil, nil,
		WithHaltGate(halt.OpenGate(nil)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ctx := testCtx()

	if err := svc.Handle(ctx, submitEnv(), mustMarshal(t, limitOrder(d(100, 0), d(1025, -2)))); err != nil {
		t.Fatalf("submit: %v", err)
	}
	before, verBefore, err := store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if before.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW {
		t.Fatalf("status = %v, want PENDING_NEW — no router is wired, so nothing should have "+
			"worked this order", before.GetStatus())
	}

	// The mandate now refuses the size this amend asks for.
	gate.armed = true
	if err := svc.Handle(ctx, amendEnvFor("acme"), mustMarshal(t, amendPriced(d(1000000, 0), nil))); err != nil {
		t.Fatalf("amend: %v", err)
	}

	status, code := outcomeCode(t, fb)
	if status != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED ||
		code != "COMPLIANCE_CONCENTRATION" {
		t.Fatalf("outcome = %v/%q, want REJECTED/COMPLIANCE_CONCENTRATION", status, code)
	}

	after, verAfter, err := store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if dec.Cmp(after.GetOrderedQuantity(), d(100, 0)) != 0 {
		t.Fatalf("durable ordered quantity = %v, want 100 — the refusal was announced over a "+
			"committed amendment, which is the original defect wearing a refusal's label",
			after.GetOrderedQuantity())
	}
	// THE VERSION IS THE PROOF NOTHING WAS WRITTEN. Save is compare-and-swap on
	// it (cas_test.go), so a control that refused only after committing would
	// leave it moved even where the quantity happened to round back.
	if verAfter != verBefore {
		t.Fatalf("order version moved %d → %d for a refused amend — something was committed",
			verBefore, verAfter)
	}

	// AND THE REDUCTION STILL COMMITS, against the same durable store and the
	// same breaching gate. Without this the test above would pass on a handler
	// that had stopped amending at all.
	if err := svc.Handle(ctx, amendEnvFor("acme"), mustMarshal(t, amendPriced(d(40, 0), nil))); err != nil {
		t.Fatalf("reducing amend: %v", err)
	}
	if status, code := outcomeCode(t, fb); status != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED {
		t.Fatalf("outcome = %v/%q, want EXECUTED for a reduction", status, code)
	}
	reduced, verReduced, err := store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if dec.Cmp(reduced.GetOrderedQuantity(), d(40, 0)) != 0 {
		t.Fatalf("durable ordered quantity = %v, want 40", reduced.GetOrderedQuantity())
	}
	if dec.Cmp(reduced.GetLeavesQuantity(), d(40, 0)) != 0 {
		t.Fatalf("durable leaves quantity = %v, want 40 — an amended size the leaves quantity "+
			"disagrees with is an open quantity nobody can work", reduced.GetLeavesQuantity())
	}
	if verReduced == verBefore {
		t.Fatal("order version did not move for an amend that was applied — the state was " +
			"answered EXECUTED and never committed")
	}
}
