package position

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/internal/outbox"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

func TestMemoryExecutionHistoryCapacityRefusesWithoutEvictingEvidence(t *testing.T) {
	b := NewBook("USD")
	ctx := context.Background()
	start := time.Unix(1700000000, 0)
	for i := range maxPositionReplay {
		f := buy(fmt.Sprint(i), "BTC-USD", "1", "100", start.Add(time.Duration(i)*time.Second))
		if _, err := b.Apply(ctx, "fund", f, f.ExecutedAt.AsTime(), nil); err != nil {
			t.Fatal(err)
		}
	}
	extra := buy("overflow", "BTC-USD", "1", "100", start.Add(time.Duration(maxPositionReplay)*time.Second))
	if _, err := b.Apply(ctx, "fund", extra, extra.ExecutedAt.AsTime(), nil); !errors.Is(err, ErrPositionHistoryUnknown) {
		t.Fatalf("capacity refusal: %v", err)
	}
	first := buy("0", "BTC-USD", "1", "100", start)
	got, err := b.Apply(ctx, "fund", first, start, nil)
	if err != nil || dec.FromProto(got.Venue.Quantity).Cmp(big.NewRat(maxPositionReplay, 1)) != 0 {
		t.Fatalf("capacity evicted evidence or duplicated economics: %v %v", got, err)
	}
	count := 0
	for _, h := range b.histories {
		count += len(h)
	}
	if count != maxPositionReplay || len(b.appliedFills) != maxPositionReplay {
		t.Fatalf("histories=%d claims=%d", count, len(b.appliedFills))
	}
}

func TestMemoryExecutionHistoryRollsBackWithFailedAnnouncement(t *testing.T) {
	b := NewBook("USD")
	ctx := context.Background()
	start := time.Unix(1700000000, 0)
	first := buy("first", "BTC-USD", "1", "100", start)
	if _, err := b.Apply(ctx, "fund", first, start, nil); err != nil {
		t.Fatal(err)
	}
	last := fillAt("last", "BTC-USD", "1", "150", orderpb.Side_SIDE_SELL, start.Add(2*time.Second))
	last.Venue = first.Venue
	if _, err := b.Apply(ctx, "fund", last, last.ExecutedAt.AsTime(), nil); err != nil {
		t.Fatal(err)
	}
	late := buy("late", "BTC-USD", "1", "200", start.Add(time.Second))
	late.Recovery = &orderpb.ExecutionRecoveryProvenance{CaseId: "case"}
	refused := errors.New("announcement refused")
	if _, err := b.Apply(ctx, "fund", late, late.ExecutedAt.AsTime(), func(context.Context, *Applied) ([]outbox.Record, error) { return nil, refused }); !errors.Is(err, refused) {
		t.Fatal(err)
	}
	snapshot, err := b.Snapshot(ctx, "fund", start)
	if err != nil {
		t.Fatal(err)
	}
	for _, position := range snapshot.Positions {
		if dec.FromProto(position.Quantity).Sign() != 0 {
			t.Fatal("failed announcement committed position")
		}
	}
	got, err := b.Apply(ctx, "fund", late, late.ExecutedAt.AsTime(), nil)
	if err != nil || dec.FromProto(got.Venue.Quantity).Cmp(big.NewRat(1, 1)) != 0 || dec.FromProto(got.Venue.AveragePrice).Cmp(big.NewRat(150, 1)) != 0 || dec.FromProto(got.Venue.RealizedPnl.Amount).Sign() != 0 {
		t.Fatalf("retry lost fill or replayed arrival order: %v %v", got, err)
	}
}

