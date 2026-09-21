package order

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/internal/outbox"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	SubjectFeePropose  = "order.order.propose_fee_correction"
	SubjectFeeApprove  = "order.order.approve_fee_correction"
	SubjectFeeProposed = "order.order.fee_correction_proposed"
	SubjectFeeApproved = "order.order.fee_correction_approved"
)

// FeeProposal exposes immutable review terms. Authorization belongs to the
// authenticated query boundary; neither actor nor new economics come from a
// client-supplied proposal body.
func (p *Postgres) FeeProposal(ctx context.Context, caseID string) (*orderpb.ExecutionFeeCorrectionProposed, error) {
	var data []byte
	var digest string
	err := p.pool.QueryRow(ctx, `SELECT proposal,digest FROM execution_fee_proposals WHERE case_id=$1`, caseID).Scan(&data, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var proposal orderpb.ExecutionFeeCorrectionProposed
	if err := proto.Unmarshal(data, &proposal); err != nil {
		return nil, err
	}
	actual, err := fillfact.FeeProposalDigest(bus.TenantIDFromContext(ctx), &proposal)
	if err != nil || actual != digest || proposal.Digest != digest || proposal.CaseId != caseID {
		return nil, fillfact.ErrFeeRevision
	}
	return &proposal, nil
}

func (p *Postgres) ProposeFeeCorrection(ctx context.Context, c RecoveryCase, actor, reason string, now time.Time) error {
	if !recoveryText(actor, 256) || !recoveryText(reason, 2048) || now.Unix() <= 0 {
		return fillfact.ErrFeeRevision
	}
	if old, err := p.FeeProposal(ctx, c.ID); err == nil {
		if old.Proposer != actor || old.Reason != reason || old.OrderId != c.OrderID {
			return ErrRecoveryEvidenceConflict
		}
		return nil
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	if c.Checkpoint != 0 || (c.Status != "blocked" && c.Status != "investigating") {
		return fillfact.ErrFeeRevision
	}
	history, historyDigest, err := p.RecoveryHistory(ctx, c.ID)
	if err != nil {
		return err
	}
	st, version, err := p.Load(ctx, c.OrderID)
	if err != nil {
		return err
	}
	posted, err := p.postedRecoveryExecutions(ctx, st)
	if err != nil {
		return err
	}
	observed := make(map[string]*orderpb.Fill, len(history.Fills))
	for _, f := range history.Fills {
		observed[fillfact.ExecutionKey(f)] = f
	}
	proposal := &orderpb.ExecutionFeeCorrectionProposed{CaseId: c.ID, OrderId: c.OrderID, PortfolioId: st.PortfolioId, MappingVersion: c.MappingVersion, HistoryDigest: historyDigest, Proposer: actor, ProposedAt: timestamppb.New(now.UTC()), ExpiresAt: timestamppb.New(now.UTC().Add(dualcontrol.DefaultTTL)), Reason: reason}
	for key, previous := range posted {
		revised := observed[key]
		if !fillfact.SameExecutionExceptFee(previous, revised) {
			return fillfact.ErrFeeRevision
		}
		if !fillfact.SameExecution(previous, revised) {
			proposal.Changes = append(proposal.Changes, &orderpb.ExecutionFeeChange{Previous: previous, Revised: revised})
		}
	}
	if len(proposal.Changes) == 0 {
		return fillfact.ErrFeeRevision
	}
	sort.Slice(proposal.Changes, func(i, j int) bool {
		return fillfact.ExecutionKey(proposal.Changes[i].Previous) < fillfact.ExecutionKey(proposal.Changes[j].Previous)
	})
	proposal.Digest, err = fillfact.FeeProposalDigest(bus.TenantIDFromContext(ctx), proposal)
	if err != nil {
		return err
	}
	if _, err := feeControlProposal(proposal); err != nil {
		return err
	}
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(proposal)
	if err != nil {
		return err
	}
	if len(data) > 512<<10 {
		return errors.New("oms: fee proposal exceeds bounded review payload")
	}
	var emitter Emitter
	record, err := outbox.From(ctx, emitter.event(SubjectFeeProposed, c.OrderID, now, proposal))
	if err != nil {
		return err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	state, err := proto.Marshal(st)
	if err != nil {
		return err
	}
	if err := p.cas(ctx, tx, st, state, version); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO execution_fee_proposals(case_id,order_id,digest,proposal) VALUES($1,$2,$3,$4)`, c.ID, c.OrderID, proposal.Digest, data); err != nil {
		return err
	}
	if err := outbox.Enqueue(ctx, tx, record); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func feeControlProposal(p *orderpb.ExecutionFeeCorrectionProposed) (dualcontrol.Proposal, error) {
	if p.GetProposedAt() == nil || p.GetExpiresAt() == nil || p.ProposedAt.CheckValid() != nil || p.ExpiresAt.CheckValid() != nil {
		return dualcontrol.Proposal{}, fillfact.ErrFeeRevision
	}
	return dualcontrol.Propose(p.CaseId, dualcontrol.ActExecutionFeeCorrection, p.OrderId, p.Proposer, p.Digest, p.ProposedAt.AsTime(), p.ExpiresAt.AsTime().Sub(p.ProposedAt.AsTime()))
}

func (p *Postgres) ApproveFeeCorrection(ctx context.Context, c RecoveryCase, actor, digest string, now time.Time) error {
	proposal, err := p.FeeProposal(ctx, c.ID)
	if err != nil {
		return err
	}
	control, err := feeControlProposal(proposal)
	if err != nil {
		return err
	}
	if proposal.OrderId != c.OrderID || !recoveryText(strings.TrimSpace(actor), 256) || len(actor) > 256 || now.Before(control.CreatedAt) {
		return fillfact.ErrFeeRevision
	}
	// A committed signature remains committed when its command redelivers after
	// the proposal deadline. Validate its original signing time, not retry time.
	var saved []byte
	err = p.pool.QueryRow(ctx, `SELECT approval FROM execution_fee_approvals WHERE case_id=$1`, c.ID).Scan(&saved)
	if err == nil {
		var old orderpb.ExecutionFeeCorrectionApproved
		if err := proto.Unmarshal(saved, &old); err != nil {
			return err
		}
		if old.Digest != digest || !dualcontrol.SameSubject(old.Approver, actor) || old.GetApprovedAt() == nil || old.ApprovedAt.CheckValid() != nil {
			return ErrRecoveryEvidenceConflict
		}
		_, err := control.Approve(old.Approver, old.Digest, old.ApprovedAt.AsTime())
		return err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	approval, err := control.Approve(actor, digest, now)
	if err != nil {
		return err
	}
	if err := approval.Covers(dualcontrol.ActExecutionFeeCorrection, proposal.Digest); err != nil {
		return err
	}
	approved := &orderpb.ExecutionFeeCorrectionApproved{CaseId: c.ID, OrderId: c.OrderID, PortfolioId: proposal.PortfolioId, Digest: approval.Digest(), Proposer: approval.Proposer(), Approver: approval.Approver(), ApprovedAt: timestamppb.New(approval.ApprovedAt())}
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(approved)
	if err != nil {
		return err
	}
	var emitter Emitter
	record, err := outbox.From(ctx, emitter.event(SubjectFeeApproved, c.OrderID, now, approved))
	if err != nil {
		return err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var previous []byte
	err = tx.QueryRow(ctx, `SELECT approval FROM execution_fee_approvals WHERE case_id=$1`, c.ID).Scan(&previous)
	if err == nil {
		var old orderpb.ExecutionFeeCorrectionApproved
		if err := proto.Unmarshal(previous, &old); err != nil {
			return err
		}
		if old.Digest != digest || !dualcontrol.SameSubject(old.Approver, actor) {
			return ErrRecoveryEvidenceConflict
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if c.Checkpoint != 0 || (c.Status != "blocked" && c.Status != "investigating") || proposal.OrderId != c.OrderID {
		return ErrConflict
	}
	tag, err := tx.Exec(ctx, `UPDATE execution_recovery_cases SET status='investigating',reason='',version=version+1,updated_at=$1 WHERE case_id=$2 AND version=$3 AND checkpoint=0 AND status IN ('blocked','investigating')`, now.UTC(), c.ID, c.Version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, `INSERT INTO execution_fee_approvals(case_id,approval) VALUES($1,$2)`, c.ID, data); err != nil {
		return err
	}
	if err := outbox.Enqueue(ctx, tx, record); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// approvedFeeChanges revalidates immutable approval evidence at application,
// including the exact current baseline. A newer correction invalidates a stale
// proposal even when its second signature was valid when collected.
func (p *Postgres) approvedFeeChanges(ctx context.Context, c RecoveryCase, historyDigest string, posted map[string]*orderpb.Fill) (map[string]*orderpb.ExecutionFeeApproval, error) {
	var data []byte
	err := p.pool.QueryRow(ctx, `SELECT approval FROM execution_fee_approvals WHERE case_id=$1`, c.ID).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var approved orderpb.ExecutionFeeCorrectionApproved
	if err := proto.Unmarshal(data, &approved); err != nil {
		return nil, err
	}
	proposal, err := p.FeeProposal(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	control, err := feeControlProposal(proposal)
	if err != nil {
		return nil, err
	}
	if approved.GetApprovedAt() == nil || approved.ApprovedAt.CheckValid() != nil || approved.Proposer != proposal.Proposer || approved.OrderId != c.OrderID || approved.CaseId != c.ID || approved.PortfolioId != proposal.PortfolioId || proposal.HistoryDigest != historyDigest || proposal.MappingVersion != c.MappingVersion {
		return nil, fillfact.ErrFeeRevision
	}
	if _, err := control.Approve(approved.Approver, approved.Digest, approved.ApprovedAt.AsTime()); err != nil {
		return nil, err
	}
	result := make(map[string]*orderpb.ExecutionFeeApproval, len(proposal.Changes))
	for _, change := range proposal.Changes {
		key := fillfact.ExecutionKey(change.Previous)
		if !fillfact.SameExecution(posted[key], change.Previous) || !fillfact.SameExecutionExceptFee(change.Previous, change.Revised) {
			return nil, fillfact.ErrFeeRevision
		}
		digest, err := fillfact.EconomicDigest(change.Previous)
		if err != nil {
			return nil, err
		}
		result[key] = &orderpb.ExecutionFeeApproval{ProposalId: c.ID, Digest: approved.Digest, Proposer: approved.Proposer, Approver: approved.Approver, ApprovedAt: approved.ApprovedAt, PreviousExecutionDigest: digest, PreviousFee: change.Previous.Fee}
	}
	return result, nil
}
