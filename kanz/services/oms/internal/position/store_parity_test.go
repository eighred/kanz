package position

// #818 — THE TWO position.Store BACKENDS MUST AGREE ABOUT A REDELIVERED FILL.
//
// Postgres.Apply claims position_fills before it folds; Book.Apply folded
// unconditionally. Both satisfy position.Store, whose Apply contract says in so
// many words that a fill "already folded — by a redelivery, or by another pod —
// is not counted again".
//
// Book is selected only when OMS_DATABASE_URL is absent, which already mandates a
// single replica, so the divergence is not itself a production double-count. What
// it broke is the CERTIFICATION: every service-level test of the projector runs
// against Book, so a redelivery test written to prove exactly-once folding passed
// against a store that doubled the position. That certification is what stands
// between a redelivery bug and a double-counted book.
//
// The Postgres arm is not decoration. It is the arm that establishes what the
// contract MEANS, and it is the arm a fake cannot supply — the whole defect is
// that the fake answered a question only the durable store could answer.

import (
	"context"
	"math/big"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/outbox"
)

// bothStores returns the two Store implementations under one scenario. The
// Postgres arm needs TEST_POSTGRES_URL; newPool skips the whole test without it,
// which is deliberate — a run that silently exercised only Book would be the
// defect certifying itself again.
func bothStores(t *testing.T) map[string]Store {
	t.Helper()
	pool := newPool(t, "__system__")
	freshSchema(t, pool)
	return map[string]Store{
		"Postgres": NewPostgres(pool, "USD"),
		"Book":     NewBook("USD"),
	}
}

// TestARedeliveredFillIsFoldedOnceByEveryStore is the issue's "Verified when":
// applying the same fill twice leaves the aggregate byte-identical.
func TestARedeliveredFillIsFoldedOnceByEveryStore(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()

	for name, store := range bothStores(t) {
		t.Run(name, func(t *testing.T) {
			f := buy("redelivered-1", "BTC-USD", "1", "50000", now)

			first, err := store.Apply(ctx, "fund-alpha", f, now, nil)
			if err != nil {
				t.Fatalf("first delivery: %v", err)
			}
			second, err := store.Apply(ctx, "fund-alpha", f, now, nil)
			if err != nil {
				t.Fatalf("redelivery: %v", err)
			}

			// The number the risk engine, the compliance monitor and the OMS's own
			// pre-trade gate consume.
			if got := dec.FromProto(second.Aggregate.GetQuantity()); got.Cmp(big.NewRat(1, 1)) != 0 {
				t.Errorf("aggregate quantity after a REDELIVERY of one fill = %s, want 1 — the fund bought 1 BTC and the book says it holds more", got.RatString())
			}
			if got := dec.FromProto(second.Venue.GetQuantity()); got.Cmp(big.NewRat(1, 1)) != 0 {
				t.Errorf("per-venue quantity after a REDELIVERY of one fill = %s, want 1 — a CLOSE would try to flatten a holding that was never opened", got.RatString())
			}
			// Byte-identical, which is stronger than "the quantity matches": a fold
			// that moved average price or realized P&L while leaving quantity alone
			// would still corrupt the book.
			if !proto.Equal(first.Aggregate, second.Aggregate) {
				t.Errorf("aggregate changed on redelivery:\n  first  = %v\n  second = %v", first.Aggregate, second.Aggregate)
			}
			if !proto.Equal(first.Venue, second.Venue) {
				t.Errorf("per-venue state changed on redelivery:\n  first  = %v\n  second = %v", first.Venue, second.Venue)
			}
		})
	}
}

// TestARedeliveredFillStillAnnouncesInEveryStore pins the OTHER half of parity,
// and it is the half a naive early-return breaks.
//
// Postgres skips the FOLD on a duplicate claim and still builds both projections
// and still calls announce — the redelivery is being ACKED, and the current state
// of the book is a true FACT worth republishing on a compacted subject. A Book
// that returned early instead would swap one divergence for another: the
// projector would enqueue nothing, and the seam would once again certify
// behaviour the durable store does not have.
func TestARedeliveredFillStillAnnouncesInEveryStore(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()

	for name, store := range bothStores(t) {
		t.Run(name, func(t *testing.T) {
			calls := 0
			announce := func(context.Context, *Applied) ([]outbox.Record, error) {
				calls++
				return nil, nil
			}
			f := buy("redelivered-2", "BTC-USD", "1", "50000", now)
			if _, err := store.Apply(ctx, "fund-alpha", f, now, announce); err != nil {
				t.Fatalf("first delivery: %v", err)
			}
			if _, err := store.Apply(ctx, "fund-alpha", f, now, announce); err != nil {
				t.Fatalf("redelivery: %v", err)
			}
			if calls != 2 {
				t.Errorf("announce ran %d time(s) across two deliveries, want 2 — a store that stops announcing on a duplicate has replaced one divergence with another", calls)
			}
		})
	}
}
