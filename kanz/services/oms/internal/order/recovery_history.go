package order

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/outbox"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const EventTypeRecoveryRecorded = "order.order.recovery_recorded"

// FreezeRecoveryHistory makes the query result durable before a financial write.
// Restart resumes these exact bytes, not a fresh query whose economics may have
// changed. A concurrent investigator may win; the loser must reload that history.
func (p *Postgres) FreezeRecoveryHistory(ctx context.Context, c RecoveryCase, mappingVersion string, st *orderpb.OrderState, view execution.OrderView, observed time.Time) error {
	if c.OrderID != st.GetOrderId() || c.ID == "" || !recoveryText(mappingVersion, 256) || observed.IsZero() {
		return errors.New("oms: invalid recovery history scope")
	}
	fills, err := execution.CompleteHistory(st, view)
	if err != nil {
		return err
	}
	history := &orderpb.ExecutionRecoveryHistory{OrderId: st.GetOrderId(), MappingVersion: mappingVersion,
		Fills: fills, ExecutedQuantity: view.ExecutedQuantity, ObservedAt: timestamppb.New(observed.UTC())}
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(history)
	if err != nil {
		return err
	}
	if len(data) > 4<<20 {
		return errors.New("oms: recovery history exceeds storage bound")
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var matched bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM execution_account_mappings
		WHERE version=$1 AND portfolio_id=$2 AND venue=$3 AND venue_account_id=$4)`,
		mappingVersion, st.GetPortfolioId(), st.GetVenue(), st.GetVenueAccountId()).Scan(&matched)
	if err != nil {
		return err
	}
	if !matched {
		return errors.New("oms: recovery history has no matching proven account mapping")
	}
	tag, err := tx.Exec(ctx, `UPDATE execution_recovery_cases
		SET mapping_version=$1, status='investigating', reason='', version=version+1, updated_at=$2
		WHERE case_id=$3 AND order_id=$4 AND version=$5 AND status IN ('observed','blocked')`,
		mappingVersion, observed.UTC(), c.ID, c.OrderID, c.Version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	_, err = tx.Exec(ctx, `INSERT INTO execution_recovery_history (case_id, mapping_version, payload_digest, history)
		VALUES ($1,$2,$3,$4)`, c.ID, mappingVersion, recoveryDigest(data), data)
	if err != nil {
		return err
	}
	c.MappingVersion, c.Status, c.Reason = mappingVersion, "investigating", ""
	record, err := recoveryStatusRecord(ctx, c, st, observed)
	if err != nil {
		return err
	}
	if err := outbox.Enqueue(ctx, tx, record); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *Postgres) RecoveryHistory(ctx context.Context, id string) (*orderpb.ExecutionRecoveryHistory, string, error) {
	var data []byte
	var digest, orderID, mapping string
	err := p.pool.QueryRow(ctx, `SELECT h.history, h.payload_digest, c.order_id, c.mapping_version
		FROM execution_recovery_history h JOIN execution_recovery_cases c USING (tenant_id,case_id)
		WHERE h.case_id=$1`, id).Scan(&data, &digest, &orderID, &mapping)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	if recoveryDigest(data) != digest {
		return nil, "", ErrRecoveryEvidenceConflict
	}
	var history orderpb.ExecutionRecoveryHistory
	if err := proto.Unmarshal(data, &history); err != nil {
		return nil, "", err
	}
	if history.GetOrderId() != orderID || history.GetMappingVersion() != mapping {
		return nil, "", ErrRecoveryEvidenceConflict
	}
	return &history, digest, nil
}

// BlockRecovery is an observable unresolved outcome, not a successful correction.
// The lifecycle fact commits with its status and retains the observation lineage.
func (p *Postgres) BlockRecovery(ctx context.Context, c RecoveryCase, st *orderpb.OrderState, reason string, now time.Time) error {
	if !recoveryText(reason, 2048) || st.GetOrderId() != c.OrderID || now.IsZero() {
		return errors.New("oms: invalid blocked recovery")
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE execution_recovery_cases SET status='blocked', reason=$1,
		version=version+1, updated_at=$2 WHERE case_id=$3 AND order_id=$4 AND version=$5 AND status <> 'corrected'`,
		reason, now.UTC(), c.ID, c.OrderID, c.Version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	c.Status, c.Reason = "blocked", reason
	record, err := recoveryStatusRecord(ctx, c, st, now)
	if err != nil {
		return err
	}
	if err := outbox.Enqueue(ctx, tx, record); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func recoveryStatusRecord(ctx context.Context, c RecoveryCase, st *orderpb.OrderState, now time.Time) (outbox.Record, error) {
	if c.Checkpoint < 0 || c.Checkpoint > 8000 {
		return outbox.Record{}, fmt.Errorf("oms: invalid recovery checkpoint %d", c.Checkpoint)
	}
	event := &orderpb.ExecutionRecoveryRecorded{CaseId: c.ID, OrderId: c.OrderID, PortfolioId: st.GetPortfolioId(),
		Venue: st.GetVenue(), VenueAccountId: st.GetVenueAccountId(), MappingVersion: c.MappingVersion,
		SourceCursor: c.SourceCursor, PayloadDigest: c.Digest, Status: c.Status, Reason: c.Reason,
		Checkpoint: uint32(c.Checkpoint), RecordedAt: timestamppb.New(now.UTC())}
	var emitter Emitter
	return outbox.From(ctx, emitter.event(EventTypeRecoveryRecorded, c.OrderID, now, event))
}
