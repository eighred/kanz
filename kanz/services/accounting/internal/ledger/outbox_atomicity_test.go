package ledger

import (
	"context"
	"errors"
	"testing"
	"time"

	"math/big"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"

	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/pkg/bus"
)

// THE ENTRY AND ITS ANNOUNCEMENT COMMIT TOGETHER, AGAINST A REAL POSTGRES (#804, #292).
//
// On the MemoryStore "the entry landed" and "the announcement landed" are the
// same fact by construction — one lock, two map writes. The durable path is
// where they can come apart, and it is the only place the transaction is real.
//
// Gated on TEST_POSTGRES_URL; skipping is not passing.

// announceOne is a well-formed cash record for the portfolio being appended.
func announceOne(t *testing.T, portfolioID string) Announcer {
	t.Helper()
	return func(ctx context.Context, _ Store) ([]outbox.Record, error) {
		rec, err := outbox.From(ctx, bus.Event{
			Subject:          "accounting.balance.portfolio",
			EventType:        "accounting.balance.portfolio",
			SchemaVersion:    1,
			Domain:           "accounting",
			EventTime:        time.Unix(1_700_000_000, 0).UTC(),
			PartitionKey:     portfolioID,
			PayloadSchemaRef: "accounting.v1.PortfolioCashBalance:1",
			Payload:          &accountingpb.PortfolioCashBalance{PortfolioId: portfolioID},
		})
		return []outbox.Record{rec}, err
	}
}

// A FAILED ANNOUNCEMENT ROLLS THE ENTRY BACK.
//
// An entry whose level cannot be stated is one the estate would be told nothing
// about. Refusing it leaves the journal consistent and the fill in the DLQ,
// where an operator sees it — rather than a committed entry whose announcement
// silently never goes out, which is the shape #804 exists to end.
func TestAnEntryWhoseAnnouncementCannotBeBuiltDoesNotCommit(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	ctx := bus.WithTenantID(context.Background(), "acme")

	boom := errors.New("the level is not representable")
	err := st.Append(ctx, cashOne("c:rollback", "PF-804-a", 100),
		func(context.Context, Store) ([]outbox.Record, error) { return nil, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("Append = %v, want the announcer's error", err)
	}

	entries, err := st.Journal(ctx, "PF-804-a")
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("the journal holds %d entry(ies) for a fold whose announcement could not be "+
			"built — the entry committed and nothing will ever announce the level it produced",
			len(entries))
	}
}

// AND THE ORDINARY PATH COMMITS BOTH.
func TestAnEntryAndItsAnnouncementLandTogether(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	ctx := bus.WithTenantID(context.Background(), "acme")

	if err := st.Append(ctx, cashOne("c:together", "PF-804-b", 250), announceOne(t, "PF-804-b")); err != nil {
		t.Fatalf("append: %v", err)
	}

	entries, err := st.Journal(ctx, "PF-804-b")
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("journal holds %d entries, want 1", len(entries))
	}

	// The record is in the queue and unpublished — durable, and waiting for the
	// relay. That is the whole property: the announcement outlives the process
	// that folded the entry.
	q := st.Outbox()
	pending, err := q.Pending(ctx, "PF-804-b", 10)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("the outbox holds %d record(s) for this portfolio, want 1 — the entry committed "+
			"and its announcement did not, which is the two-independent-writes shape #804 removed",
			len(pending))
	}
	if pending[0].Record.PartitionKey != "PF-804-b" {
		t.Errorf("partition key = %q, want the portfolio — the relay orders one key at a time and "+
			"the portfolio is the granularity a consumer folds at", pending[0].Record.PartitionKey)
	}
}

// cashOne is a minimal cash entry.
func cashOne(id, portfolio string, amount int64) *Event {
	at := time.Unix(1_700_000_000, 0).UTC()
	return &Event{
		EntryID: id, PortfolioID: portfolio, VenueAccountID: "",
		Type: EntryCash, Cash: big.NewRat(amount, 1), CashCurrency: "USD",
		Effective: at, Knowledge: at,
	}
}
