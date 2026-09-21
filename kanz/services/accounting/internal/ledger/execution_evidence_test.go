package ledger

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"testing"

	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/internal/outbox"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func executionEvidenceFixture(account string) *orderpb.Fill {
	return &orderpb.Fill{FillId: "BTCUSDT-7", OrderId: "order-" + account, Venue: "BINANCE", VenueAccountId: account, VenueExecutionId: "7", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
		Quantity: d(1, 0), Price: d(100, 0), Fee: &commonpb.Money{Amount: d(1, 0), CurrencyCode: "USD"}, ExecutedAt: timestamppb.New(day(1))}
}

func TestExecutionEvidenceSeparatesAccountsAndRefusesFeeOverwrite(t *testing.T) {
	for _, backend := range []string{"memory", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var store Store = NewMemoryStore()
			if backend == "postgres" {
				store = NewPostgres(newPool(t))
			}
			ctx := context.Background()
			var entries []*Event
			for _, account := range []string{"account-a", "account-b"} {
				e, err := FromFill("fund", executionEvidenceFixture(account), "USD", day(2))
				if err != nil {
					t.Fatal(err)
				}
				entries = append(entries, e)
			}
			var wg sync.WaitGroup
			failures := make(chan error, 4)
			for _, e := range entries {
				for range 2 {
					wg.Go(func() { failures <- store.Append(ctx, e, nil) })
				}
			}
			wg.Wait()
			close(failures)
			for err := range failures {
				if err != nil {
					t.Fatal(err)
				}
			}
			journal, err := store.Journal(ctx, "fund")
			if err != nil || len(journal) != 2 {
				t.Fatalf("journal=%v err=%v", journal, err)
			}
			book := Replay("fund", journal)
			if book.Cash["USD"].Cmp(big.NewRat(-202, 1)) != 0 || book.Positions["BTC-USD"].Qty.Cmp(big.NewRat(2, 1)) != 0 {
				t.Fatalf("economic effect: %+v", book)
			}
			for _, e := range journal {
				var fill orderpb.Fill
				if err := proto.Unmarshal(e.ExecutionEvidence, &fill); err != nil || fill.GetVenueExecutionId() != "7" || fill.GetVenueAccountId() != e.VenueAccountID {
					t.Fatalf("provenance lost: %v %v", &fill, err)
				}
			}
			changed := executionEvidenceFixture("account-a")
			changed.Fee.Amount = d(2, 0)
			e, err := FromFill("fund", changed, "USD", day(3))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Append(ctx, e, nil); !errors.Is(err, fillfact.ErrExecutionIdentityConflict) {
				t.Fatalf("fee change silently deduplicated: %v", err)
			}
		})
	}
}

func TestExecutionEvidenceAndClaimRollBackWithAnnouncement(t *testing.T) {
	for _, backend := range []string{"memory", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var store Store = NewMemoryStore()
			if backend == "postgres" {
				store = NewPostgres(newPool(t))
			}
			ctx := context.Background()
			e, err := FromFill("fund", executionEvidenceFixture("account"), "USD", day(2))
			if err != nil {
				t.Fatal(err)
			}
			refusal := errors.New("announcement refused")
			if err := store.Append(ctx, e, func(context.Context, Store) ([]outbox.Record, error) { return nil, refusal }); !errors.Is(err, refusal) {
				t.Fatal(err)
			}
			journal, err := store.Journal(ctx, "fund")
			if err != nil || len(journal) != 0 {
				t.Fatalf("failed announcement committed execution: %v %v", journal, err)
			}
			if err := store.Append(ctx, e, nil); err != nil {
				t.Fatal(err)
			}
			journal, err = store.Journal(ctx, "fund")
			if err != nil || len(journal) != 1 {
				t.Fatalf("retry lost execution: %v %v", journal, err)
			}
		})
	}
}
