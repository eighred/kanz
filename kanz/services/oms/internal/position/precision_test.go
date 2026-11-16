package position

import (
	"context"
	"fmt"
	"math/big"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// Expectations use integer units or fixed rational examples, independently of
// the production conversion and weighted-cost helpers.
func assertPositionNumber(t *testing.T, got *commonpb.Decimal, want string) {
	t.Helper()
	if got == nil || got.Exponent < -64 || got.Exponent > 64 {
		t.Fatalf("missing or out-of-domain Decimal: %v", got)
	}
	n := big.NewRat(got.Coefficient, 1)
	e := int64(got.Exponent)
	if e < 0 {
		n.Quo(n, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(-e), nil)))
	} else {
		n.Mul(n, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(e), nil)))
	}
	w, ok := new(big.Rat).SetString(want)
	if !ok || n.Cmp(w) != 0 {
		t.Fatalf("got %s, want %s", n.RatString(), want)
	}
}

func TestPositionPrecisionAccumulatedSignedFills(t *testing.T) {
	for _, side := range []orderpb.Side{orderpb.Side_SIDE_BUY, orderpb.Side_SIDE_SELL} {
		t.Run(side.String(), func(t *testing.T) {
			b := NewBook("USD")
			sign := int64(1)
			if side == orderpb.Side_SIDE_SELL {
				sign = -1
			}
			for i := int64(1); i <= 10; i++ {
				f := fill(fmt.Sprint(i), side, d(1, -9), d(1, -9), i)
				a, err := b.Apply(context.Background(), "p", f, time.Unix(i, 0), nil)
				if err != nil {
					t.Fatal(err)
				}
				for _, st := range []*commonpb.Decimal{a.Venue.Quantity, a.Aggregate.Quantity} {
					assertPositionNumber(t, st, fmt.Sprintf("%d/1000000000", sign*i))
				}
				assertPositionNumber(t, a.Aggregate.MarketValue.Amount, fmt.Sprintf("%d/1000000000000000000", sign*i))
				// Redelivery must announce the same holding without accumulating it.
				if _, err := b.Apply(context.Background(), "p", f, time.Unix(i, 0), nil); err != nil {
					t.Fatal(err)
				}
				snap, err := b.Snapshot(context.Background(), "p", time.Unix(i, 0))
				if err != nil || len(snap.GetPositions()) != 1 {
					t.Fatalf("snapshot: %v", err)
				}
				assertPositionNumber(t, snap.Positions[0].Quantity, fmt.Sprintf("%d/1000000000", sign*i))
				assertPositionNumber(t, snap.Portfolio.TotalMarketValue.Amount, fmt.Sprintf("%d/1000000000000000000", sign*i))
			}
		})
	}
}

func TestPositionPrecisionFiniteMoneyAndDerivedRounding(t *testing.T) {
	for _, source := range []string{"1/1000000000000000000", "-1/1000000000", "21/10", "1/3", "-1/3"} {
		t.Run(source, func(t *testing.T) {
			r, _ := new(big.Rat).SetString(source)
			want := source
			if source == "1/3" {
				want = "33333333/100000000"
			} else if source == "-1/3" {
				want = "-33333333/100000000"
			}
			for _, money := range []func(*big.Rat) (*commonpb.Money, error){NewBook("USD").money, (&Postgres{baseCcy: "USD"}).money} {
				m, err := money(r)
				if err != nil || m.GetCurrencyCode() != "USD" {
					t.Fatalf("money: %v", err)
				}
				assertPositionNumber(t, m.Amount, want)
			}
		})
	}
}

func TestPositionPrecisionPartialClosePnL(t *testing.T) {
	b := NewBook("USD")
	for i, c := range []struct {
		side orderpb.Side
		q, p int64
	}{{orderpb.Side_SIDE_BUY, 2, 1}, {orderpb.Side_SIDE_SELL, 1, 3}} {
		a, err := b.Apply(context.Background(), "p", fill(fmt.Sprint(i), c.side, d(c.q, -9), d(c.p, 0), int64(i+1)), time.Unix(int64(i+1), 0), nil)
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			assertPositionNumber(t, a.Aggregate.Quantity, "1/1000000000")
			assertPositionNumber(t, a.Aggregate.MarketValue.Amount, "3/1000000000")
			assertPositionNumber(t, a.Aggregate.RealizedPnl.Amount, "2/1000000000")
			assertPositionNumber(t, a.Aggregate.UnrealizedPnl.Amount, "2/1000000000")
		}
	}
}

