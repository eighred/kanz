package ingest

import (
	"context"
	"math/big"
	"net"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
)

// fillAtMark is a minimal venue double: it fills a SubmitOrder in full at the
// mark, producing an order.v1.Fill exactly as the OMS's SimVenue does for a
// marketable order. It exists only because the Go internal-package boundary (an
// intentional service-isolation invariant, enforced by test/arch) forbids
// importing services/oms/internal/execution here — the REAL SimVenue executes
// these same commands live over the bus. The point of this test is the
// end-to-end property the ingest half owns: a signal fanned across N venues
// conserves its resolved quantity, and every emitted command is a valid,
// fillable order.v1.SubmitOrder.
func fillAtMark(cmd *orderpb.SubmitOrder, mark *big.Rat) *orderpb.Fill {
	return &orderpb.Fill{
		FillId:       "fill-" + cmd.GetOrderId(),
		OrderId:      cmd.GetOrderId(),
		InstrumentId: cmd.GetInstrumentId(),
		Side:         cmd.GetSide(),
		Quantity:     cmd.GetQuantity(),
		Price:        dec.ToProto(mark),
		Venue:        "SIM",
	}
}

// TestLoop_FanOutConservesSizeAndFills drives the full ingest path, then runs
// each emitted command through the venue double, and asserts the fills sum back
// to the resolved base quantity — the loop closes with no size leaked or
// invented.
func TestLoop_FanOutConservesSizeAndFills(t *testing.T) {
	p, cap := harness(t)
	// 5% of 1,000,000 NAV / 50,000 mark = 1.0 base, split 0.6 / 0.4.
	raw := body("buy", "5", "pct_of_equity", "loop-1")
	if _, err := p.Process(context.Background(), []byte(raw), net.ParseIP("10.0.0.1"), sign(raw, testSecret)); err != nil {
		t.Fatalf("Process: %v", err)
	}

	cmds := cap.commands()
	if len(cmds) != 2 {
		t.Fatalf("want 2 fanned-out commands, got %d", len(cmds))
	}

	mark := big.NewRat(50000, 1)
	filled := new(big.Rat)
	for _, c := range cmds {
		// Every command is a well-formed, fillable market order.
		if c.GetOrderType() != orderpb.OrderType_ORDER_TYPE_MARKET {
			t.Errorf("order %s not a market order: %v", c.GetOrderId(), c.GetOrderType())
		}
		if !dec.IsPositive(c.GetQuantity()) {
			t.Errorf("order %s has non-positive quantity", c.GetOrderId())
		}
		fill := fillAtMark(c, mark)
		if fill.GetSide() != orderpb.Side_SIDE_BUY {
			t.Errorf("fill side = %v, want BUY", fill.GetSide())
		}
		filled.Add(filled, dec.FromProto(fill.GetQuantity()))
	}

	// Conservation: the fanned-out fills reconstruct the resolved base size (1.0).
	if want := big.NewRat(1, 1); filled.Cmp(want) != 0 {
		t.Fatalf("total filled = %s, want %s (fan-out must conserve size)", filled.RatString(), want.RatString())
	}
}
