package order

import (
	"context"
	"errors"
	"time"

	"github.com/eighred/kanz/internal/outbox"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
)

// AcknowledgeRecovery closes a case only after both books confirm every target.
// An OMS outbox commit alone does not prove that cash and positions were booked.
func (p *Postgres) AcknowledgeRecovery(ctx context.Context, ack *orderpb.ExecutionRecoveryApplied, book string, now time.Time) error {
	if (book != "ledger" && book != "position") || !recoveryText(ack.GetCaseId(), 256) || !recoveryText(ack.GetExecutionKey(), 256) || !recoveryText(ack.GetPayloadDigest(), 64) {
		return errors.New("oms: invalid recovery acknowledgement")
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var c RecoveryCase
	err = tx.QueryRow(ctx, recoveryCaseSQL+" FOR UPDATE", ack.GetCaseId()).Scan(&c.ID, &c.OrderID,
		&c.MappingVersion, &c.SourceCursor, &c.Digest, &c.Evidence, &c.Status, &c.Reason, &c.Checkpoint, &c.Version)
	if err != nil {
		return err
	}
	if c.OrderID != ack.GetOrderId() {
		return ErrRecoveryEvidenceConflict
	}
	var matches bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM execution_recovery_targets WHERE case_id=$1 AND execution_key=$2 AND payload_digest=$3 AND execution_digest=$4)`, c.ID, ack.GetExecutionKey(), ack.GetPayloadDigest(), ack.GetExecutionDigest()).Scan(&matches)
	if err != nil {
		return err
	}
	if !matches {
		return ErrRecoveryEvidenceConflict
	}
	if c.Status == "corrected" {
		return tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO execution_recovery_acks (case_id,execution_key,book) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, c.ID, ack.GetExecutionKey(), book); err != nil {
		return err
	}
	var expected, received int
	err = tx.QueryRow(ctx, `SELECT expected_ack_count,(SELECT count(*) FROM execution_recovery_acks WHERE case_id=$1)
		FROM execution_recovery_cases WHERE case_id=$1`, c.ID).Scan(&expected, &received)
	if err != nil {
		return err
	}
	if expected == 0 || received != expected {
		return tx.Commit(ctx)
	}
	if expected != c.Checkpoint*2 {
		return ErrRecoveryEvidenceConflict
	}
	var stateData []byte
	if err := tx.QueryRow(ctx, `SELECT state FROM orders WHERE order_id=$1`, c.OrderID).Scan(&stateData); err != nil {
		return err
	}
	var state orderpb.OrderState
	if err := proto.Unmarshal(stateData, &state); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE execution_recovery_cases SET status='corrected',reason='',version=version+1,updated_at=$2 WHERE case_id=$1`, c.ID, now.UTC()); err != nil {
		return err
	}
	c.Status, c.Reason = "corrected", ""
	record, err := recoveryStatusRecord(ctx, c, &state, now)
	if err != nil {
		return err
	}
	if err := outbox.Enqueue(ctx, tx, record); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
