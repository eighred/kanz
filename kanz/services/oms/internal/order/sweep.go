package order

import (
	"context"
	"fmt"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
)

// SweepInterrupted reconciles every order this OMS left mid-flight, and it must
// run BEFORE the consumers subscribe.
//
// WHY A SWEEP AS WELL AS THE REDELIVERY PATH. A redelivery rescues an order
// whose command is still in the stream. It does nothing for an order whose
// command was ACKED and whose process then died between Save(ROUTED) and the
// fold: no redelivery is coming, and nothing will ever mention that order again.
// That order is a live position, or an order resting at an exchange, that this
// platform has forgotten. The sweep is the only thing that finds it.
//
// The shape is tv-sync's (cmd/tv-sync/main.go:114): rebuild what you know before
// anything can read it or add to it. The caller treats a failure the same way —
// a pod that could not reconcile its in-flight orders does not know what the
// fund holds, and starting anyway is not degraded service, it is wrong service.
//
// It returns the number of orders it acted on. Orders it leaves alone (the venue
// is working them) and orders it freezes both count: they were interrupted, and
// the number is the operator's first signal about how bad the interruption was.
func (s *Service) SweepInterrupted(ctx context.Context) (int, error) {
	open, err := s.store.ListByStatus(ctx,
		orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW,
		orderpb.OrderStatus_ORDER_STATUS_ROUTED,
		orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED,
	)
	if err != nil {
		return 0, fmt.Errorf("oms: could not list in-flight orders to reconcile: %w", err)
	}

	swept := 0
	for _, st := range open {
		if st.GetQuarantine() != nil {
			continue // already frozen; a human owns it
		}
		if err := s.resume(ctx, st); err != nil {
			// One order that cannot be reconciled stops the sweep. The alternative
			// is a pod that starts having silently skipped an order it could not
			// account for, which is the failure this whole path exists to end.
			return swept, fmt.Errorf("oms: could not reconcile in-flight order %s: %w",
				st.GetOrderId(), err)
		}
		swept++
	}
	return swept, nil
}
