package translate

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	signalpb "github.com/eighred/kanz/kanz-schemas-go/signal/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/pkg/bus"
)

// The three defects of #240 met at one root: values crossing this boundary carried
// no unit. These tests pin the resolved-unit bound, the venue-weight audit, and the
// leverage refusal.

type recorder struct{ events []bus.Event }

func (r *recorder) Publish(_ context.Context, e bus.Event) error {
	r.events = append(r.events, e)
	return nil
}

func (r *recorder) commands() []*orderpb.SubmitOrder {
	var out []*orderpb.SubmitOrder
	for _, e := range r.events {
		if c, ok := e.Payload.(*orderpb.SubmitOrder); ok {
			out = append(out, c)
		}
	}
	return out
}

func (r *recorder) facts() []*signalpb.StrategySignal {
	var out []*signalpb.StrategySignal
	for _, e := range r.events {
		if s, ok := e.Payload.(*signalpb.StrategySignal); ok {
			out = append(out, s)
		}
	}
	return out
}

// boundedTranslator mirrors the #240 scenario exactly: a cap of 10 written by an
// operator who meant "10 BTC", a $1e9 NAV, and a $50k mark.
func boundedTranslator(t *testing.T, max Qty) (*Translator, *recorder) {
	t.Helper()
	rec := &recorder{}
	tr, err := New(Options{
		Prices: StaticPrices{"BTC-USD": big.NewRat(50_000, 1)},
		Equity: StaticEquity{"fund-alpha": new(big.Rat).SetInt64(1_000_000_000)},
		Positions: StaticPositions{
			"fund-alpha/BINANCE/BTC-USD": big.NewRat(500, 1), // a large open long, for the CLOSE case
		},
		Alloc:       StaticAllocation{"fund-alpha": {{Venue: "BINANCE", Weight: big.NewRat(1, 1)}}},
		Publisher:   rec,
		Gate:        halt.OpenGate(nil),
		Authority:   boundAuthority(t),
		MaxQuantity: max,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return tr, rec
}

func intent(action signalpb.SignalAction, size *big.Rat, st signalpb.SizeType) Intent {
	return Intent{
		SignalID:     "sig-1",
		StrategyID:   "momentum",
		FundID:       "fund-alpha",
		InstrumentID: "BTC-USD",
		Action:       action,
		Size:         size,
		SizeType:     st,
		OrderType:    orderpb.OrderType_ORDER_TYPE_MARKET,
		Source:       signalpb.SignalSource_SIGNAL_SOURCE_TRADINGVIEW_WEBHOOK,
	}
}

// THE defect: a cap of 10 meaning "10 BTC" used to admit size=9 as a percentage of
// equity, because the bound was compared against the raw number before size_type
// decided what it meant. 9% of a $1e9 NAV at $50k is 1,800 BTC — 180x the cap.
func TestMaxQuantity_BoundsTheResolvedUnit(t *testing.T) {
	tr, rec := boundedTranslator(t, NewQty(big.NewRat(10, 1)))

	_, err := tr.Emit(context.Background(),
		intent(signalpb.SignalAction_SIGNAL_ACTION_BUY, big.NewRat(9, 1),
			signalpb.SizeType_SIZE_TYPE_PCT_OF_EQUITY))
	if !errors.Is(err, ErrSizeExceedsMax) {
		t.Fatalf("9%% of a $1e9 NAV resolved to 1800 BTC against a cap of 10 and was ACCEPTED: err = %v, "+
			"want ErrSizeExceedsMax", err)
	}
	// The error must name the resolved quantity, not the raw size — an operator
	// reading "size 9 exceeds max 10" would conclude the cap is broken.
	if !strings.Contains(err.Error(), "1800") {
		t.Errorf("error does not name the resolved quantity 1800: %v", err)
	}
	// And NOTHING reached the bus: a refused signal must not leave a StrategySignal
	// FACT with no orders chained to it, which reads as a lost fan-out.
	if n := len(rec.events); n != 0 {
		t.Fatalf("a refused signal published %d events, want 0 — the audit root records an "+
			"intent the platform never acted on", n)
	}
}

// The bound must still be a bound in its own unit, and must still let a compliant
// signal through — a cap that refuses everything is not a fix.
func TestMaxQuantity_AdmitsAndRefusesInTheSameUnit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		size     *big.Rat
		sizeType signalpb.SizeType
		wantQty  *big.Rat // nil ⇒ expect refusal
	}{
		{"absolute under the cap", big.NewRat(9, 1), signalpb.SizeType_SIZE_TYPE_ABSOLUTE_QTY, big.NewRat(9, 1)},
		{"absolute at the cap", big.NewRat(10, 1), signalpb.SizeType_SIZE_TYPE_ABSOLUTE_QTY, big.NewRat(10, 1)},
		{"absolute over the cap", big.NewRat(11, 1), signalpb.SizeType_SIZE_TYPE_ABSOLUTE_QTY, nil},
		// $250k notional / $50k = 5 BTC — well under a cap the raw number (250000)
		// would have blown through by five orders of magnitude.
		{"notional resolving under the cap", big.NewRat(250_000, 1), signalpb.SizeType_SIZE_TYPE_QUOTE_NOTIONAL, big.NewRat(5, 1)},
		{"notional resolving over the cap", big.NewRat(1_000_000, 1), signalpb.SizeType_SIZE_TYPE_QUOTE_NOTIONAL, nil},
		// 0.05% of $1e9 = $500k / $50k = 10 BTC, exactly at the cap.
		{"pct resolving to the cap", big.NewRat(5, 100), signalpb.SizeType_SIZE_TYPE_PCT_OF_EQUITY, big.NewRat(10, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, rec := boundedTranslator(t, NewQty(big.NewRat(10, 1)))
			_, err := tr.Emit(context.Background(),
				intent(signalpb.SignalAction_SIGNAL_ACTION_BUY, tc.size, tc.sizeType))
			if tc.wantQty == nil {
				if !errors.Is(err, ErrSizeExceedsMax) {
					t.Fatalf("err = %v, want ErrSizeExceedsMax", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Emit = %v, want the signal admitted", err)
			}
			cmds := rec.commands()
			if len(cmds) != 1 {
				t.Fatalf("got %d commands, want 1", len(cmds))
			}
			if got := dec.FromProto(cmds[0].GetQuantity()); got.Cmp(tc.wantQty) != 0 {
				t.Fatalf("quantity = %s, want %s", got.RatString(), tc.wantQty.RatString())
			}
		})
	}
}

// A CLOSE flattens a position that already exists. A size cap that can refuse a
// flatten is a cap that traps a fund in a position it asked to exit.
func TestMaxQuantity_DoesNotBlockAClose(t *testing.T) {
	tr, rec := boundedTranslator(t, NewQty(big.NewRat(10, 1)))
	_, err := tr.Emit(context.Background(),
		intent(signalpb.SignalAction_SIGNAL_ACTION_CLOSE, nil, signalpb.SizeType_SIZE_TYPE_UNSPECIFIED))
	if err != nil {
		t.Fatalf("CLOSE of a 500 BTC position under a cap of 10 = %v, want it admitted — "+
			"the fund cannot exit", err)
	}
	cmds := rec.commands()
	if len(cmds) != 1 || dec.FromProto(cmds[0].GetQuantity()).Cmp(big.NewRat(500, 1)) != 0 {
		t.Fatalf("close legs = %v, want one leg flattening 500", cmds)
	}
}

// leverage was parsed, bounds-checked, and written onto the FACT — then dropped,
// because SubmitOrder has no such field. Refusing is the only honest answer until
// it is plumbed through.
func TestEmit_RefusesLeverageItCannotExecute(t *testing.T) {
	tr, rec := boundedTranslator(t, Qty{})
	in := intent(signalpb.SignalAction_SIGNAL_ACTION_BUY, big.NewRat(1, 1),
		signalpb.SizeType_SIZE_TYPE_ABSOLUTE_QTY)
	in.Leverage = big.NewRat(10, 1)

	_, err := tr.Emit(context.Background(), in)
	if !errors.Is(err, ErrInvalidIntent) {
		t.Fatalf("leverage=10 = %v, want ErrInvalidIntent — the order placed is unlevered while "+
			"the audit root claims 10x", err)
	}
	if n := len(rec.facts()); n != 0 {
		t.Fatalf("a refused levered signal recorded %d StrategySignal FACTs, want 0 — that FACT is "+
			"the audit root, and it would assert a position the fund never held", n)
	}
}

func TestEmit_RefusesMarginModeItCannotExecute(t *testing.T) {
	tr, _ := boundedTranslator(t, Qty{})
	in := intent(signalpb.SignalAction_SIGNAL_ACTION_BUY, big.NewRat(1, 1),
		signalpb.SizeType_SIZE_TYPE_ABSOLUTE_QTY)
	in.MarginMode = signalpb.MarginMode_MARGIN_MODE_CROSS

	if _, err := tr.Emit(context.Background(), in); !errors.Is(err, ErrInvalidIntent) {
		t.Fatalf("margin_mode=CROSS = %v, want ErrInvalidIntent", err)
	}
}

// Unlevered spot is what the platform executes, so it is what it accepts — both
// spellings: an explicit 1, and an absent leverage (which publishSignal records
// as 1).
func TestEmit_AcceptsUnleveredBothWays(t *testing.T) {
	for _, lev := range []*big.Rat{nil, big.NewRat(1, 1)} {
		tr, _ := boundedTranslator(t, Qty{})
		in := intent(signalpb.SignalAction_SIGNAL_ACTION_BUY, big.NewRat(1, 1),
			signalpb.SizeType_SIZE_TYPE_ABSOLUTE_QTY)
		in.Leverage = lev
		if _, err := tr.Emit(context.Background(), in); err != nil {
			t.Fatalf("leverage %v = %v, want accepted", lev, err)
		}
	}
}

// --- venue weights: a split, audited at startup ---

func TestNew_RefusesWeightsThatAreNotASplit(t *testing.T) {
	for _, tc := range []struct {
		name string
		legs []VenueAllocation
		want string
	}{
		{
			// The natural mistake: every other percentage on this surface is
			// whole-percent, so 60/40 is what an operator writes. It used to load clean
			// and turn a 2 BTC signal into 120 + 80 BTC.
			"whole percents instead of fractions",
			[]VenueAllocation{{Venue: "BINANCE", Weight: big.NewRat(60, 1)}, {Venue: "OKX", Weight: big.NewRat(40, 1)}},
			"sum to 100",
		},
		{
			"duplicated leg",
			[]VenueAllocation{{Venue: "BINANCE", Weight: big.NewRat(6, 10)}, {Venue: "BINANCE", Weight: big.NewRat(6, 10)}},
			"twice",
		},
		{
			"weights that do not reach 1",
			[]VenueAllocation{{Venue: "BINANCE", Weight: big.NewRat(6, 10)}, {Venue: "OKX", Weight: big.NewRat(3, 10)}},
			"sum to 9/10",
		},
		{
			"a zero-weight leg",
			[]VenueAllocation{{Venue: "BINANCE", Weight: big.NewRat(1, 1)}, {Venue: "OKX", Weight: new(big.Rat)}},
			"positive fraction",
		},
		{
			"a nil weight",
			[]VenueAllocation{{Venue: "BINANCE", Weight: nil}},
			"positive fraction",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Options{
				Prices: StaticPrices{}, Equity: StaticEquity{}, Positions: StaticPositions{},
				Alloc:     StaticAllocation{"fund-alpha": tc.legs},
				Publisher: &recorder{}, Gate: halt.OpenGate(nil),
				Authority: boundAuthority(t),
			})
			if !errors.Is(err, ErrBadAllocation) {
				t.Fatalf("New = %v, want ErrBadAllocation — this allocation scales every order "+
					"the fund ever places", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q — an operator cannot see what to fix", err, tc.want)
			}
		})
	}
}

func TestNew_AcceptsARealSplit(t *testing.T) {
	_, err := New(Options{
		Prices: StaticPrices{}, Equity: StaticEquity{}, Positions: StaticPositions{},
		Alloc: StaticAllocation{"fund-alpha": {
			{Venue: "BINANCE", Weight: big.NewRat(6, 10)},
			{Venue: "OKX", Weight: big.NewRat(4, 10)},
		}},
		Publisher: &recorder{}, Gate: halt.OpenGate(nil),
		Authority: boundAuthority(t),
	})
	if err != nil {
		t.Fatalf("New on a valid 0.6/0.4 split = %v", err)
	}
}
