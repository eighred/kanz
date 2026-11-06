package main

import (
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func decimal(c int64, e int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: c, Exponent: e}
}
func unknown(n protowire.Number) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), []byte("retained-unknown"))
}
func baseState() *orderpb.OrderState {
	s := &orderpb.OrderState{OrderId: "order", PortfolioId: "portfolio", InstrumentId: "BTC/USD", Side: orderpb.Side_SIDE_BUY, OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, TimeInForce: orderpb.TimeInForce_TIME_IN_FORCE_GTC, OrderedQuantity: decimal(10, 0), FilledQuantity: decimal(0, 0), LeavesQuantity: decimal(10, 0), LimitPrice: decimal(123, -2), AverageFillPrice: decimal(17, -1), ExpireAt: timestamppb.New(eventTime.Add(time.Hour)), StopPrice: decimal(122, -2), Status: orderpb.OrderStatus_ORDER_STATUS_ROUTED, Venue: "fixture", VenueAccountId: "account", ParentOrderId: "parent", AsOf: timestamppb.New(eventTime.Add(-1)), ArrivalPrice: decimal(99, 0), ArrivalAt: timestamppb.New(eventTime), ReleasePrice: decimal(101, 0), ReleaseBid: decimal(100, 0), ReleaseAsk: decimal(102, 0), ReleaseAt: timestamppb.New(eventTime), Leverage: decimal(2, 0), MarginMode: 2, VenueAckAt: timestamppb.New(eventTime), Quarantine: &orderpb.OrderQuarantine{Reason: "retained", At: timestamppb.New(eventTime)}, AcceptedAnnouncedAt: timestamppb.New(eventTime), CancelAnnouncedAt: timestamppb.New(eventTime), OutcomeAnnouncedAt: timestamppb.New(eventTime), ExecutionSchedule: &orderpb.ExecutionSchedule{Algo: 1, SliceCount: 2, WindowStart: timestamppb.New(eventTime), WindowEnd: timestamppb.New(eventTime.Add(1000000000))}}
	s.ProtoReflect().SetUnknown(append(unknown(501), unknown(33)...))
	s.LimitPrice.ProtoReflect().SetUnknown(unknown(502))
	s.AverageFillPrice.ProtoReflect().SetUnknown(unknown(505))
	s.ExpireAt.ProtoReflect().SetUnknown(unknown(506))
	s.Quarantine.ProtoReflect().SetUnknown(unknown(503))
	s.AsOf.ProtoReflect().SetUnknown(unknown(504))
	return s
}
func submit() *orderpb.SubmitOrder {
	return &orderpb.SubmitOrder{OrderId: "order", PortfolioId: "portfolio", InstrumentId: "BTC/USD", Side: 1, OrderType: 2, TimeInForce: 1, Quantity: decimal(10, 0), LimitPrice: decimal(2, 0), Venue: "fixture", MarginMode: 2, Leverage: decimal(3, 0)}
}
func accepted(c *orderpb.SubmitOrder) *orderpb.OrderState {
	return &orderpb.OrderState{OrderId: c.OrderId, PortfolioId: c.PortfolioId, InstrumentId: c.InstrumentId, Side: c.Side, OrderType: c.OrderType, TimeInForce: c.TimeInForce, OrderedQuantity: c.Quantity, LeavesQuantity: c.Quantity, FilledQuantity: decimal(0, 0), LimitPrice: c.LimitPrice, StopPrice: c.StopPrice, ExpireAt: c.ExpireAt, MarginMode: c.MarginMode, Leverage: c.Leverage, Venue: c.Venue, Status: 1, AsOf: timestamppb.New(eventTime)}
}
func rq(op string, s *orderpb.OrderState) request {
	return request{Operation: op, State: wire(s), At: at()}
}
func changed(s *orderpb.OrderState, status orderpb.OrderStatus) *orderpb.OrderState {
	n := copyState(s)
	n.Status = status
	n.AsOf = timestamppb.New(eventTime)
	return n
}

