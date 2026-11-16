package translate

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/platform/halt"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	signalpb "github.com/eighred/kanz/kanz-schemas-go/signal/v1"
)

type closePositions func(context.Context, string, string, string) (*big.Rat, error)

func (f closePositions) Position(ctx context.Context, fund, venue, instrument string) (*big.Rat, error) {
	return f(ctx, fund, venue, instrument)
}

func closeTranslator(t *testing.T, source PositionSource) (*Translator, *recorder) {
	t.Helper()
	rec := &recorder{}
	tr, err := New(Options{Prices: StaticPrices{}, Equity: StaticEquity{}, Positions: source,
		Alloc:     StaticAllocation{"fund-alpha": {{Venue: "BINANCE", Weight: big.NewRat(1, 2)}, {Venue: "OKX", Weight: big.NewRat(1, 2)}}},
		Publisher: rec, Gate: halt.OpenGate(nil), Authority: boundAuthority(t)})
	if err != nil {
		t.Fatal(err)
	}
	return tr, rec
}

func TestClosePreflightRefusesEveryLegBeforeEffects(t *testing.T) {
	for _, unknownVenue := range []string{"BINANCE", "OKX"} {
		t.Run(unknownVenue, func(t *testing.T) {
			for _, failure := range []string{"error", "nil"} {
				t.Run(failure, func(t *testing.T) {
					unknown := errors.New("fixture position unknown")
					tr, rec := closeTranslator(t, closePositions(func(_ context.Context, _, venue, _ string) (*big.Rat, error) {
						if venue == unknownVenue {
							if failure == "error" {
								return nil, unknown
							}
							return nil, nil
						}
						return big.NewRat(3, 5), nil
					}))
					result, err := tr.Emit(t.Context(), intent(signalpb.SignalAction_SIGNAL_ACTION_CLOSE, nil, signalpb.SizeType_SIZE_TYPE_UNSPECIFIED))
					if err == nil || result != nil || len(rec.events) != 0 {
						t.Fatalf("unresolved %s CLOSE had effects: result=%+v err=%v events=%d", unknownVenue, result, err, len(rec.events))
					}
				})
			}
		})
	}
}

func TestClosePreflightResolvesOnceAndPreservesLongShortFlat(t *testing.T) {
	for _, quantity := range []int64{3, -3, 0} {
		t.Run(big.NewInt(quantity).String(), func(t *testing.T) {
			calls := map[string]int{}
			tr, rec := closeTranslator(t, closePositions(func(_ context.Context, _, venue, _ string) (*big.Rat, error) {
				calls[venue]++
				if calls[venue] > 1 {
					return nil, errors.New("quantity was resolved twice")
				}
				return big.NewRat(quantity, 5), nil
			}))
			result, err := tr.Emit(t.Context(), intent(signalpb.SignalAction_SIGNAL_ACTION_CLOSE, nil, signalpb.SizeType_SIZE_TYPE_UNSPECIFIED))
			if err != nil || calls["BINANCE"] != 1 || calls["OKX"] != 1 || len(rec.facts()) != 1 {
				t.Fatalf("close did not resolve once: %+v %v calls=%v", result, err, calls)
			}
			want := 2
			if quantity == 0 {
				want = 0
			}
			if len(rec.commands()) != want || len(result.OrderIDs) != want {
				t.Fatalf("flat/held command count=%d want%d", len(rec.commands()), want)
			}
			for _, cmd := range rec.commands() {
				side := orderpb.Side_SIDE_SELL
				if quantity < 0 {
					side = orderpb.Side_SIDE_BUY
				}
				if cmd.Side != side || dec.FromProto(cmd.Quantity).Cmp(big.NewRat(3, 5)) != 0 || cmd.OrderId != DeterministicID("sig-1", cmd.Venue) || cmd.PortfolioId != "fund-alpha" {
					t.Fatalf("close command changed: %v", cmd)
				}
			}
		})
	}
	for _, action := range []signalpb.SignalAction{signalpb.SignalAction_SIGNAL_ACTION_BUY, signalpb.SignalAction_SIGNAL_ACTION_SELL} {
		t.Run(action.String(), func(t *testing.T) {
			tr, rec := closeTranslator(t, closePositions(func(context.Context, string, string, string) (*big.Rat, error) {
				t.Fatal("sized order looked up a CLOSE position")
				return nil, nil
			}))
			_, err := tr.Emit(t.Context(), intent(action, big.NewRat(2, 1), signalpb.SizeType_SIZE_TYPE_ABSOLUTE_QTY))
			if err != nil || len(rec.commands()) != 2 {
				t.Fatalf("sized order changed: %v", err)
			}
			for _, cmd := range rec.commands() {
				if dec.FromProto(cmd.Quantity).Cmp(big.NewRat(1, 1)) != 0 {
					t.Fatalf("sized split changed: %v", cmd)
				}
			}
		})
	}
}
