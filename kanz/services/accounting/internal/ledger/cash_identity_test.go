package ledger

import (
	"errors"
	"math/big"
	"testing"
	"time"
)

func TestMemoryCashEntryRejectsChangedRetryAndOwnsAmount(t *testing.T) {
	store := NewMemoryStore()
	entry := &Event{EntryID: "cash:transfer", PortfolioID: "PF", Type: EntryCash, Cash: big.NewRat(1, 1000000000), CashCurrency: "USD", Effective: time.Now(), Knowledge: time.Now(), SourceRef: "evidence"}
	if err := store.Append(t.Context(), entry, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(t.Context(), entry, nil); err != nil {
		t.Fatal(err)
	}
	entry.Cash.SetInt64(1)
	if err := store.Append(t.Context(), entry, nil); !errors.Is(err, ErrCashConflict) {
		t.Fatalf("changed retry: %v", err)
	}
	entries, err := store.Journal(t.Context(), "PF")
	if err != nil || len(entries) != 1 || entries[0].Cash.Cmp(big.NewRat(1, 1000000000)) != 0 {
		t.Fatalf("mutated accepted entry: %v %v", entries, err)
	}
}
