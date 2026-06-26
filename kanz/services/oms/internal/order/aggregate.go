// Package order is the OMS core: the order aggregate state machine (this file),
// its durable store (store.go), and the bus command handler that drives them
// (service.go). The aggregate is pure — it validates a command or applies an
// event to an OrderState and returns the next state, with no I/O — so the state
// transitions are unit-testable in isolation and identical on the live path and
// any replay.
package order

import (
	"fmt"
	"math/big"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"

	"github.com/kanz-eng/kanz/services/oms/internal/dec"
)

// RejectError is a validation/authority failure that maps to an ORDER_REJECTED
// FACT and a REJECTED command outcome. Code is the stable machine code; it
// mirrors the command.v1 outcome vocabulary.
type RejectError struct {
	Code string
	Msg  string
}

func (e *RejectError) Error() string { return e.Code + ": " + e.Msg }

func reject(code, format string, args ...any) *RejectError {
	return &RejectError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// Accept validates a SubmitOrder and returns the initial working state
// (PENDING_NEW). A returned *RejectError means the order is refused before any
// effect; nil error means it is admitted.
func Accept(cmd *orderpb.SubmitOrder, now time.Time) (*orderpb.OrderState, error) {
	if cmd.GetOrderId() == "" {
		return nil, reject("INVALID_ORDER", "order_id required")
	}
	if cmd.GetPortfolioId() == "" {
		return nil, reject("INVALID_ORDER", "portfolio_id required")
	}
	if cmd.GetInstrumentId() == "" {
		return nil, reject("INVALID_ORDER", "instrument_id required")
	}
	if cmd.GetSide() == orderpb.Side_SIDE_UNSPECIFIED {
		return nil, reject("INVALID_ORDER", "side required")
	}
	if !dec.IsPositive(cmd.GetQuantity()) {
		return nil, reject("INVALID_QUANTITY", "quantity must be > 0")
	}
	if cmd.GetTimeInForce() == orderpb.TimeInForce_TIME_IN_FORCE_UNSPECIFIED {
		return nil, reject("INVALID_ORDER", "time_in_force required")
	}
	if cmd.GetTimeInForce() == orderpb.TimeInForce_TIME_IN_FORCE_GTD && cmd.GetExpireAt() == nil {
		return nil, reject("INVALID_ORDER", "expire_at required for GTD")
	}
	switch cmd.GetOrderType() {
	case orderpb.OrderType_ORDER_TYPE_UNSPECIFIED:
		return nil, reject("INVALID_ORDER", "order_type required")
	case orderpb.OrderType_ORDER_TYPE_LIMIT, orderpb.OrderType_ORDER_TYPE_STOP_LIMIT:
		if !dec.IsPositive(cmd.GetLimitPrice()) {
			return nil, reject("INVALID_PRICE", "limit_price required for a priced order")
		}
	}
	switch cmd.GetOrderType() {
	case orderpb.OrderType_ORDER_TYPE_STOP, orderpb.OrderType_ORDER_TYPE_STOP_LIMIT:
		if !dec.IsPositive(cmd.GetStopPrice()) {
			return nil, reject("INVALID_PRICE", "stop_price required for a stop order")
		}
	}

	zero := &commonpb.Decimal{Coefficient: 0, Exponent: 0}
	return &orderpb.OrderState{
		OrderId:         cmd.GetOrderId(),
		PortfolioId:     cmd.GetPortfolioId(),
		InstrumentId:    cmd.GetInstrumentId(),
		Side:            cmd.GetSide(),
		OrderType:       cmd.GetOrderType(),
		TimeInForce:     cmd.GetTimeInForce(),
		OrderedQuantity: cmd.GetQuantity(),
		LimitPrice:      cmd.GetLimitPrice(),
		Status:          orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW,
		FilledQuantity:  zero,
		LeavesQuantity:  cmd.GetQuantity(),
		AsOf:            timestamppb.New(now.UTC()),
	}, nil
}

// IsTerminal reports whether the order can no longer transition.
func IsTerminal(st *orderpb.OrderState) bool {
	switch st.GetStatus() {
	case orderpb.OrderStatus_ORDER_STATUS_FILLED,
		orderpb.OrderStatus_ORDER_STATUS_CANCELLED,
		orderpb.OrderStatus_ORDER_STATUS_REJECTED,
		orderpb.OrderStatus_ORDER_STATUS_EXPIRED:
		return true
	default:
		return false
	}
}

// Route marks an admitted order as working at a venue. Returns a copy.
func Route(st *orderpb.OrderState, now time.Time) *orderpb.OrderState {
	next := cloneState(st)
	next.Status = orderpb.OrderStatus_ORDER_STATUS_ROUTED
	next.AsOf = timestamppb.New(now.UTC())
	return next
}

// ApplyFill folds one fill into the order, recomputing filled/leaves quantity,
// the quantity-weighted average fill price, and the status (PARTIALLY_FILLED or
// FILLED). A fill exceeding the open quantity is rejected (over-fill guard).
func ApplyFill(st *orderpb.OrderState, fill *orderpb.Fill, now time.Time) (*orderpb.OrderState, error) {
	if IsTerminal(st) {
		return nil, reject("ORDER_TERMINAL", "cannot fill a %s order", st.GetStatus())
	}
	if !dec.IsPositive(fill.GetQuantity()) {
		return nil, reject("INVALID_FILL", "fill quantity must be > 0")
	}
	leaves := dec.FromProto(st.GetLeavesQuantity())
	fq := dec.FromProto(fill.GetQuantity())
	if fq.Cmp(leaves) > 0 {
		return nil, reject("OVERFILL", "fill quantity exceeds open quantity")
	}

	oldFilled := dec.FromProto(st.GetFilledQuantity())
	newFilled := new(big.Rat).Add(oldFilled, fq)
	newLeaves := new(big.Rat).Sub(dec.FromProto(st.GetOrderedQuantity()), newFilled)

	// Weighted average fill price: (oldFilled*oldAvg + fq*price) / newFilled.
	cost := new(big.Rat).Mul(oldFilled, dec.FromProto(st.GetAverageFillPrice()))
	cost.Add(cost, new(big.Rat).Mul(fq, dec.FromProto(fill.GetPrice())))
	avg := new(big.Rat).Quo(cost, newFilled)

	next := cloneState(st)
	next.FilledQuantity = dec.ToProto(newFilled)
	next.LeavesQuantity = dec.ToProto(newLeaves)
	next.AverageFillPrice = dec.ToProto(avg)
	next.AsOf = timestamppb.New(now.UTC())
	if newLeaves.Sign() == 0 {
		next.Status = orderpb.OrderStatus_ORDER_STATUS_FILLED
	} else {
		next.Status = orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED
	}
	return next, nil
}

// Cancel withdraws a resting order. It returns the updated state and the
// cancelled (open) quantity. A terminal order cannot be cancelled.
func Cancel(st *orderpb.OrderState, now time.Time) (*orderpb.OrderState, *commonpb.Decimal, error) {
	if IsTerminal(st) {
		return nil, nil, reject("ORDER_TERMINAL", "cannot cancel a %s order", st.GetStatus())
	}
	next := cloneState(st)
	next.Status = orderpb.OrderStatus_ORDER_STATUS_CANCELLED
	next.AsOf = timestamppb.New(now.UTC())
	return next, st.GetLeavesQuantity(), nil
}

// Expire terminates an order whose time-in-force elapsed. Returns the updated
// state and the unfilled quantity.
func Expire(st *orderpb.OrderState, now time.Time) (*orderpb.OrderState, *commonpb.Decimal, error) {
	if IsTerminal(st) {
		return nil, nil, reject("ORDER_TERMINAL", "cannot expire a %s order", st.GetStatus())
	}
	next := cloneState(st)
	next.Status = orderpb.OrderStatus_ORDER_STATUS_EXPIRED
	next.AsOf = timestamppb.New(now.UTC())
	return next, st.GetLeavesQuantity(), nil
}

// Amend changes a resting order's quantity and/or limit price. A new quantity
// below the already-filled amount is rejected.
func Amend(st *orderpb.OrderState, cmd *orderpb.AmendOrder, now time.Time) (*orderpb.OrderState, error) {
	if IsTerminal(st) {
		return nil, reject("ORDER_TERMINAL", "cannot amend a %s order", st.GetStatus())
	}
	next := cloneState(st)
	if q := cmd.GetNewQuantity(); q != nil {
		if !dec.IsPositive(q) {
			return nil, reject("INVALID_QUANTITY", "new_quantity must be > 0")
		}
		if dec.Cmp(q, st.GetFilledQuantity()) < 0 {
			return nil, reject("INVALID_QUANTITY", "new_quantity below filled quantity")
		}
		next.OrderedQuantity = q
		next.LeavesQuantity = dec.ToProto(new(big.Rat).Sub(dec.FromProto(q), dec.FromProto(st.GetFilledQuantity())))
	}
	if p := cmd.GetNewLimitPrice(); p != nil {
		switch st.GetOrderType() {
		case orderpb.OrderType_ORDER_TYPE_LIMIT, orderpb.OrderType_ORDER_TYPE_STOP_LIMIT:
			next.LimitPrice = p
		default:
			return nil, reject("INVALID_PRICE", "cannot set a limit on a non-priced order")
		}
	}
	next.AsOf = timestamppb.New(now.UTC())
	return next, nil
}

func cloneState(st *orderpb.OrderState) *orderpb.OrderState {
	return &orderpb.OrderState{
		OrderId:          st.GetOrderId(),
		PortfolioId:      st.GetPortfolioId(),
		InstrumentId:     st.GetInstrumentId(),
		Side:             st.GetSide(),
		OrderType:        st.GetOrderType(),
		TimeInForce:      st.GetTimeInForce(),
		OrderedQuantity:  st.GetOrderedQuantity(),
		LimitPrice:       st.GetLimitPrice(),
		Status:           st.GetStatus(),
		FilledQuantity:   st.GetFilledQuantity(),
		LeavesQuantity:   st.GetLeavesQuantity(),
		AverageFillPrice: st.GetAverageFillPrice(),
		AsOf:             st.GetAsOf(),
	}
}
