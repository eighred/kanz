package projection

import (
	"context"
	"errors"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	tvsyncpb "github.com/eighred/kanz/kanz-schemas-go/tvsync/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestCheckpointExactDecimalLimitsAndMissing(t *testing.T) {
	for _, d := range []*commonpb.Decimal{nil, {}, {Coefficient: 1, Exponent: -10}, {Coefficient: 1, Exponent: -60}, {Coefficient: 1, Exponent: 60}, {Coefficient: math.MaxInt64, Exponent: -60}, {Coefficient: math.MinInt64, Exponent: 60}} {
		r, err := protoToRat(d)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ratToProto(r)
		if err != nil {
			t.Fatalf("exact conversion: %v", err)
		}
		back, err := protoToRat(got)
		if err != nil {
			t.Fatal(err)
		}
		if r == nil {
			if got != nil {
				t.Fatal("missing became zero")
			}
			continue
		}
		if back == nil || r.Cmp(back) != 0 {
			t.Fatal("exact decimal changed")
		}
	}
}

type precisionCountingLog struct {
	*memLog
	saves int
}

func (l *precisionCountingLog) SaveCheckpoint(ctx context.Context, seq int64, payload []byte) error {
	l.saves++
	return l.memLog.SaveCheckpoint(ctx, seq, payload)
}

func TestCheckpointRefusalPreservesPriorBytes(t *testing.T) {
	for _, text := range []string{"1/3", "1e-61", "1e61", "1e-64", "1e64", "9223372036854775808"} {
		t.Run(text, func(t *testing.T) {
			log := &precisionCountingLog{memLog: newMemLog()}
			p := New(time.Now, nil, WithLog(log, testTenant))
			deliver(t, p, "seed", evtFilled, filledOrder("one", "fund-alpha", "BTC", orderpb.Side_SIDE_BUY, 1, 100))
			if err := p.Checkpoint(context.Background()); err != nil {
				t.Fatal(err)
			}
			before, seq := append([]byte(nil), log.cp...), log.cpSeq
			r, ok := new(big.Rat).SetString(text)
			if !ok {
				t.Fatal("bad test rational")
			}
			for _, field := range []string{"qty", "price", "fee"} {
				a := p.acct(testTenant, "fund-alpha", false)
				old := a.execs[0]
				switch field {
				case "qty":
					a.execs[0].qty = r
				case "price":
					a.execs[0].price = r
				case "fee":
					a.execs[0].fee = r
				}
				if err := p.Checkpoint(context.Background()); !errors.Is(err, ErrCheckpointValueOutOfDomain) {
					t.Fatalf("%s refusal: %v", field, err)
				}
				if log.saves != 1 || string(before) != string(log.cp) || seq != log.cpSeq {
					t.Fatal("refused checkpoint overwrote prior state")
				}
				a.execs[0] = old
			}
		})
	}
}

func TestCheckpointLegacyAndUnknownVersionRefused(t *testing.T) {
	for _, version := range []uint32{0, 2, math.MaxUint32} {
		log := newMemLog()
		source := New(time.Now, nil, WithLog(log, testTenant))
		deliver(t, source, "seed", evtFilled, filledOrder("one", "fund-alpha", "BTC", orderpb.Side_SIDE_BUY, 1, 100))
		if err := source.Checkpoint(context.Background()); err != nil {
			t.Fatal(err)
		}
		var cp tvsyncpb.Checkpoint
		if err := proto.Unmarshal(log.cp, &cp); err != nil {
			t.Fatal(err)
		}
		cp.ExactEncodingVersion = version
		log.cp = mustMarshal(t, &cp)
		original := append([]byte(nil), log.cp...)
		if err := validateReplayCheckpoint(replaySnapshot{checkpoint: log.cp, checkpointSeq: log.cpSeq, watermark: log.cpSeq, at: time.Now()}, testTenant, 7*24*time.Hour); !errors.Is(err, ErrReplayInput) {
			t.Fatalf("legacy/unknown checkpoint qualified: %v", err)
		}
		restored := New(time.Now, nil, WithLog(log, testTenant))
		if err := restored.Rehydrate(context.Background()); !errors.Is(err, ErrCheckpointEncodingVersion) {
			t.Fatalf("version %d: %v", version, err)
		}
		if len(restored.accounts) != 0 || restored.BootedFromCheckpoint() || string(original) != string(log.cp) {
			t.Fatal("legacy refusal changed state or relabeled checkpoint")
		}
	}
}

