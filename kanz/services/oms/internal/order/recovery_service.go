package order

import (
	"context"
	"errors"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/fillfact"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/proto"
)

// HandleRecovery treats an aggregate discrepancy as an investigation trigger.
// Only the adapter's complete execution history may change the financial books;
// this path has no call to Execute, CancelOrder, or the order resumption driver.
func (s *Service) HandleRecovery(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	if err := bus.RequireTenantScope(env.GetTenantId(), s.tenant); err != nil {
		return err
	}
	if err := fillfact.RequireRecoveryTenant(env, s.tenant); err != nil {
		return err
	}
	if env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT || env.GetEventType() != execution.SubjectStateHealed || len(payload) == 0 || len(payload) > 4<<20 {
		return errors.New("oms: invalid execution recovery observation")
	}
	var observation orderpb.StateHealed
	if err := proto.Unmarshal(payload, &observation); err != nil {
		return err
	}
	if !recoveryText(observation.GetOrderId(), 256) || !recoveryText(observation.GetVenue(), 256) {
		return errors.New("oms: recovery observation lacks order or venue")
	}
	store, ok := s.store.(*Postgres)
	if !ok {
		return errors.New("oms: execution recovery requires durable evidence storage")
	}
	ctx, release, err := s.awaitClaim(ctx, observation.GetOrderId())
	if err != nil {
		return err
	}
	defer release()
	c, err := store.ObserveRecovery(ctx, env.GetEventId(), observation.GetOrderId(), env.GetEventId(), payload)
	if err != nil {
		return err
	}
	for range 4 {
		if c.Status == "corrected" || c.Status == "blocked" || c.Checkpoint > 0 {
			_, err = s.relay.Flush(ctx, c.OrderID)
			return err
		}
		st, _, err := store.Load(ctx, c.OrderID)
		if errors.Is(err, ErrNotFound) {
			return s.blockRecovery(ctx, store, c, &orderpb.OrderState{OrderId: c.OrderID, Venue: observation.GetVenue()}, "observed order is not attributable to this tenant's OMS")
		}
		if err != nil {
			return err
		}
		observedState := observation.GetState()
		if observation.GetVenue() != st.GetVenue() || (observedState.GetOrderId() != "" && observedState.GetOrderId() != st.GetOrderId()) || (observedState.GetVenueAccountId() != "" && observedState.GetVenueAccountId() != st.GetVenueAccountId()) {
			return s.blockRecovery(ctx, store, c, st, "observation conflicts with the persisted order attribution")
		}
		if c.Status == "observed" {
			account, bound := s.accounts.Account(s.tenant, st.GetPortfolioId(), st.GetVenue())
			if !bound || account != st.GetVenueAccountId() || s.router == nil {
				return s.blockRecovery(ctx, store, c, st, "no explicit portfolio-to-venue-account binding matches the order")
			}
			venue, routeErr := s.router.Route(st)
			if routeErr != nil {
				return s.blockRecovery(ctx, store, c, st, "the attributed venue account is unavailable to recovery")
			}
			querier, queries := venue.(execution.Querier)
			describer, describes := venue.(interface {
				Describe(context.Context) (execution.VenueIdentity, error)
			})
			if !queries || !describes {
				return s.blockRecovery(ctx, store, c, st, "venue cannot provide both execution history and verified account identity")
			}
			identity, err := describer.Describe(ctx)
			if err != nil {
				return err
			}
			if identity.MIC != st.GetVenue() || identity.Account != account || !identity.Proof.Verified || identity.Proof.ExchangeAccountID == "" {
				return s.blockRecovery(ctx, store, c, st, "venue account proof does not match the persisted order attribution")
			}
			mapping, err := store.RecordRecoveryMapping(ctx, RecoveryMapping{s.tenant, st.GetPortfolioId(), st.GetVenue(), account, identity.Proof.ExchangeAccountID}, identity.Proof)
			if err != nil {
				return err
			}
			view, err := querier.QueryOrder(ctx, st)
			if err != nil {
				return err
			}
			if _, err := execution.CompleteHistory(st, view); err != nil {
				return s.blockRecovery(ctx, store, c, st, "venue execution history is incomplete or inconsistent with the independent total")
			}
			err = store.FreezeRecoveryHistory(ctx, c, mapping, st, view, s.now().UTC())
			if err != nil && !errors.Is(err, ErrConflict) {
				return err
			}
		} else {
			err = store.CommitRecovery(ctx, c, s.now().UTC())
			if err == nil || errors.Is(err, ErrRecoveryBlocked) {
				_, err = s.relay.Flush(ctx, c.OrderID)
				return err
			}
			if errors.Is(err, ErrPostedExecutionUnknown) || errors.Is(err, fillfact.ErrExecutionIdentityConflict) || errors.Is(err, fillfact.ErrFeeRevision) || errors.Is(err, execution.ErrHistoryIncomplete) {
				return s.blockRecovery(ctx, store, c, st, "booked execution evidence is incomplete or differs from venue history; an authorized correction is required")
			}
			if !errors.Is(err, ErrConflict) {
				return err
			}
		}
		c, err = store.RecoveryCase(ctx, c.ID)
		if err != nil {
			return err
		}
	}
	return ErrConflict
}

