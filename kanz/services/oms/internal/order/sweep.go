package order

import (
	"context"
	"errors"
	"fmt"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/pkg/bus"
)

// SweepInterrupted reconciles every order this OMS left mid-flight. It is the
// STARTUP sweep, and it must run BEFORE the consumers subscribe.
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
//
// WHAT "BEFORE THE CONSUMERS SUBSCRIBE" DOES AND DOES NOT MEAN (#238). It is a
// COMPLETENESS requirement on startup, not an exclusion mechanism: this pod must
// account for what its predecessor left before it admits anything new, because
// admitting orders onto a book it cannot account for is trading blind. It is NOT
// the thing that keeps a sweep from colliding with a live handler — that job
// belongs to the per-order claim (orderlock.go), which resume() has always
// taken, and to Store.Save's version predicate across pods (#122). Both work
// under live consumption; neither depends on the subscriptions being idle. So
// running an ADDITIONAL sweep later, concurrently with live traffic, does not
// weaken this one — see SweepOlderThan, which exists for exactly that and
// carries the one extra precaution concurrency does require.
func (s *Service) SweepInterrupted(ctx context.Context) (int, error) {
	return s.sweep(ctx, 0)
}

// SweepOlderThan is SweepInterrupted restricted to orders admitted longer ago
// than minAge, for running on a ticker ALONGSIDE live consumption.
//
// WHY A PERIODIC SWEEP AT ALL. Admission is two independent writes with no
// outbox between them — store.Create then EmitAccepted — and when the publish
// fails the row is durable and no downstream service has heard of the order
// (#238). The startup sweep is the compensator, so the recovery latency was
// "whenever this pod next restarts", which on a healthy deployment is days. This
// turns that into one interval. It does not remove the divergence; a
// transactional outbox does, and that remains the real fix.
//
// WHY minAge, AND WHY IT IS NOT OPTIONAL. handleSubmit's own window between
// store.Create and s.claim spans EmitAccepted — a network publish with a 5s
// timeout — and during it the order EXISTS with NO CLAIM HELD. A sweep that
// looked at orders of any age could take the claim inside that window and drive
// an order a live delivery is still admitting. The per-order lock keeps that from
// becoming a double trade (the live delivery finds the claim held and stops), but
// the two would still both publish an ACCEPTED FACT, and a consumer folding the
// live one AFTER the sweep's would walk its view of the order backwards to a
// pre-routing snapshot. minAge removes the case rather than racing it: an order
// admitted seconds ago is being worked by something, and the compensator is for
// orders nothing is working.
//
// It must therefore be set longer than every recovery that could still be
// running against a young order: JetStream's AckWait on order subjects
// (workAckWait, 60s, pkg/bus/tuning.go), which bounds how long the broker waits
// before redelivering; and the in-process dedup claim lease derived from it
// (dedupClaimLease = maxTunedAckWait + 15s = 75s, pkg/bus/dedup.go), which
// bounds how long a dispatch can hold an idempotency key. The deployed default
// is 2m — 60% headroom over the longer of the two — which also happens to match
// defaultDedupTTL, though that constant governs kanz-redrive's own --min-age
// (bus.DefaultMinAge is defaultDedupTTL + margin) rather than this one. See
// OMS_SWEEP_MIN_AGE for the operator override.
//
// A zero or negative minAge is refused rather than treated as "no filter",
// because the value that disables the precaution must not be the value somebody
// gets by leaving a config field unset.
//
// WHAT IT COSTS PER TICK: one index scan over the open orders
// (orders_status_idx), plus one store.Load per order older than minAge, because
// resume() re-reads under the claim. An order RESTING at PENDING_NEW in a
// deployment with no venue wired stays open forever and is therefore reloaded on
// every tick, doing nothing. That is the bound an operator with a large resting
// book must size the interval against.
func (s *Service) SweepOlderThan(ctx context.Context, minAge time.Duration) (int, error) {
	if minAge <= 0 {
		return 0, fmt.Errorf("oms: a periodic sweep needs a positive minimum age (got %v) — "+
			"without one it can claim an order a live delivery is still admitting, and both "+
			"would announce it", minAge)
	}
	return s.sweep(ctx, minAge)
}

// sweep is the one implementation behind both entry points. minAge <= 0 means
// "every open order", which is only ever correct at startup, when nothing else
// in this process is touching an order.
func (s *Service) sweep(ctx context.Context, minAge time.Duration) (int, error) {
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
	// Collected, not returned on sight — see the periodic branch below. Always
	// empty on a startup sweep, which still aborts on the first failure.
	var failures []error
	// One clock read for the whole pass, not one per order: a cutoff that crept
	// forward mid-scan would include or exclude orders depending on where they
	// happened to sort, which is not a decision anybody could reproduce.
	var cutoff time.Time
	if minAge > 0 {
		cutoff = s.now().UTC().Add(-minAge)
	}
	for _, st := range open {
		if st.GetQuarantine() != nil {
			continue // already frozen; a human owns it
		}
		// TOO YOUNG TO BE INTERRUPTED. Filtered here, on the state ListByStatus
		// already returned, so skipping costs nothing — no claim, no Load, no
		// counted sweep. as_of is the order's admission time until something moves
		// it, which is precisely the clock this bounds. An order with no as_of at
		// all is not skipped: it cannot be shown to be young, and a compensator
		// that ignored an order because a timestamp was missing would be the
		// silent failure this whole path exists to end.
		if minAge > 0 && st.GetAsOf() != nil && st.GetAsOf().AsTime().UTC().After(cutoff) {
			continue
		}
		if err := s.resume(ctx, st); err != nil {
			err = fmt.Errorf("oms: could not reconcile in-flight order %s: %w", st.GetOrderId(), err)
			if minAge <= 0 {
				// STARTUP. One order that cannot be reconciled stops the sweep. The
				// alternative is a pod that starts having silently skipped an order it
				// could not account for, which is the failure this whole path exists
				// to end. The caller treats this as fatal.
				return swept, err
			}
			// PERIODIC, AND THE OPPOSITE ANSWER — deliberately, because the same
			// stance here would defeat the sweep entirely. ListByStatus returns
			// ORDER BY order_id, so one order that fails every time (a venue that
			// will not answer, a store row that will not decode) would abort the
			// pass at the same point on every tick and permanently shadow every
			// order sorting after it. Nothing would say so: the pod stays up, the
			// sweep "runs", and the orders behind the poison one are never looked at
			// again. So a running pod records the failure and carries on to the rest.
			//
			// It is still loud. Every failure is logged at ERROR here, the joined
			// error goes back to the caller, and cmd/oms/main.go counts it — a
			// periodic sweep that has been failing is an alert (OMSSweepFailing), not
			// a debug line.
			s.logger.Error("oms: could not reconcile one in-flight order during a periodic sweep — "+
				"continuing with the rest of the book so one stuck order cannot shadow every order behind it",
				"order_id", st.GetOrderId(), "err", err)
			failures = append(failures, err)
			continue
		}
		swept++
	}
	// errors.Join of an empty slice is nil, so a clean pass returns nil and a
	// startup sweep — which never appends here — is unchanged.
	return swept, errors.Join(failures...)
}
