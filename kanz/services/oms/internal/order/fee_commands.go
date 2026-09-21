package order

import (
	"context"
	"errors"
	"strings"

	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/internal/fillfact"
	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/proto"
)

func (s *Service) HandleFeeCorrection(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	if err := bus.RequireTenantScope(env.GetTenantId(), s.tenant); err != nil {
		return err
	}
	if err := fillfact.RequireRecoveryTenant(env, s.tenant); err != nil {
		return err
	}
	if env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_COMMAND || len(payload) == 0 || len(payload) > 65536 {
		return errors.New("oms: invalid fee correction command")
	}
	var metadata *commandpb.CommandMetadata
	var orderID, caseID, reason, digest string
	switch env.GetEventType() {
	case SubjectFeePropose:
		var cmd orderpb.ProposeExecutionFeeCorrection
		if err := proto.Unmarshal(payload, &cmd); err != nil {
			return err
		}
		metadata, orderID, caseID, reason = cmd.Metadata, cmd.OrderId, cmd.CaseId, cmd.Reason
	case SubjectFeeApprove:
		var cmd orderpb.ApproveExecutionFeeCorrection
		if err := proto.Unmarshal(payload, &cmd); err != nil {
			return err
		}
		metadata, orderID, caseID, digest = cmd.Metadata, cmd.OrderId, cmd.CaseId, cmd.Digest
	default:
		return errors.New("oms: unknown fee correction command")
	}
	if !recoveryText(orderID, 256) || !recoveryText(caseID, 256) || metadata.GetTargetId() != orderID || !strings.HasPrefix(metadata.GetIssuer(), delegatedIssuerPrefix) || !recoveryText(strings.TrimPrefix(metadata.GetIssuer(), delegatedIssuerPrefix), 251) {
		return errors.New("oms: fee correction requires an authenticated human and exact target")
	}
	store, ok := s.store.(*Postgres)
	if !ok {
		return errors.New("oms: fee correction requires durable evidence")
	}
	ctx, release, err := s.awaitClaim(ctx, orderID)
	if err != nil {
		return err
	}
	defer release()
	st, _, err := store.Load(ctx, orderID)
	if err != nil {
		return err
	}
	if !entitledTo(metadata.GetPrincipalPortfolios(), st.GetPortfolioId()) {
		return errors.New("oms: fee correction portfolio is not authorized")
	}
	c, err := store.RecoveryCase(ctx, caseID)
	if err != nil {
		return err
	}
	if c.OrderID != orderID {
		return ErrRecoveryEvidenceConflict
	}
	if env.GetEventType() == SubjectFeePropose {
		err = store.ProposeFeeCorrection(ctx, c, metadata.GetIssuer(), reason, s.now().UTC())
	} else {
		err = store.ApproveFeeCorrection(ctx, c, metadata.GetIssuer(), digest, s.now().UTC())
		if c.Checkpoint == 0 && c.Status != "corrected" {
			reason := ""
			switch {
			case errors.Is(err, dualcontrol.ErrSelfApproval):
				reason = "fee approval refused: proposer and approver must be different people"
			case errors.Is(err, dualcontrol.ErrExpired):
				reason = "fee approval refused: proposal expired; start a new investigation"
			case errors.Is(err, dualcontrol.ErrPayloadChanged):
				reason = "fee approval refused: digest does not match the proposed changes"
			}
			if reason != "" {
				return s.blockRecovery(ctx, store, c, st, reason)
			}
		}
		if err == nil {
			c, err = store.RecoveryCase(ctx, caseID)
			if err == nil && c.Status == "investigating" && c.Checkpoint == 0 {
				err = store.CommitRecovery(ctx, c, s.now().UTC())
				if errors.Is(err, fillfact.ErrFeeRevision) || errors.Is(err, fillfact.ErrExecutionIdentityConflict) || errors.Is(err, ErrPostedExecutionUnknown) {
					return s.blockRecovery(ctx, store, c, st, "approved fee correction no longer matches the booked execution; a new investigation is required")
				}
			}
		}
	}
	if err != nil {
		return err
	}
	_, err = s.relay.Flush(ctx, orderID)
	return err
}