// This arithmetic oracle uses decimal text rounding, independently of both
// implementations' integer quotient/remainder arithmetic. It preserves the
// established sequential half-away rescaling, including double rounding.
func rounded(r *big.Rat) *commonpb.Decimal {
	whole, fraction, _ := strings.Cut(r.FloatString(8), ".")
	n, _ := new(big.Int).SetString(whole+fraction, 10)
	e := int32(-8)
	for !n.IsInt64() {
		neg := n.Sign() < 0
		text := new(big.Int).Abs(n).String()
		up := text[len(text)-1] >= '5'
		n.SetString(text[:len(text)-1], 10)
		if up {
			n.Add(n, big.NewInt(1))
		}
		if neg {
			n.Neg(n)
		}
		e++
	}
	return decimal(n.Int64(), e)
}
func rational(d *commonpb.Decimal) *big.Rat {
	if d == nil {
		return new(big.Rat)
	}
	r := new(big.Rat).SetInt64(d.Coefficient)
	e := int64(d.Exponent)
	a := e
	if a < 0 {
		a = -a
	}
	if a > 64 {
		panic("oracle domain")
	}
	power := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(a), nil))
	if e < 0 {
		return r.Quo(r, power)
	}
	return r.Mul(r, power)
}
func corpus() []sample {
	var out []sample
	add := func(name, op string, s *orderpb.OrderState, want *orderpb.OrderState, code string) {
		out = append(out, sample{name: name, request: rq(op, s), want: want, code: code})
	}
	terminal := map[orderpb.OrderStatus]bool{4: true, 5: true, 6: true, 7: true}
	for _, status := range []orderpb.OrderStatus{0, 1, 2, 3, 4, 5, 6, 7, 8, 99} {
		for _, op := range []string{"Route", "Reject", "Cancel", "Expire", "ApplyFill", "Amend", "IsTerminal"} {
			s := baseState()
			s.Status = status
			name := fmt.Sprintf("matrix-%s-%d", op, status)
			code := ""
			var want *orderpb.OrderState
			var qty *commonpb.Decimal
			if terminal[status] && op != "IsTerminal" {
				code = "ORDER_TERMINAL"
			} else {
				switch op {
				case "Route":
					want = changed(s, 2)
				case "Reject":
					want = changed(s, 6)
				case "Cancel":
					want = changed(s, 5)
					qty = s.LeavesQuantity
				case "Expire":
					want = changed(s, 7)
					qty = s.LeavesQuantity
				case "Amend":
					want = changed(s, status)
				case "ApplyFill":
					want = changed(s, 3)
					want.FilledQuantity = decimal(100000000, -8)
					want.LeavesQuantity = decimal(900000000, -8)
					want.AverageFillPrice = decimal(200000000, -8)
				}
			}
			add(name, op, s, want, code)
			x := &out[len(out)-1]
			x.quantity = qty
			if op == "IsTerminal" {
				x.terminal = terminal[status]
			}
			if op == "ApplyFill" {
				x.request.Fill = wire(&orderpb.Fill{Quantity: decimal(1, 0), Price: decimal(2, 0)})
			}
			if op == "Amend" {
				x.request.Command = wire(&orderpb.AmendOrder{})
			}
		}
		for _, ack := range []bool{false, true} {
			for _, view := range []string{"Indeterminate", "Unknown", "Working", "PartiallyFilled", "Filled", "Rejected", "Cancelled", "Expired", "Future"} {
				s := baseState()
				s.Status = status
				if !ack {
					s.VenueAckAt = nil
				}
				action, reason := "ADOPT", ""
				switch {
				case terminal[status]:
					action, reason = "QUARANTINE", "TerminalOrder"
				case view == "Unknown" && !ack:
					action = "REDRIVE"
				case view == "Unknown":
					action, reason = "QUARANTINE", "AcknowledgementContradiction"
				case view == "Working":
					action = "LEAVE"
				case view == "Indeterminate" || view == "Future":
					action, reason = "QUARANTINE", "IndeterminateVenue"
				}
				q := rq("Reconcile", s)
				q.View = view
				out = append(out, sample{name: fmt.Sprintf("reconcile-%d-%t-%s", status, ack, view), request: q, action: action, reason: reason})
			}
		}
	}
	accepts := []struct {
		name string
		edit func(*orderpb.SubmitOrder)
		code string
	}{
		{"valid", func(c *orderpb.SubmitOrder) {}, ""}, {"missing-order", func(c *orderpb.SubmitOrder) { c.OrderId = "" }, "INVALID_ORDER"}, {"missing-portfolio", func(c *orderpb.SubmitOrder) { c.PortfolioId = "" }, "INVALID_ORDER"}, {"missing-instrument", func(c *orderpb.SubmitOrder) { c.InstrumentId = "" }, "INVALID_ORDER"}, {"instrument64bytes", func(c *orderpb.SubmitOrder) { c.InstrumentId = strings.Repeat("é", 32) }, ""}, {"instrument66bytes", func(c *orderpb.SubmitOrder) { c.InstrumentId = strings.Repeat("é", 33) }, "INVALID_ORDER"}, {"instrument-control", func(c *orderpb.SubmitOrder) { c.InstrumentId = "BTC\nUSD" }, "INVALID_ORDER"}, {"missing-side", func(c *orderpb.SubmitOrder) { c.Side = 0 }, "INVALID_ORDER"}, {"nil-quantity", func(c *orderpb.SubmitOrder) { c.Quantity = nil }, "INVALID_QUANTITY"}, {"zero-quantity", func(c *orderpb.SubmitOrder) { c.Quantity = decimal(0, 0) }, "INVALID_QUANTITY"}, {"negative-quantity", func(c *orderpb.SubmitOrder) { c.Quantity = decimal(-1, 0) }, "INVALID_QUANTITY"}, {"missing-tif", func(c *orderpb.SubmitOrder) { c.TimeInForce = 0 }, "INVALID_ORDER"}, {"gtd-missing-time", func(c *orderpb.SubmitOrder) { c.TimeInForce = 5 }, "INVALID_ORDER"}, {"gtd-present-time", func(c *orderpb.SubmitOrder) { c.TimeInForce = 5; c.ExpireAt = timestamppb.New(eventTime) }, ""}, {"missing-type", func(c *orderpb.SubmitOrder) { c.OrderType = 0 }, "INVALID_ORDER"}, {"limit-missing-price", func(c *orderpb.SubmitOrder) { c.LimitPrice = nil }, "INVALID_PRICE"}, {"stop-missing-price", func(c *orderpb.SubmitOrder) { c.OrderType = 3; c.StopPrice = nil }, "INVALID_PRICE"}, {"stop-limit", func(c *orderpb.SubmitOrder) { c.OrderType = 4; c.StopPrice = decimal(3, 0) }, ""}, {"future-enums-preserved", func(c *orderpb.SubmitOrder) { c.Side = 99; c.OrderType = 99; c.TimeInForce = 99 }, ""}, {"predicate-extreme-exponent", func(c *orderpb.SubmitOrder) { c.Quantity = decimal(1, math.MaxInt32) }, ""},
	}
	for _, v := range accepts {
		c := submit()
		v.edit(c)
		var want *orderpb.OrderState
		if v.code == "" {
			want = accepted(c)
		}
		out = append(out, sample{name: "accept-" + v.name, request: request{Operation: "Accept", Command: wire(c), At: at()}, want: want, code: v.code})
	}
	for _, nilQty := range []bool{true, false} {
		for _, op := range []string{"Cancel", "Expire"} {
			s := baseState()
			if nilQty {
				s.LeavesQuantity = nil
			} else {
				s.LeavesQuantity = decimal(0, 0)
			}
			status := orderpb.OrderStatus(5)
			if op == "Expire" {
				status = 7
			}
			add(fmt.Sprintf("optional-%s-%t", op, nilQty), op, s, changed(s, status), "")
			out[len(out)-1].quantity = s.LeavesQuantity
		}
	}
	addFill := func(name string, s *orderpb.OrderState, q, p *commonpb.Decimal, code string) {
		f := &orderpb.Fill{Quantity: q, Price: p}
		var want *orderpb.OrderState
		if code == "" {
			filled := new(big.Rat).Add(rational(s.FilledQuantity), rational(q))
			leaves := new(big.Rat).Sub(rational(s.OrderedQuantity), filled)
			cost := new(big.Rat).Add(new(big.Rat).Mul(rational(s.FilledQuantity), rational(s.AverageFillPrice)), new(big.Rat).Mul(rational(q), rational(p)))
			avg := new(big.Rat).Quo(cost, filled)
			want = changed(s, 3)
			if leaves.Sign() == 0 {
				want.Status = 4
			}
			want.FilledQuantity = rounded(filled)
			want.LeavesQuantity = rounded(leaves)
			want.AverageFillPrice = rounded(avg)
		}
		add("numeric-"+name, "ApplyFill", s, want, code)
		out[len(out)-1].request.Fill = wire(f)
	}
	for _, coefficient := range []int64{-6, -5, -4, 0, 4, 5, 6} {
		addFill(fmt.Sprintf("half-%d", coefficient), baseState(), decimal(1, 0), decimal(coefficient, -9), "")
	}
	addFill("nil-price", baseState(), decimal(1, 0), nil, "")
	addFill("nil-quantity", baseState(), nil, decimal(1, 0), "INVALID_FILL")
	addFill("overfill", baseState(), decimal(1000000001, -8), decimal(1, 0), "OVERFILL")
	addFill("tiny-quantity", baseState(), decimal(1, -9), decimal(1, 0), "")
	s := baseState()
	s.OrderedQuantity = decimal(math.MaxInt64, 0)
	s.LeavesQuantity = decimal(math.MaxInt64, 0)
	addFill("int64-rescale", s, decimal(math.MaxInt64, 0), decimal(math.MinInt64, 0), "")
	s = baseState()
	s.FilledQuantity = decimal(1, 0)
	s.LeavesQuantity = decimal(9, 0)
	s.AverageFillPrice = decimal(2, 20)
	addFill("sequential-double-round", s, decimal(1, 0), decimal(898, 0), "")
	s = baseState()
	s.FilledQuantity = nil
	addFill("nil-filled", s, decimal(1, 0), decimal(2, 0), "")
	for _, v := range []struct {
		name string
		q, p *commonpb.Decimal
		typ  orderpb.OrderType
		code string
	}{{"noop", nil, nil, 2, ""}, {"quantity", decimal(11, 0), nil, 2, ""}, {"equal-filled", decimal(2, 0), nil, 2, ""}, {"below-filled", decimal(1, 0), nil, 2, "INVALID_QUANTITY"}, {"zero", decimal(0, 0), nil, 2, "INVALID_QUANTITY"}, {"negative", decimal(-1, 0), nil, 2, "INVALID_QUANTITY"}, {"market-price", nil, decimal(4, 0), 1, "INVALID_PRICE"}, {"priced-negative-caller-owned", nil, decimal(-1, 0), 2, ""}, {"priced-zero-caller-owned", nil, decimal(0, 0), 4, ""}, {"tiny-remaining", decimal(2000000001, -9), nil, 2, ""}} {
		s := baseState()
		s.FilledQuantity = decimal(2, 0)
		s.LeavesQuantity = decimal(8, 0)
		s.OrderType = v.typ
		var want *orderpb.OrderState
		if v.code == "" {
			want = changed(s, s.Status)
			if v.q != nil {
				want.OrderedQuantity = v.q
				want.LeavesQuantity = rounded(new(big.Rat).Sub(rational(v.q), rational(s.FilledQuantity)))
			}
			if v.p != nil {
				want.LimitPrice = v.p
			}
		}
		add("amend-"+v.name, "Amend", s, want, v.code)
		out[len(out)-1].request.Command = wire(&orderpb.AmendOrder{NewQuantity: v.q, NewLimitPrice: v.p})
	}
	for _, stamp := range []timestamp{{0, 0}, {-1, 999999999}, {253402300799, 999999999}} {
		s := baseState()
		want := changed(s, 2)
		want.AsOf = timestamppb.New(time.Unix(stamp.Seconds, int64(stamp.Nanos)).UTC())
		q := rq("Route", s)
		q.At = stamp
		out = append(out, sample{name: fmt.Sprintf("timestamp-%d-%d", stamp.Seconds, stamp.Nanos), request: q, want: want})
	}
	return append(append(append(out, wireCases()...), wrongWireCases()...), replayCases()...)
}
func wireCases() []sample {
	var out []sample
	for _, op := range []string{"Route", "Reject", "Cancel", "Expire", "Amend", "ApplyFill"} {
		s := baseState()
		raw := wire(s)
		field := s.ProtoReflect().Descriptor().Fields().ByName("ordered_quantity").Number()
		// Duplicate embedded message with unknown-only content must merge, not erase
		// the coefficient/presence. A noncanonical known varint is then normalized.
		raw = protowire.AppendBytes(protowire.AppendTag(raw, field, protowire.BytesType), unknown(510))
		status := s.ProtoReflect().Descriptor().Fields().ByName("status").Number()
		raw = protowire.AppendTag(raw, status, protowire.VarintType)
		raw = append(raw, 0x82, 0x00)
		// Preserve group framing and noncanonical unknown value bytes through cloning.
		raw = protowire.AppendTag(raw, 530, protowire.StartGroupType)
		raw = protowire.AppendTag(raw, 531, protowire.VarintType)
		raw = append(raw, 0x81, 0x00)
		raw = protowire.AppendTag(raw, 530, protowire.EndGroupType)
		s = read(raw, &orderpb.OrderState{})
		want := changed(s, 2)
		q := rq(op, s)
		q.State = raw
		var qty *commonpb.Decimal
		switch op {
		case "Reject":
			want.Status = 6
		case "Cancel":
			want.Status = 5
			qty = s.LeavesQuantity
		case "Expire":
			want.Status = 7
			qty = s.LeavesQuantity
		case "Amend":
			a := &orderpb.AmendOrder{NewLimitPrice: decimal(7, 0)}
			a.NewLimitPrice.ProtoReflect().SetUnknown(unknown(511))
			q.Command = wire(a)
			want.LimitPrice = a.NewLimitPrice
		case "ApplyFill":
			q.Fill = wire(&orderpb.Fill{Quantity: decimal(1, 0), Price: decimal(2, 0)})
			want.Status = 3
			want.FilledQuantity = decimal(100000000, -8)
			want.LeavesQuantity = decimal(900000000, -8)
			want.AverageFillPrice = decimal(200000000, -8)
		}
		out = append(out, sample{name: "wire-duplicate-unknown-" + op, request: q, want: want, quantity: qty})
	}
	c := submit()
	c.Quantity.ProtoReflect().SetUnknown(unknown(520))
	c.Leverage.ProtoReflect().SetUnknown(unknown(521))
	c.ProtoReflect().SetUnknown(unknown(522))
	out = append(out, sample{name: "wire-accept-carries-nested-not-command-root", request: request{Operation: "Accept", Command: wire(c), At: at()}, want: accepted(c)})
	return out
}

