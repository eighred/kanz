package order

import (
	"context"
	"errors"
	"fmt"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"

	"github.com/kanz-eng/kanz/pkg/bus"
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
	// A REDRIVEN ORDER IS PUBLISHED, NOT JUST RESUMED — SO IT NEEDS A TENANT
	// BEFORE IT NEEDS ANYTHING ELSE.
	//
	// Resuming a stranded order re-emits its lifecycle as FACT events (see
	// resume/work below), and every Publish needs a tenant to stamp on the
	// envelope. A handler gets its tenant for free — bus.Consumer lifts it off
	// the inbound envelope onto ctx (pkg/bus/consumer.go) before the handler
	// ever runs. This function has no such luxury: it is called at startup,
	// before the process has subscribed to anything, so there is no inbound
	// envelope to inherit a tenant from. If the caller forgot to supply one,
	// failing loudly here is the only alternative to a much worse failure
	// downstream: the first redrive would reach envelope validation, be
	// rejected for a missing tenant_id, and SweepInterrupted would return that
	// error to a caller that treats ANY sweep failure as fatal (by design —
	// see the package doc above and cmd/oms/main.go) — so the OMS would never
	// start again. The one order it stranded stays stranded forever, and the
	// operator sees an unexplained crash loop instead of this message.
	//
	// The fix at the call site is bus.WithTenantID(ctx, <the OMS's own
	// tenant>) — cmd/oms/main.go does this for the real startup sweep.
	if bus.TenantIDFromContext(ctx) == "" {
		return 0, errors.New("oms: SweepInterrupted requires a tenant on ctx " +
			"(bus.WithTenantID) — it re-publishes the lifecycle of every order " +
			"it redrives, and it runs before any bus delivery, so unlike a " +
			"handler it has no inbound envelope to inherit a tenant from; " +
			"without one the first redrive fails envelope validation and, " +
			"because a sweep failure is fatal at startup, the OMS never starts")
	}

	// DELIBERATELY NOT FILLED/REJECTED/CANCELLED/EXPIRED, even though a FILLED or
	// REJECTED order can now be terminal-but-unannounced (outcome_announced_at
	// unset — see order_events.proto:19 and resume()'s terminal branch). resume()
	// reaches that case safely because it is only ever invoked for a SPECIFIC
	// order_id a SubjectSubmit redelivery names, which the broker only redelivers
	// within its own bounded retry/DLQ window. ListByStatus has no such bound: it
	// is every order in that status, ever. outcome_announced_at is an ADDITIVE
	// field, so every order Saved before this field existed reads back with it
	// unset — indistinguishable, by that column alone, from a genuine
	// interruption. Selecting FILLED/REJECTED here would treat the fund's entire
	// pre-migration order history as newly-interrupted and re-announce a
	// CommandOutcome for all of it on every single startup. That is a much worse
	// failure than the one this field fixes, so the sweep continues to skip
	// every terminal status; a terminal-but-unannounced order whose SubmitOrder
	// command has stopped being redelivered (retries exhausted, past the DLQ
	// window) is a known, residual gap this fix does not close.
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
