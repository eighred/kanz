package fillfact

import (
	"context"
	"errors"
	"math/big"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// FeeHead reads the latest append-only revision. The caller holds its normal
// aggregate lock and owns the transaction, including any cash and outbox writes.
// The same table contract is installed in the OMS and accounting databases.
func FeeHead(ctx context.Context, tx pgx.Tx, book string, original *orderpb.Fill) (*orderpb.Fill, error) {
	if !feeBook(book) || original == nil {
		return nil, ErrFeeRevision
	}
	var data []byte
	err := tx.QueryRow(ctx, `SELECT revised_fill FROM execution_fee_revisions WHERE book=$1 AND execution_key=$2 ORDER BY revision_sequence DESC LIMIT 1`, book, ExecutionKey(original)).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return original, nil
	}
	if err != nil {
		return nil, err
	}
	var fill orderpb.Fill
	if err := proto.Unmarshal(data, &fill); err != nil {
		return nil, err
	}
	if !SameExecutionExceptFee(original, &fill) {
		return nil, ErrFeeRevision
	}
	return &fill, nil
}

// RecordFeeRevision returns the new cash delta only when this transaction first
// records the revision. Replaying an old approved case after later revisions is
// a no-op, even when fees have since returned to their original value (ABA).
// Callers must commit this claim with their economic change and acknowledgement.
func RecordFeeRevision(ctx context.Context, tx pgx.Tx, book string, original, revised *orderpb.Fill) (*big.Rat, bool, error) {
	if !feeBook(book) || !SameExecutionExceptFee(original, revised) {
		return nil, false, ErrFeeRevision
	}
	if _, err := FeeRevisionTerms(revised); err != nil {
		return nil, false, err
	}
	key, caseID := ExecutionKey(revised), revised.GetRecovery().GetCaseId()
	var prior []byte
	err := tx.QueryRow(ctx, `SELECT revised_fill FROM execution_fee_revisions WHERE book=$1 AND execution_key=$2 AND case_id=$3`, book, key, caseID).Scan(&prior)
	if err == nil {
		var recorded orderpb.Fill
		if err := proto.Unmarshal(prior, &recorded); err != nil {
			return nil, false, err
		}
		if !proto.Equal(&recorded, revised) {
			return nil, false, ErrFeeRevision
		}
		return new(big.Rat), false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}
	head, err := FeeHead(ctx, tx, book, original)
	if err != nil {
		return nil, false, err
	}
	delta, err := ApprovedFeeDelta(head, revised)
	if err != nil {
		return nil, false, err
	}
	previous, err := proto.MarshalOptions{Deterministic: true}.Marshal(head)
	if err != nil {
		return nil, false, err
	}
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(revised)
	if err != nil {
		return nil, false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO execution_fee_revisions (book,execution_key,case_id,approval_digest,previous_fill,revised_fill,order_id) VALUES ($1,$2,$3,$4,$5,$6,$7)`, book, key, caseID, revised.GetRecovery().GetFeeApproval().GetDigest(), previous, data, revised.OrderId)
	if err != nil {
		return nil, false, err
	}
	return delta, true, nil
}

func feeBook(book string) bool { return book == "oms" || book == "position" || book == "ledger" }