// Each replay input is the independently specified preceding state, never Go output.
func replayCases() []sample {
	c := submit()
	first := accepted(c)
	routed := changed(first, 2)
	partial := changed(routed, 3)
	partial.FilledQuantity = decimal(300000000, -8)
	partial.LeavesQuantity = decimal(700000000, -8)
	partial.AverageFillPrice = decimal(2000000000, -8)
	amended := changed(partial, 3)
	amended.OrderedQuantity = decimal(12, 0)
	amended.LeavesQuantity = decimal(900000000, -8)
	cancelled := changed(amended, 5)
	return []sample{
		{name: "replay-accept", request: request{Operation: "Accept", Command: wire(c), At: at()}, want: first},
		{name: "replay-route", request: rq("Route", first), want: routed},
		{name: "replay-partial", request: request{Operation: "ApplyFill", State: wire(routed), Fill: wire(&orderpb.Fill{Quantity: decimal(3, 0), Price: decimal(20, 0)}), At: at()}, want: partial},
		{name: "replay-amend", request: request{Operation: "Amend", State: wire(partial), Command: wire(&orderpb.AmendOrder{NewQuantity: decimal(12, 0)}), At: at()}, want: amended},
		{name: "replay-cancel", request: rq("Cancel", amended), want: cancelled, quantity: amended.LeavesQuantity},
		{name: "replay-terminal-refuses-redrive", request: request{Operation: "Reconcile", State: wire(cancelled), View: "Unknown", At: at()}, action: "QUARANTINE", reason: "TerminalOrder"},
	}
}

