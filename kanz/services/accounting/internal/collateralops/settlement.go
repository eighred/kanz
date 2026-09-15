package collateralops

import (
	"context"
	"errors"
	"github.com/eighred/kanz/internal/outbox"
	"math/big"
	"time"

	pb "github.com/eighred/kanz/kanz-schemas-go/collateral/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Confirm receives only authoritative custody FACTs from the scoped consumer.
// Cumulative quantities make partial and out-of-order reports deterministic.
// No inventory is freed until every return leg is fully custody-confirmed.
func (s *Store) Confirm(ctx context.Context, tenant string, c *pb.PostingConfirmation) error {
	if c == nil || !identifier(c.SourceId) || !identifier(c.WorkflowId) || !identifier(c.AgreementId) || !identifier(c.LotId) || !identifier(c.CustodianId) || !identifier(c.AccountId) || c.SettledAt == nil || c.SettledAt.CheckValid() != nil || c.InstructionRevision <= 0 {
		return ErrInput
	}
	if _, err := exact(c.CumulativeQuantity, false); err != nil {
		return err
	}
	if c.SettledAt.AsTime().After(time.Now().UTC()) {
		return ErrInput
	}
	blob, err := marshal(c)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = lock(ctx, tx, tenant); err != nil {
		return err
	}
	var prior string
	err = tx.QueryRow(ctx, `SELECT digest FROM collateral_confirmations WHERE source_id=$1`, c.SourceId).Scan(&prior)
	if err == nil {
		if prior != digest(blob) {
			return ErrConflict
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	state, err := loadWorkflow(ctx, tx, c.WorkflowId)
	if err != nil {
		return err
	}
	status := state.Status
	if status == pb.WorkflowStatus_WORKFLOW_STATUS_DISPUTED {
		status = state.DisputedStatus
	}
	revision := state.PostingRevision
	instructed := state.PostingInstructedAt
	if c.Returned {
		if status != pb.WorkflowStatus_WORKFLOW_STATUS_RETURN_INSTRUCTED && status != pb.WorkflowStatus_WORKFLOW_STATUS_RELEASED {
			return ErrTransition
		}
		revision = state.ReturnRevision
		instructed = state.ReturnInstructedAt
	} else if status != pb.WorkflowStatus_WORKFLOW_STATUS_INSTRUCTED && status != pb.WorkflowStatus_WORKFLOW_STATUS_SETTLED && status != pb.WorkflowStatus_WORKFLOW_STATUS_RETURN_PROPOSED && status != pb.WorkflowStatus_WORKFLOW_STATUS_RETURN_INSTRUCTED && status != pb.WorkflowStatus_WORKFLOW_STATUS_RELEASED {
		return ErrTransition
	}
	if revision != c.InstructionRevision || instructed == nil || c.SettledAt.AsTime().Before(instructed.AsTime()) {
		return ErrConflict
	}
	matched := false
	for _, leg := range state.Legs {
		if leg.AgreementId == c.AgreementId && leg.LotId == c.LotId {
			if leg.CustodianId != c.CustodianId || leg.AccountId != c.AccountId || rat(c.CumulativeQuantity).Cmp(rat(leg.Quantity)) > 0 {
				return ErrConflict
			}
			matched = true
		}
	}
	if !matched {
		return ErrInput
	}
	if _, err = tx.Exec(ctx, `INSERT INTO collateral_confirmations(source_id,workflow_id,agreement_id,lot_id,returned,digest,payload) VALUES($1,$2,$3,$4,$5,$6,$7)`, c.SourceId, c.WorkflowId, c.AgreementId, c.LotId, c.Returned, digest(blob), blob); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT payload FROM collateral_confirmations WHERE workflow_id=$1 AND returned=$2`, c.WorkflowId, c.Returned)
	if err != nil {
		return err
	}
	maximum := map[[2]string]*big.Rat{}
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			break
		}
		report := new(pb.PostingConfirmation)
		if err = proto.Unmarshal(b, report); err != nil {
			break
		}
		key := [2]string{report.AgreementId, report.LotId}
		value := rat(report.CumulativeQuantity)
		if key == [2]string{c.AgreementId, c.LotId} {
			quantityOrder := rat(c.CumulativeQuantity).Cmp(value)
			timeOrder := c.SettledAt.AsTime().Compare(report.SettledAt.AsTime())
			if (timeOrder > 0 && quantityOrder < 0) || (timeOrder < 0 && quantityOrder > 0) || (timeOrder == 0 && quantityOrder != 0) {
				err = ErrConflict
				break
			}
		}
		if old := maximum[key]; old == nil || value.Cmp(old) > 0 {
			maximum[key] = value
		}
	}
	rows.Close()
	if err != nil {
		return err
	}
	if err = rows.Err(); err != nil {
		return err
	}
	complete := true
	for _, leg := range state.Legs {
		q := maximum[[2]string{leg.AgreementId, leg.LotId}]
		if q == nil || q.Cmp(rat(leg.Quantity)) != 0 {
			complete = false
		}
	}
	if complete {
		if c.Returned {
			state.Status = pb.WorkflowStatus_WORKFLOW_STATUS_RELEASED
			if _, err = tx.Exec(ctx, `DELETE FROM collateral_reservations WHERE workflow_id=$1`, c.WorkflowId); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `DELETE FROM collateral_active_agreements WHERE workflow_id=$1`, c.WorkflowId); err != nil {
				return err
			}
		} else if status == pb.WorkflowStatus_WORKFLOW_STATUS_INSTRUCTED {
			if state.Status == pb.WorkflowStatus_WORKFLOW_STATUS_DISPUTED {
				state.DisputedStatus = pb.WorkflowStatus_WORKFLOW_STATUS_SETTLED
			} else {
				state.Status = pb.WorkflowStatus_WORKFLOW_STATUS_SETTLED
			}
		}
	}
	state.Revision++
	state.Actor = "custodian:" + c.CustodianId
	state.RequestId = c.SourceId
	state.RecordedAt = timestamppb.New(time.Now().UTC().Truncate(time.Microsecond))
	fact, err := record(ctx, tx, tenant, state)
	if err != nil {
		return err
	}
	if err = outbox.Enqueue(ctx, tx, fact); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
