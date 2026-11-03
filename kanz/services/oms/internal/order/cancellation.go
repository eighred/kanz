package order

import (
	"context"
	"errors"
	"fmt"

	"github.com/eighred/kanz/internal/outbox"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

var ErrParentStopped = errors.New("oms: scheduled parent no longer permits child admission or dispatch")
var ErrCancellationPending = errors.New("oms: cancellation is pending")

// The parent row is the cross-replica admission barrier. Lock it before the
// child row, and release it at commit, never across a venue network call.
func lockWorkingParent(ctx context.Context, tx pgx.Tx, child *orderpb.OrderState) error {
	if child.GetParentOrderId() == "" {
		return nil
	}
	var blob []byte
	err := tx.QueryRow(ctx, `SELECT state FROM orders WHERE order_id=$1 FOR UPDATE`, child.GetParentOrderId()).Scan(&blob)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrParentStopped
	}
	if err != nil {
		return err
	}
	parent, err := unmarshalState(blob, child.GetParentOrderId())
	if err != nil {
		return err
	}
	return checkWorkingParent(parent, child)
}

func checkWorkingParent(parent, child *orderpb.OrderState) error {
	if parent == nil || parent.GetOrderId() == child.GetOrderId() ||
		parent.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_WORKING_SCHEDULED ||
		parent.GetQuarantine() != nil || parent.GetCancellationRequest() != nil ||
		parent.GetPortfolioId() != child.GetPortfolioId() {
		return ErrParentStopped
	}
	return nil
}

func announcesDispatch(records []outbox.Record) bool {
	for _, record := range records {
		if record.EventType == EventTypeRouted {
			return true
		}
	}
	return false
}

// A fresh version is not authority to erase a withdrawal. In particular the
// schedule retirement path must not replace a pending cancel with EXPIRED.
func preserveCancellation(current, next *orderpb.OrderState) error {
	request := current.GetCancellationRequest()
	if request != nil && (!proto.Equal(request, next.GetCancellationRequest()) ||
		next.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_WORKING_SCHEDULED &&
			next.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED) {
		return ErrCancellationPending
	}
	return nil
}

func (s *Service) resumeParentCancellation(ctx context.Context, parent *orderpb.OrderState) error {
	request := parent.GetCancellationRequest()
	if request.GetCommand().GetOrderId() != parent.GetOrderId() {
		return fmt.Errorf("oms: invalid durable cancellation for %s", parent.GetOrderId())
	}
	payload, err := proto.Marshal(request.GetCommand())
	if err != nil {
		return err
	}
	ctx = bus.WithCorrelationID(ctx, request.GetCorrelationId())
	ctx = bus.WithCausationID(ctx, request.GetCausationId())
	return s.handleCancel(ctx, payload)
}
