package consume

import (
	"context"
	"encoding/json"
	"testing"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/wealth"
	"github.com/eighred/kanz/services/wealth/internal/book"
)

// testTenant is the tenant these folders serve. It is the SYSTEM tenant, so
// RequireTenantScope's shared-bucket branch applies and these tests exercise the
// folding logic exactly as they did before #223. The cross-tenant refusal itself
// is proven in cross_tenant_test.go, which uses a real tenant.
const testTenant = "__system__"

func payload(t *testing.T, h wealth.Household) []byte {
	t.Helper()
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestFolderFoldsCompositionIntoBook(t *testing.T) {
	st := book.NewMemoryStore()
	f, err := NewFolder(testTenant, st, nil)
	if err != nil {
		t.Fatalf("new folder: %v", err)
	}
	ctx := context.Background()
	env := &envelopepb.Envelope{EventType: "wealth.household.updated"}

	h := wealth.Household{
		HouseholdID: "HH-1",
		Accounts: []wealth.Account{
			{AccountID: "A-1", Cash: 5000, Holdings: []wealth.Holding{
				{InstrumentID: "VTI", AssetClass: "EQUITY", MarketValue: 95_000},
			}},
		},
	}
	if err := f.Handle(ctx, env, payload(t, h)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	// Last-write-wins replace.
	h.Accounts[0].Cash = 8000
	if err := f.Handle(ctx, env, payload(t, h)); err != nil {
		t.Fatalf("replace: %v", err)
	}

	got, ok, err := st.Get(ctx, "HH-1")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Accounts[0].Cash != 8000 {
		t.Fatalf("cash = %v, want 8000 (last write wins)", got.Accounts[0].Cash)
	}

	// The live composition aggregates correctly.
	vp := wealth.Aggregate(got)
	if vp.Holdings["VTI"] != 95_000 || vp.Cash != 8000 {
		t.Fatalf("aggregate over live composition: %+v", vp)
	}
}

func TestFolderRejectsMissingHouseholdID(t *testing.T) {
	st := book.NewMemoryStore()
	f, _ := NewFolder(testTenant, st, nil)
	env := &envelopepb.Envelope{EventType: "wealth.household.updated"}
	if err := f.Handle(context.Background(), env, payload(t, wealth.Household{})); err == nil {
		t.Fatal("missing household_id should surface an error")
	}
}