func TestBackdatedExecutionReplaysCostBasisRatherThanArrivalOrder(t *testing.T) {
	pool := newPool(t, "__system__")
	freshSchema(t, pool)
	ctx := context.Background()
	s := NewPostgres(pool, "USD")
	start := time.Unix(1700000000, 0)
	first := buy("first", "BTC-USD", "1", "100", start)
	last := fillAt("last", "BTC-USD", "1", "150", orderpb.Side_SIDE_SELL, start.Add(2*time.Second))
	last.Venue = first.Venue
	late := buy("late", "BTC-USD", "1", "200", start.Add(time.Second))
	late.Recovery = &orderpb.ExecutionRecoveryProvenance{CaseId: "case"}
	for _, f := range []*orderpb.Fill{first, last} {
		if _, err := s.Apply(ctx, "fund", f, f.ExecutedAt.AsTime(), nil); err != nil {
			t.Fatal(err)
		}
	}
	got, err := NewPostgres(pool, "USD").Apply(ctx, "fund", late, late.ExecutedAt.AsTime(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if dec.FromProto(got.Venue.Quantity).Cmp(big.NewRat(1, 1)) != 0 || dec.FromProto(got.Venue.AveragePrice).Cmp(big.NewRat(150, 1)) != 0 || dec.FromProto(got.Venue.RealizedPnl.Amount).Sign() != 0 {
		t.Fatalf("backdated execution used arrival-order economics: %v", got.Venue)
	}
	again, err := s.Apply(ctx, "fund", late, late.ExecutedAt.AsTime(), nil)
	if err != nil || dec.Cmp(again.Venue.Quantity, got.Venue.Quantity) != 0 {
		t.Fatalf("redelivery: %v %v", again, err)
	}
	if !again.Venue.GetAsOf().AsTime().Equal(last.ExecutedAt.AsTime()) || dec.FromProto(again.Venue.UnrealizedPnl.Amount).Sign() != 0 {
		t.Fatalf("late redelivery replaced latest projection time or mark: %v", again.Venue)
	}
}

func TestPositionExecutionIdentitySeparatesAccountsAndDetectsChangedFees(t *testing.T) {
	pool := newPool(t, "__system__")
	freshSchema(t, pool)
	ctx := context.Background()
	s := NewPostgres(pool, "USD")
	now := time.Unix(1700000000, 0)
	for _, account := range []string{"account-a", "account-b"} {
		f := buy("symbol-7", "BTC-USD", "1", "100", now)
		f.VenueAccountId, f.VenueExecutionId = account, "7"
		if _, err := s.Apply(ctx, "fund", f, now, nil); err != nil {
			t.Fatal(err)
		}
		f.Fee = &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 1}, CurrencyCode: "USD"}
		if _, err := s.Apply(ctx, "fund", f, now, nil); !errors.Is(err, fillfact.ErrExecutionIdentityConflict) {
			t.Fatalf("changed fee silently acknowledged: %v", err)
		}
	}
	snapshot, err := s.Snapshot(ctx, "fund", now)
	if err != nil || len(snapshot.GetPositions()) != 1 || dec.FromProto(snapshot.Positions[0].Quantity).Cmp(big.NewRat(2, 1)) != 0 {
		t.Fatalf("accounts collided: %v %v", snapshot, err)
	}
}

func TestRecoveryDoesNotReplaceUnknownPositionHistoryWithZero(t *testing.T) {
	pool := newPool(t, "__system__")
	freshSchema(t, pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO positions (portfolio_id,venue,instrument_id,quantity,average_price,realized_pnl) VALUES ('fund','XBIN','BTC-USD','1','100','0')`); err != nil {
		t.Fatal(err)
	}
	f := buy("late", "BTC-USD", "1", "200", time.Unix(1700000000, 0))
	f.Recovery = &orderpb.ExecutionRecoveryProvenance{CaseId: "case"}
	if _, err := NewPostgres(pool, "USD").Apply(ctx, "fund", f, f.ExecutedAt.AsTime(), nil); !errors.Is(err, ErrPositionHistoryUnknown) {
		t.Fatalf("unknown prefix guessed away: %v", err)
	}
	var claims, history int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM position_fills),(SELECT count(*) FROM position_execution_history)`).Scan(&claims, &history); err != nil || claims != 0 || history != 0 {
		t.Fatalf("refused recovery committed a prefix: %d %d %v", claims, history, err)
	}
}
