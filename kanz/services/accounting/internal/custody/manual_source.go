package custody

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Serialize the view of the signed intake with the importer's append. A
// recorded batch awaiting consumer delivery makes absence or an older signed
// level inconclusive. The request can be retried once ingestion catches up.
func confirmManualIntake(ctx context.Context, tx pgx.Tx, tenant string, run Run) ([]byte, error) {
	var locked bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock_shared(hashtextextended($1,0))`,
		batchLockKey(tenant, run.Subject.PortfolioID, run.Subject.CustodianID)).Scan(&locked); err != nil {
		return nil, err
	}
	if !locked {
		return nil, ErrRunBusy
	}
	var digest []byte
	var err error
	if verifying(run) {
		digest, err = signedStatementDigest(ctx, tx, tenant, run)
		if err != nil {
			return nil, err
		}
	}
	newest, err := latestSignedStatementID(ctx, tx, run.Subject)
	if err != nil {
		return nil, err
	}
	if newest != "" && (run.observedMissing || run.StatementID != newest) {
		return nil, ErrStatementChanged
	}
	return digest, nil
}

func latestSignedStatementID(ctx context.Context, tx pgx.Tx, subject Subject) (string, error) {
	var newest string
	err := tx.QueryRow(ctx, `SELECT statement_id FROM custody_batch_receipts
        WHERE tenant_id=app_current_tenant() AND portfolio_id=$1 AND custodian_id=$2 AND business_date=$3
        ORDER BY revision DESC LIMIT 1`, subject.PortfolioID, subject.CustodianID, BusinessDay(subject.BusinessDate)).Scan(&newest)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return newest, err
}

// The importer alone writes the WORM receipt. Reconstruct its stamped FACT
// from retained signed bytes so pruning the transport outbox is independent of
// whether a dated comparison can verify the actual winner already in storage.
func signedStatementDigest(ctx context.Context, tx pgx.Tx, tenant string, run Run) ([]byte, error) {
	s := run.compared
	if s == nil || run.StatementID != s.StatementID || !isBatchStatement(s.StatementID) || run.Basis == nil || run.Basis.Mode != BasisDeclared {
		return nil, errBatchReceipt
	}
	accounts := run.Basis.Accounts[run.Subject.CustodianID]
	if len(accounts) != 1 {
		return nil, errBatchReceipt
	}
	var raw, signature, key, wireDigest []byte
	var source string
	var revision int64
	var received time.Time
	err := tx.QueryRow(ctx, `SELECT signed_batch,signature,public_key,wire_sha256,source_id,revision,received_at
        FROM custody_batch_receipts WHERE tenant_id=app_current_tenant() AND portfolio_id=$1
        AND custodian_id=$2 AND business_date=$3 AND statement_id=$4 AND account_id=$5`,
		run.Subject.PortfolioID, run.Subject.CustodianID, run.Subject.BusinessDate, s.StatementID, accounts[0]).
		Scan(&raw, &signature, &key, &wireDigest, &source, &revision, &received)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errBatchReceipt
	}
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(raw)
	expectedID := batchStatementID(source, revision, hex.EncodeToString(digest[:]))
	if s.StatementID != expectedID || len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, raw, signature) {
		return nil, errBatchReceipt
	}
	var batch signedBatch
	if json.Unmarshal(raw, &batch) != nil || batch.Format != BatchFormat || !batch.Complete ||
		batch.Tenant != tenant || batch.Account != accounts[0] || batch.Source != source || batch.Revision != revision {
		return nil, errBatchReceipt
	}
	var msg accountingpb.CustodianStatement
	if protojson.Unmarshal(batch.Statement, &msg) != nil {
		return nil, errBatchReceipt
	}
	msg.StatementId, msg.ReceivedAt = expectedID, timestamppb.New(received)
	wire, err := proto.Marshal(&msg)
	if err != nil {
		return nil, errBatchReceipt
	}
	actual := sha256.Sum256(wire)
	retained, err := statementFromProto(&msg)
	if err != nil || !bytes.Equal(actual[:], wireDigest) || !sameComparison(retained, *s) {
		return nil, errBatchReceipt
	}
	return digest[:], nil
}
