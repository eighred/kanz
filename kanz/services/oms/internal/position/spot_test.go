package position

import (
	"context"
	"errors"
	"math/big"
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

func spotFill(id, venue string, side orderpb.Side, q, price, fee *commonpb.Decimal, ccy string, sec int64) *orderpb.Fill {
	f := fill(id, side, q, price, sec)
	f.OrderId = "order-" + id
	f.Venue = venue
	f.VenueAccountId = "account-" + venue
	f.VenueExecutionId = id
	f.InstrumentId = "BTC-USDT"
	f.SpotVenueSymbol = "BTC-USDT"
	if venue == "BINANCE" {
		f.SpotVenueSymbol = "BTCUSDT"
	}
	f.Fee = &commonpb.Money{Amount: fee, CurrencyCode: ccy}
	return f
}

func TestUnclassifiedSpotLotsReplayBeforeRiskAndRejectGrossWriter(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t, "__system__")
	freshSchema(t, pool)
	st := NewPostgres(pool, "USDT")
	one := spotFill("a", "BINANCE", orderpb.Side_SIDE_BUY, d(1, 0), d(50000, 0), d(25, 0), "USDT", 10)
	two := spotFill("b", "OKX", orderpb.Side_SIDE_BUY, d(1, 0), d(50000, 0), d(8, -4), "BTC", 20)
	for _, f := range []*orderpb.Fill{one, two} {
		key := fillfact.ExecutionKey(f)
		data, err := proto.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO position_fills(fill_id,raw_fill_id) VALUES($1,$2)`, []any{key, f.FillId}},
			{`INSERT INTO position_execution_history(execution_key,raw_fill_id,portfolio_id,venue,instrument_id,execution_time,time_proven,fill) VALUES($1,$2,'old',$3,'BTC-USDT',$4,true,$5)`, []any{key, f.FillId, f.Venue, f.ExecutedAt.AsTime(), data}},
			{`INSERT INTO position_execution_basis(portfolio_id,venue,instrument_id,complete,last_execution_time,last_execution_key) VALUES('old',$1,'BTC-USDT',true,$2,$3)`, []any{f.Venue, f.ExecutedAt.AsTime(), key}},
			{`INSERT INTO positions(portfolio_id,venue,instrument_id,quantity,average_price,realized_pnl) VALUES('old',$1,'BTC-USDT','1','50000','0')`, []any{f.Venue}},
		} {
			if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := st.Snapshot(ctx, "old", time.Now()); !errors.Is(err, ErrPositionHistoryUnknown) {
		t.Fatalf("gross legacy snapshot: %v", err)
	}
	failure := errors.New("no outbox")
	if _, err := st.Apply(ctx, "old", two, time.Now(), func(context.Context, *Applied) ([]outbox.Record, error) { return nil, failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if _, err := st.Snapshot(ctx, "old", time.Now()); !errors.Is(err, ErrPositionHistoryUnknown) {
		t.Fatal("partial reclassification", err)
	}
	got, err := st.Apply(ctx, "old", two, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Cmp(got.Aggregate.Quantity, d(19992, -4)) != 0 || len(got.RestatedVenues) != 1 || got.RestatedVenues[0].Venue != "BINANCE" {
		t.Fatalf("incomplete restatement %+v", got)
	}
	snap, err := NewPostgres(pool, "USDT").Snapshot(ctx, "old", time.Now())
	if err != nil || len(snap.GetPositions()) != 1 || dec.Cmp(snap.Positions[0].Quantity, d(19992, -4)) != 0 {
		t.Fatalf("reopened %v %v", snap, err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE positions SET quantity='1' WHERE portfolio_id='old' AND venue='OKX'`, nil},
		{`INSERT INTO position_fills(fill_id,raw_fill_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, []any{fillfact.ExecutionKey(two), two.FillId}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err == nil || !strings.Contains(err.Error(), "native fee position writer") {
			t.Fatalf("old writer: %v", err)
		}
	}
}

func TestNativeSpotInventoryAndFeeRevision(t *testing.T) {
	for _, backend := range []string{"memory", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			var st Store = NewBook("USD")
			restart := func() {}
			if backend == "postgres" {
				pool := newPool(t, "__system__")
				freshSchema(t, pool)
				restart = func() { st = NewPostgres(pool, "USD") }
				restart()
			}
			post := func(f *orderpb.Fill) *Applied {
				t.Helper()
				res, e := st.Apply(ctx, "fund", f, f.ExecutedAt.AsTime(), nil)
				if e != nil {
					t.Fatal(e)
				}
				return res
			}
			check := func(label string, a *commonpb.Decimal, w string) {
				t.Helper()
				want, _ := new(big.Rat).SetString(w)
				if a == nil || dec.FromProto(a).Cmp(want) != 0 {
					t.Fatalf("%s: %v want %s", label, a, w)
				}
			}
			open := spotFill("open", "BINANCE", orderpb.Side_SIDE_BUY, d(1, 0), d(50000, 0), d(0, 0), "USDT", 10)
			buy := spotFill("buy", "OKX", orderpb.Side_SIDE_BUY, d(1, 0), d(50000, 0), d(8, -4), "BTC", 20)
			sell := spotFill("sell", "BINANCE", orderpb.Side_SIDE_SELL, d(9992, -4), d(50030, 0), d(25, 0), "USDT", 30)
			post(open)
			got := post(buy)
			check("OKX credited", got.Venue.Quantity, "0.9992")
			if got.Aggregate.MarketValue.CurrencyCode != "USDT" || got.Venue.RealizedPnl.CurrencyCode != "USDT" {
				t.Fatal("native quote lost")
			}
			got = post(sell)
			check("Binance remaining", got.Venue.Quantity, "0.0008")
			check("fund", got.Aggregate.Quantity, "1")
			check("sale gross P&L", got.Venue.RealizedPnl.Amount, "29.976")
			if _, err := st.Snapshot(ctx, "fund", time.Now()); err == nil {
				t.Fatal("uncalibrated USDT/USD conversion")
			}
			restart()
			check("redelivery and restart", post(buy).Aggregate.Quantity, "1")
			digest, err := fillfact.EconomicDigest(buy)
			if err != nil {
				t.Fatal(err)
			}
			revised := proto.Clone(buy).(*orderpb.Fill)
			revised.Fee.Amount = d(-2, -4)
			revised.Recovery = &orderpb.ExecutionRecoveryProvenance{CaseId: "fee", FeeApproval: &orderpb.ExecutionFeeApproval{ProposalId: "fee", Digest: strings.Repeat("b", 64), PreviousExecutionDigest: digest, PreviousFee: proto.Clone(buy.Fee).(*commonpb.Money), Proposer: "maker", Approver: "checker", ApprovedAt: timestamppb.New(time.Unix(40, 0))}}
			refuse := errors.New("outbox unavailable")
			if _, err := st.Apply(ctx, "fund", revised, time.Now(), func(context.Context, *Applied) ([]outbox.Record, error) { return nil, refuse }); !errors.Is(err, refuse) {
				t.Fatal(err)
			}
			check("rolled back", post(buy).Aggregate.Quantity, "1")
			check("authorized rebate", post(revised).Aggregate.Quantity, "1.001")
			restart()
			check("original after revision", post(buy).Aggregate.Quantity, "1.001")
			check("repeated approval", post(revised).Aggregate.Quantity, "1.001")
			prior := spotFill("older", "OKX", orderpb.Side_SIDE_SELL, d(1, -1), d(55000, 0), d(1, -4), "BTC", 15)
			got = post(prior)
			check("backdated sell and current fee head", got.Venue.Quantity, "0.9001")
			check("updated fund", got.Aggregate.Quantity, "0.9009")
			check("fund mark from the most recent venue", got.Aggregate.MarketValue.Amount, "45072.027")
			if !got.Aggregate.AsOf.AsTime().Equal(sell.ExecutedAt.AsTime()) {
				t.Fatal(got.Aggregate.AsOf)
			}
			check("realized after backdated close", got.Venue.RealizedPnl.Amount, "500.5")
			if !got.Venue.AsOf.AsTime().Equal(buy.ExecutedAt.AsTime()) {
				t.Fatal("backdated fill moved mark backward")
			}
		})
	}
}
