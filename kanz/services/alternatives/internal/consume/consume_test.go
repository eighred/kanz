package consume

import (
	"context"
	"encoding/json"
	"math/big"
	"testing"
	"time"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	"github.com/kanz-eng/kanz/internal/alternatives"
	"github.com/kanz-eng/kanz/services/alternatives/internal/fund"
)

func payload(t *testing.T, e alternatives.Event) []byte {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestFolderFoldsLifecycleIntoFund(t *testing.T) {
	st := fund.NewMemoryStore()
	f, err := NewFolder(st, nil)
	if err != nil {
		t.Fatalf("new folder: %v", err)
	}
	ctx := context.Background()
	t0 := time.Unix(1_700_000_000, 0).UTC()
	env := &envelopepb.Envelope{EventType: "alternatives.commitment.called"}

	events := []alternatives.Event{
		{EventID: "c1", CommitmentID: "CMT-1", Type: alternatives.EventCommit, Amount: big.NewRat(1_000_000, 1), Date: t0},
		{EventID: "k1", CommitmentID: "CMT-1", Type: alternatives.EventCall, Amount: big.NewRat(250_000, 1), Date: t0.Add(24 * time.Hour)},
	}
	for _, e := range events {
		if err := f.Handle(ctx, env, payload(t, e)); err != nil {
			t.Fatalf("handle %s: %v", e.EventID, err)
		}
	}
	// Redelivery is idempotent.
	if err := f.Handle(ctx, env, payload(t, events[0])); err != nil {
		t.Fatalf("redelivery: %v", err)
	}

	pos, err := fund.Materialize(ctx, st, "CMT-1")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if pos.Committed.Cmp(big.NewRat(1_000_000, 1)) != 0 || pos.Called.Cmp(big.NewRat(250_000, 1)) != 0 {
		t.Fatalf("position committed=%v called=%v", pos.Committed, pos.Called)
	}
}

func TestFolderRejectsMissingEventID(t *testing.T) {
	st := fund.NewMemoryStore()
	f, _ := NewFolder(st, nil)
	env := &envelopepb.Envelope{EventType: "alternatives.commitment.called"}
	if err := f.Handle(context.Background(), env, payload(t, alternatives.Event{CommitmentID: "CMT-1"})); err == nil {
		t.Fatal("missing event_id should surface an error")
	}
}

func TestFolderRejectsMalformed(t *testing.T) {
	st := fund.NewMemoryStore()
	f, _ := NewFolder(st, nil)
	env := &envelopepb.Envelope{EventType: "alternatives.commitment.called"}
	if err := f.Handle(context.Background(), env, []byte("not-json")); err == nil {
		t.Fatal("malformed payload should surface an error (DLQ)")
	}
}