// Known field numbers with incompatible wire types are unknown fields in Go,
// not a request to replace the known value. Clone/assignment must retain them.
func wrongWireCases() []sample {
	var out []sample
	for _, op := range []string{"Route", "Cancel", "Expire"} {
		s := baseState()
		fs := s.ProtoReflect().Descriptor().Fields()
		extra := unknown(fs.ByName("status").Number())
		for _, n := range []protowire.Number{fs.ByName("as_of").Number(), fs.ByName("leaves_quantity").Number()} {
			extra = protowire.AppendVarint(protowire.AppendTag(extra, n, protowire.VarintType), 7)
		}
		s.ProtoReflect().SetUnknown(append(s.ProtoReflect().GetUnknown(), extra...))
		s.LeavesQuantity.ProtoReflect().SetUnknown(unknown(1))
		want := changed(s, 2)
		var qty *commonpb.Decimal
		if op == "Cancel" {
			want.Status = 5
			qty = s.LeavesQuantity
		}
		if op == "Expire" {
			want.Status = 7
			qty = s.LeavesQuantity
		}
		out = append(out, sample{name: "wire-wrong-type-known-number-" + op, request: rq(op, s), want: want, quantity: qty})
	}
	c := submit()
	c.Quantity.ProtoReflect().SetUnknown(unknown(1))
	n := c.ProtoReflect().Descriptor().Fields().ByName("quantity").Number()
	c.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, n, protowire.VarintType), 9))
	out = append(out, sample{name: "wire-wrong-type-accept-quantity", request: request{Operation: "Accept", Command: wire(c), At: at()}, want: accepted(c)})
	return out
}
