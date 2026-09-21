package position

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/internal/outbox"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestApprovedPositionFeesPreserveQuantityCostAndReplayClaims(t *testing.T) {
	for _, backend := range []string{"memory", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var store interface {
				Apply(context.Context, string, *orderpb.Fill, time.Time, Announcer) (*Applied, error)
			} = NewBook("USD")
			if backend == "postgres" {
				pool := newPool(t, "__system__")
				freshSchema(t, pool)
				store = NewPostgres(pool, "USD")
			}
			ctx := context.Background()
			now := time.Unix(1700000000, 0)
			original := buy("alias", "BTC-USD", "1", "100", now)
			original.OrderId = "order"
			original.VenueAccountId = "account"
			original.VenueExecutionId = "trade"
			original.Fee = &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 1}, CurrencyCode: "USD"}
			baseline, err := store.Apply(ctx, "fund", original, now, nil)
			if err != nil {
				t.Fatal(err)
			}
			revise := func(previous *orderpb.Fill, id string, fee int64) *orderpb.Fill {
				t.Helper()
				digest, err := fillfact.EconomicDigest(previous)
				if err != nil {
					t.Fatal(err)
				}
				v := proto.Clone(previous).(*orderpb.Fill)
				v.Fee.Amount.Coefficient = fee
				v.Recovery = &orderpb.ExecutionRecoveryProvenance{CaseId: id, FeeApproval: &orderpb.ExecutionFeeApproval{ProposalId: id, Digest: strings.Repeat("a", 64), Proposer: "maker", Approver: "checker", ApprovedAt: timestamppb.New(now), PreviousExecutionDigest: digest, PreviousFee: proto.Clone(previous.Fee).(*commonpb.Money)}}
				return v
			}
			first := revise(original, "first", 2)
			second := revise(first, "second", 1)
			refusal := errors.New("ack outbox unavailable")
			if _, err := store.Apply(ctx, "fund", first, now, func(context.Context, *Applied) ([]outbox.Record, error) { return nil, refusal }); !errors.Is(err, refusal) {
				t.Fatal(err)
			}
			for _, f := range []*orderpb.Fill{first, second, first, second, original} {
				got, err := store.Apply(ctx, "fund", f, now, nil)
				if err != nil {
					t.Fatal(err)
				}
				if dec.Cmp(got.Venue.Quantity, baseline.Venue.Quantity) != 0 || dec.Cmp(got.Venue.AveragePrice, baseline.Venue.AveragePrice) != 0 || dec.Cmp(got.Venue.RealizedPnl.Amount, baseline.Venue.RealizedPnl.Amount) != 0 {
					t.Fatal("fee changed position economics")
				}
			}
			if _, err := store.Apply(ctx, "fund", revise(first, "stale", 3), now, nil); !errors.Is(err, fillfact.ErrFeeRevision) {
				t.Fatalf("stale fee baseline: %v", err)
			}
		})
	}
}
