package custody

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

type assignmentProbe func(context.Context, Subject, map[string][]string) ([]AccountAssignment, error)

func (f assignmentProbe) ProvenPortfolioAssignments(ctx context.Context, sub Subject, accounts map[string][]string) ([]AccountAssignment, error) {
	return f(ctx, sub, accounts)
}

func TestAdmittedBookUsesDatedIndependentVersionsForAllDeclaredAccounts(t *testing.T) {
	ctx := bus.WithTenantID(t.Context(), "test-tenant")
	boot := BusinessDay(t0).AddDate(0, 0, 3)
	oldDay := BusinessDay(t0)
	subject := Subject{PortfolioID: "PF1", CustodianID: "CUST-A", BusinessDate: oldDay}
	var observed []time.Time
	refuse := false
	proofs := []AccountAssignment{{CustodianID: "CUST-A", AccountID: "okx-sub-1", Version: "A-1"}, {CustodianID: "CUST-B", AccountID: "bin-main", Version: "B-2"}}
	reader := assignmentProbe(func(ctx context.Context, sub Subject, accounts map[string][]string) ([]AccountAssignment, error) {
		if bus.TenantIDFromContext(ctx) != "test-tenant" || sub.PortfolioID != "PF1" || sub.CustodianID != "CUST-A" ||
			!reflect.DeepEqual(accounts, map[string][]string{"CUST-A": {"okx-sub-1"}, "CUST-B": {"bin-main"}}) {
			t.Fatalf("assignment lookup did not receive authenticated tenant and complete declared scope: %+v, %+v", sub, accounts)
		}
		observed = append(observed, sub.BusinessDate)
		if refuse {
			return nil, errors.New("private reader error")
		}
		return proofs, nil
	})
	raw := twoCustodianScope(t)
	if scope, err := raw.AdmitAssignments(ctx, boot, nil); scope != nil || !errors.Is(err, ErrUnprovenAssignment) {
		t.Fatalf("unverified declaration admitted at startup: scope=%v err=%v", scope, err)
	}
	scope, err := raw.AdmitAssignments(ctx, boot, reader)
	if err != nil {
		t.Fatal(err)
	}
	st := ledger.NewMemoryStore()
	load := LedgerBookLoader(st, scope, testLogger())
	book, _, basis, err := load(ctx, subject)
	if err != nil || book == nil || basis == nil || !reflect.DeepEqual(basis.Assignments, proofs) || !reflect.DeepEqual(observed, []time.Time{boot, oldDay}) {
		t.Fatalf("dated book proof without prior trades: basis=%+v dates=%v err=%v", basis, observed, err)
	}
	refuse = true
	if book, _, basis, err := load(ctx, subject); book != nil || basis != nil || !errors.Is(err, ErrUnprovenAssignment) || strings.Contains(err.Error(), "private reader error") {
		t.Fatalf("runtime accepted old boot proof: %+v %+v %v", book, basis, err)
	}
	store := NewMemoryStore()
	if err := store.SaveStatement(ctx, Statement{PortfolioID: subject.PortfolioID, CustodianID: subject.CustodianID, BusinessDate: oldDay, StatementID: "source-row", ReceivedAt: oldDay}); err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time { return oldDay.AddDate(0, 0, 1) }
	r, err := NewReconciler(store, load, nil, nil, nil, testLogger(), clock)
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewScheduler(r, SchedulerConfig{Pairs: []Subject{subject}, Interval: time.Minute, LagDays: 1}, testLogger(), clock)
	if err != nil {
		t.Fatal(err)
	}
	scheduler.Tick(ctx)
	run, err := store.LatestRun(ctx, subject.PortfolioID, subject.CustodianID)
	if err != nil || run.Outcome != OutcomeFailed || run.Basis != nil || !run.Subject.BusinessDate.Equal(oldDay) || len(run.Breaks) != 0 {
		t.Fatalf("scheduled comparison with unavailable approval: %+v %v", run, err)
	}
}