func TestCheckpointExactRestartAndSubsequentFacts(t *testing.T) {
	exactCheckpointScenario(t, newMemLog())
}

func TestCheckpointExactPostgresRestartAndSubsequentFacts(t *testing.T) {
	pool := newPool(t, testTenant)
	freshSchema(t, pool)
	exactCheckpointScenario(t, NewPostgresLog(pool))
}

func exactCheckpointScenario(t *testing.T, log Log) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	clock := func() time.Time { return now }
	live := New(clock, nil, WithLog(log, testTenant))
	fill := func(id string, side orderpb.Side, qty, price, fee int64) *orderpb.OrderFilled {
		ev := filledOrder(id, "fund-alpha", "BTC", side, 1, 1)
		ev.Fill.Quantity = &commonpb.Decimal{Coefficient: qty, Exponent: -10}
		ev.Fill.Price = &commonpb.Decimal{Coefficient: price, Exponent: -10}
		ev.Fill.Fee = &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: fee, Exponent: -10}, CurrencyCode: "USD"}
		ev.State.OrderedQuantity = proto.Clone(ev.Fill.Quantity).(*commonpb.Decimal)
		ev.State.FilledQuantity = proto.Clone(ev.Fill.Quantity).(*commonpb.Decimal)
		ev.State.AverageFillPrice = proto.Clone(ev.Fill.Price).(*commonpb.Decimal)
		ev.State.AsOf, ev.Fill.ExecutedAt = timestamppb.New(now), timestamppb.New(now)
		return ev
	}
	buy := fill("buy", orderpb.Side_SIDE_BUY, 3, 1000000000001, 1)
	buy.State.Status = orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED
	buy.State.OrderedQuantity = &commonpb.Decimal{Coefficient: 4, Exponent: -10}
	buy.State.LeavesQuantity = &commonpb.Decimal{Coefficient: 1, Exponent: -10}
	deliver(t, live, "buy", evtPartiallyFilled, &orderpb.OrderPartiallyFilled{OrderId: buy.OrderId, State: buy.State, Fill: buy.Fill})
	if err := live.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	restored := New(clock, nil, WithLog(log, testTenant))
	if err := restored.Rehydrate(ctx); err != nil {
		t.Fatal(err)
	}
	if !restored.BootedFromCheckpoint() {
		t.Fatal("did not test checkpoint restart")
	}
	now = now.Add(time.Second)
	deliver(t, restored, "sell", evtFilled, fill("sell", orderpb.Side_SIDE_SELL, 1, 1200000000002, 2))
	deliver(t, restored, "cancel", evtCancelled, &orderpb.OrderCancelled{OrderId: "buy", CancelledQuantity: &commonpb.Decimal{Coefficient: 1, Exponent: -10}})
	full := New(clock, nil, WithLog(log, testTenant))
	if err := full.replayEverythingForTest(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := compareReplayExact(full, restored); err != nil {
		t.Fatal(err)
	}
	assertSameView(t, full, restored)
	a := restored.acct(testTenant, "fund-alpha", false)
	// Independent rational oracle: remaining 2e-10, average100.0000000001;
	// realized=(120.0000000002-100.0000000001)*1e-10 -3e-10 fees.
	wantQty := big.NewRat(2, 10000000000)
	wantAvg := big.NewRat(1000000000001, 10000000000)
	wantRealized := new(big.Rat).Sub(new(big.Rat).Mul(big.NewRat(200000000001, 10000000000), big.NewRat(1, 10000000000)), big.NewRat(3, 10000000000))
	lot := a.live["BTC"]
	if lot.Qty.Cmp(wantQty) != 0 || lot.AvgCost.Cmp(wantAvg) != 0 || lot.Realized.Cmp(wantRealized) != 0 {
		t.Fatalf("independent arithmetic mismatch: %s %s %s", lot.Qty, lot.AvgCost, lot.Realized)
	}
	for _, e := range a.execs {
		for _, r := range []*big.Rat{e.qty, e.price, e.fee} {
			if _, ok := dec.ToProtoExact(r); !ok {
				t.Fatal("fixture not representable")
			}
		}
	}
	if err := restored.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	twice := New(clock, nil, WithLog(log, testTenant))
	if err := twice.Rehydrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := compareReplayExact(full, twice); err != nil {
		t.Fatal(err)
	}
}
