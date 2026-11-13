package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

type reconcileReadStore struct {
	ledger.Store
	writes int
}

func (s *reconcileReadStore) Append(context.Context, *ledger.Event, ledger.Announcer) error {
	s.writes++
	return errors.New("ad-hoc reconciliation must not append")
}

func (s *reconcileReadStore) SaveSnapshot(context.Context, *ledger.Snapshot) error {
	s.writes++
	return errors.New("ad-hoc reconciliation must not save a snapshot")
}

func TestAdHocReconcileReadsTheCurrentScopedBookWithoutWriting(t *testing.T) {
	s, st := newServer(t)
	seedDual(t, st)
	// This input is already known, but does not economically exist yet. The
	// date-free human operation still means right now, not the entire journal.
	future := accountTrade("future", dualPortfolio, "okx-sub-1", "AAPL", -40, 6000)
	future.Effective = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := st.Append(context.Background(), future, nil); err != nil {
		t.Fatal(err)
	}
	readonly := &reconcileReadStore{Store: st}
	s.store = readonly
	rec := reconcileAs(t, s, dualPortfolio, `{"custodian_id":"CUST-A","positions":{"AAPL":"100"},"cash":{"USD":"-15000"}}`)
	if rec.Code != 200 {
		t.Fatalf("date-free current comparison status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeReconcile(t, rec)
	if got.Count != 0 || got.CustodianID != "CUST-A" || got.PortfolioID != dualPortfolio {
		t.Fatalf("future or other-custodian holdings contaminated the current book: %+v", got)
	}
	if readonly.writes != 0 {
		t.Fatalf("ad-hoc read attempted %d ledger writes", readonly.writes)
	}
	journal, err := st.Journal(context.Background(), dualPortfolio)
	if err != nil || len(journal) != 3 {
		t.Fatalf("ad-hoc read changed the source journal: entries=%v err=%v", journal, err)
	}
}