func TestPositionPrecisionRefusesWithoutClaimOrMutation(t *testing.T) {
	b := NewBook("USD")
	first := fill("large", orderpb.Side_SIDE_BUY, d(9223372036854775807, 0), d(1, 0), 1)
	if _, err := b.Apply(context.Background(), "p", first, time.Unix(1, 0), nil); err != nil {
		t.Fatal(err)
	}
	_, err := b.Apply(context.Background(), "p", fill("overflow", orderpb.Side_SIDE_BUY, d(1, 0), d(1, 0), 2), time.Unix(2, 0), nil)
	if err == nil || len(b.appliedFills) != 1 {
		t.Fatal("unrepresentable holding must refuse without a claim")
	}
	snap, err := b.Snapshot(context.Background(), "p", time.Unix(2, 0))
	if err != nil {
		t.Fatal(err)
	}
	assertPositionNumber(t, snap.Positions[0].Quantity, "9223372036854775807")
}

func TestPositionPrecisionFiniteLimitsNeverRound(t *testing.T) {
	for _, source := range []string{"9223372036854775808", "-9223372036854775809", "1/10000000000000000000000000000000000000000000000000000000000000"} {
		r, _ := new(big.Rat).SetString(source)
		for _, money := range []func(*big.Rat) (*commonpb.Money, error){NewBook("USD").money, (&Postgres{baseCcy: "USD"}).money} {
			if m, err := money(r); err == nil || m != nil {
				t.Fatalf("finite %s was rounded or fabricated: %v", source, m)
			}
		}
	}
}

func TestPositionPrecisionRepeatingCostDoesNotDiscardFill(t *testing.T) {
	b := NewBook("USD")
	for i, c := range []struct {
		side orderpb.Side
		q, p int64
	}{{orderpb.Side_SIDE_BUY, 1, 1}, {orderpb.Side_SIDE_BUY, 2, 2}, {orderpb.Side_SIDE_SELL, 1, 2}} {
		a, err := b.Apply(context.Background(), "p", fill(fmt.Sprint(i), c.side, d(c.q, 0), d(c.p, 0), int64(i+1)), time.Unix(int64(i+1), 0), nil)
		if err != nil {
			t.Fatal(err)
		}
		if i == 2 {
			assertPositionNumber(t, a.Aggregate.Quantity, "2")
			assertPositionNumber(t, a.Aggregate.AveragePrice, "166666667/100000000")
			assertPositionNumber(t, a.Aggregate.RealizedPnl.Amount, "33333333/100000000")
			assertPositionNumber(t, a.Aggregate.UnrealizedPnl.Amount, "66666667/100000000")
		}
	}
	if len(b.appliedFills) != 3 {
		t.Fatal("derived rounding lost an execution claim")
	}
}

func TestPositionPrecisionTinyRepeatingSnapshotRefusesWithoutLosingFill(t *testing.T) {
	b := NewBook("USD")
	for i, c := range []struct {
		side orderpb.Side
		q, p int64
	}{{orderpb.Side_SIDE_BUY, 1, 1}, {orderpb.Side_SIDE_BUY, 2, 2}, {orderpb.Side_SIDE_SELL, 1, 2}} {
		a, err := b.Apply(context.Background(), "p", fill(fmt.Sprint(i), c.side, d(c.q, -9), d(c.p, -9), int64(i+1)), time.Unix(int64(i+1), 0), nil)
		if err != nil {
			t.Fatal(err)
		}
		if i == 2 {
			assertPositionNumber(t, a.Aggregate.Quantity, "2/1000000000")
			assertPositionNumber(t, a.Aggregate.MarketValue.Amount, "4/1000000000000000000")
		}
	}
	if len(b.appliedFills) != 3 {
		t.Fatal("derived analytics discarded fill")
	}
	if snap, err := b.Snapshot(context.Background(), "p", time.Unix(3, 0)); err == nil || snap != nil {
		t.Fatal("nonzero repeating cost mark became a successful zero snapshot")
	}
}
