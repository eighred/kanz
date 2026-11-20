//go:build synthetic_handoff

package order

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/proto"
)

var errSyntheticRemainder = errors.New("oms: synthetic remainder does not match the retained order and execution")

// SyntheticHandoffRemainder checks the complete typed XSIM history against the
// retained execution. Only the second half at the same price and fee belongs
// to this handback profile. Existing working and cancellation paths are separate.
func SyntheticHandoffRemainder(st *orderpb.OrderState, retained *orderpb.Fill, view execution.OrderView) (*orderpb.Fill, error) {
	if st.GetVenue() != "XSIM" || st.GetVenueAccountId() != "synthetic-spot" || st.GetInstrumentId() != "SYNTH-USD" ||
		st.GetOrderId() == "" || st.GetPortfolioId() == "" || st.GetParentOrderId() != "" ||
		st.GetQuarantine() != nil || st.GetCancellationRequest() != nil || st.GetVenueAckAt().CheckValid() != nil ||
		st.GetAsOf().CheckValid() != nil || st.GetSide() != orderpb.Side_SIDE_BUY ||
		view.State != execution.OrderViewFilled || len(view.Fills) != 2 ||
		!syntheticFillID(retained.GetFillId(), "xsim-partial/") || retained.GetVenueExecutionId() != retained.GetFillId() ||
		retained.GetOrderId() != st.GetOrderId() || retained.GetVenue() != st.GetVenue() ||
		retained.GetVenueAccountId() != st.GetVenueAccountId() || retained.GetInstrumentId() != st.GetInstrumentId() ||
		retained.GetSide() != st.GetSide() || retained.GetRecovery() != nil || retained.GetExecutedAt().CheckValid() != nil ||
		!proto.Equal(retained, view.Fills[0]) || !proto.Equal(retained.GetPrice(), st.GetLimitPrice()) ||
		retained.GetFee().GetCurrencyCode() != "USD" {
		return nil, errSyntheticRemainder
	}
	for _, value := range []*commonpb.Decimal{st.GetOrderedQuantity(), view.ExecutedQuantity, retained.GetQuantity(),
		retained.GetPrice(), retained.GetFee().GetAmount()} {
		if !dec.InDomain(value) || !dec.IsPositive(value) {
			return nil, errSyntheticRemainder
		}
	}
	// Compare the actual two half quantities to the independent venue total.
	// Decimal conversion stays exact even when the coefficient cannot double.
	first := dec.FromProto(retained.GetQuantity())
	total := first.Add(first, first)
	if total.Cmp(dec.FromProto(st.GetOrderedQuantity())) != 0 || total.Cmp(dec.FromProto(view.ExecutedQuantity)) != 0 {
		return nil, errSyntheticRemainder
	}
	fill := view.Fills[1]
	if !syntheticFillID(fill.GetFillId(), "xsim-remainder/") || fill.GetExecutedAt().CheckValid() != nil ||
		!fill.GetExecutedAt().AsTime().After(retained.GetExecutedAt().AsTime()) {
		return nil, errSyntheticRemainder
	}
	expected := proto.Clone(retained).(*orderpb.Fill)
	expected.FillId, expected.VenueExecutionId, expected.ExecutedAt = fill.GetFillId(), fill.GetFillId(), fill.GetExecutedAt()
	if !proto.Equal(expected, fill) || !dec.InDomain(st.GetFilledQuantity()) || !dec.InDomain(st.GetLeavesQuantity()) {
		return nil, errSyntheticRemainder
	}
	switch st.GetStatus() {
	case orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED:
		if dec.Cmp(st.GetFilledQuantity(), retained.GetQuantity()) != 0 || dec.Cmp(st.GetLeavesQuantity(), fill.GetQuantity()) != 0 ||
			!fill.GetExecutedAt().AsTime().After(st.GetAsOf().AsTime()) {
			return nil, errSyntheticRemainder
		}
	case orderpb.OrderStatus_ORDER_STATUS_FILLED:
		if dec.Cmp(st.GetFilledQuantity(), view.ExecutedQuantity) != 0 ||
			st.GetLeavesQuantity().GetCoefficient() != 0 || !proto.Equal(st.GetAsOf(), fill.GetExecutedAt()) {
			return nil, errSyntheticRemainder
		}
	default:
		return nil, errSyntheticRemainder
	}
	return fill, nil
}

func syntheticFillID(value, prefix string) bool {
	digest, ok := strings.CutPrefix(value, prefix)
	if !ok || len(digest) != 64 || digest != strings.ToLower(digest) {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

// ReconcileSyntheticHandoffRemainder applies a terminal XSIM observation in
// the isolated writer-handoff fixture. The database owns the epoch, exact
// authorization, execution claim and atomic cash/order/outbox commit.
func (s *Service) ReconcileSyntheticHandoffRemainder(ctx context.Context, store *Postgres, orderID string) error {
	if store == nil || s.store != store || s.router == nil || bus.TenantIDFromContext(ctx) != s.tenant {
		return errSyntheticRemainder
	}
	st, _, err := store.Load(ctx, orderID)
	if err != nil {
		return err
	}
	var raw []byte
	if err := store.pool.QueryRow(ctx, `SELECT fill FROM public.order_executions WHERE order_id=$1 AND fill_id LIKE 'xsim-partial/%'`, orderID).Scan(&raw); err != nil {
		return err
	}
	var retained orderpb.Fill
	if err := proto.Unmarshal(raw, &retained); err != nil {
		return fmt.Errorf("oms: read retained synthetic execution: %w", err)
	}
	venue, err := s.router.Route(st)
	if err != nil {
		return err
	}
	query, ok := venue.(execution.Querier)
	if !ok {
		return errSyntheticRemainder
	}
	view, err := query.QueryOrder(ctx, st)
	if err != nil {
		return err
	}
	fill, err := SyntheticHandoffRemainder(st, &retained, view)
	if err != nil {
		return err
	}
	at := fill.GetExecutedAt()
	next := st
	if st.GetStatus() == orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED {
		next, err = ApplyFill(st, fill, at.AsTime())
		if err != nil {
			return err
		}
	}
	fact, err := s.emitter.FillFact(ctx, fill, next)
	if err != nil {
		return err
	}
	wire := proto.MarshalOptions{Deterministic: true}
	executionBytes, err := wire.Marshal(fill)
	if err != nil {
		return err
	}
	stateBytes, err := wire.Marshal(next)
	if err != nil {
		return err
	}
	ctx, done := commitContext(ctx)
	defer done()
	var decision string
	err = store.pool.QueryRow(ctx, `SELECT public.synthetic_oms_go_fill_remainder($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		st.GetPortfolioId(), orderID, fill.GetFillId(), at.Seconds, at.Nanos, raw, executionBytes, stateBytes, fact.Payload).Scan(&decision)
	if err != nil {
		return err
	}
	if decision != "APPLIED" && decision != "DUPLICATE" {
		return errSyntheticRemainder
	}
	_, err = s.relay.Flush(ctx, orderID)
	return err
}