func (s *Service) blockRecovery(ctx context.Context, store *Postgres, c RecoveryCase, st *orderpb.OrderState, reason string) error {
	if err := store.BlockRecovery(ctx, c, st, reason, s.now().UTC()); err != nil {
		return err
	}
	_, err := s.relay.Flush(ctx, c.OrderID)
	return err
}

// HandleRecoveryAck accepts separate subjects granted only to the respective
// book publisher. A caller cannot choose which book its acknowledgement proves.
func (s *Service) HandleRecoveryAck(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	if err := bus.RequireTenantScope(env.GetTenantId(), s.tenant); err != nil {
		return err
	}
	if err := fillfact.RequireRecoveryTenant(env, s.tenant); err != nil {
		return err
	}
	if env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT || len(payload) > 4096 {
		return errors.New("oms: invalid recovery acknowledgement envelope")
	}
	var book string
	switch env.GetEventType() {
	case fillfact.RecoveryPositionApplied:
		book = "position"
	case fillfact.RecoveryLedgerApplied:
		book = "ledger"
	default:
		return errors.New("oms: unknown recovery acknowledgement type")
	}
	var ack orderpb.ExecutionRecoveryApplied
	if err := proto.Unmarshal(payload, &ack); err != nil {
		return err
	}
	store, ok := s.store.(*Postgres)
	if !ok {
		return errors.New("oms: execution recovery requires durable evidence storage")
	}
	if err := store.AcknowledgeRecovery(ctx, &ack, book, s.now().UTC()); err != nil {
		return err
	}
	_, err := s.relay.Flush(ctx, ack.GetOrderId())
	return err
}

// HandleRecoveryParked consumes only the recovery execution DLQ. The original
// evidence identifies the target; a delivery failure never implies that either
// book committed. Subsequent successful book acknowledgements can resolve it.
func (s *Service) HandleRecoveryParked(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	if err := bus.RequireTenantScope(env.GetTenantId(), s.tenant); err != nil {
		return err
	}
	if err := fillfact.RequireRecoveryTenant(env, s.tenant); err != nil {
		return err
	}
	if env.GetEventType() != fillfact.SubjectRecovered || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT {
		return errors.New("oms: invalid parked recovery fact")
	}
	fill, _, err := fillfact.DecodeRecovery(payload)
	if err != nil {
		return err
	}
	store, ok := s.store.(*Postgres)
	if !ok {
		return errors.New("oms: recovery failure requires durable evidence storage")
	}
	for range 4 {
		c, err := store.RecoveryCase(ctx, fill.GetRecovery().GetCaseId())
		if err != nil {
			return err
		}
		var matches bool
		if err := store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM execution_recovery_targets WHERE case_id=$1 AND execution_key=$2 AND payload_digest=$3 AND execution_digest=$4)`, c.ID, fillfact.ExecutionKey(fill), fill.GetRecovery().GetPayloadDigest(), fill.GetRecovery().GetExecutionDigest()).Scan(&matches); err != nil {
			return err
		}
		if !matches || c.OrderID != fill.GetOrderId() {
			return ErrRecoveryEvidenceConflict
		}
		if c.Status == "corrected" || c.Status == "blocked" {
			_, err = s.relay.Flush(ctx, c.OrderID)
			return err
		}
		st, _, err := store.Load(ctx, c.OrderID)
		if err != nil {
			return err
		}
		err = s.blockRecovery(ctx, store, c, st, "a recovered execution delivery was parked; book acknowledgements remain incomplete")
		if !errors.Is(err, ErrConflict) {
			return err
		}
	}
	return ErrConflict
}
