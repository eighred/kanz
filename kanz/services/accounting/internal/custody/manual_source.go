package custody

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Serialize the view of the signed intake with the importer's append. A
// recorded batch awaiting consumer delivery makes absence or an older signed
// level inconclusive. The request can be retried once ingestion catches up.
func confirmManualIntake(ctx context.Context, tx pgx.Tx, run Run) error {
	var locked bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock_shared(hashtextextended($1,0))`,
		batchLockKey(bus.TenantIDFromContext(ctx), run.Subject.PortfolioID, run.Subject.CustodianID)).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return ErrRunBusy
	}
	var newest string
	err := tx.QueryRow(ctx, `SELECT statement_id FROM custody_batch_receipts
        WHERE tenant_id=app_current_tenant() AND portfolio_id=$1 AND custodian_id=$2 AND business_date=$3
        ORDER BY revision DESC LIMIT 1`, run.Subject.PortfolioID, run.Subject.CustodianID, run.Subject.BusinessDate).Scan(&newest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if run.observedMissing || isBatchStatement(run.StatementID) && run.StatementID != newest {
		return ErrStatementChanged
	}
	return nil
}

// The importer alone writes the WORM receipt. Reconstruct its stamped FACT
// from retained signed bytes so pruning the transport outbox is independent of
// whether a dated comparison can verify the actual winner already in storage.
func confirmManualStatement(ctx context.Context, tx pgx.Tx, run Run) error {
	s := run.compared
	if s == nil || !isBatchStatement(s.StatementID) || run.Basis == nil || run.Basis.Mode != BasisDeclared {
		return errBatchReceipt
	}
	accounts := run.Basis.Accounts[run.Subject.CustodianID]
	if len(accounts) != 1 {
		return errBatchReceipt
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
		return errBatchReceipt
	}
	if err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	expectedID := fmt.Sprintf("%s%s.r%d.%s", batchStatementPrefix, source, revision, hex.EncodeToString(digest[:]))
	if s.StatementID != expectedID || len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, raw, signature) {
		return errBatchReceipt
	}
	var batch signedBatch
	if json.Unmarshal(raw, &batch) != nil || batch.Format != BatchFormat || !batch.Complete ||
		batch.Tenant != bus.TenantIDFromContext(ctx) || batch.Account != accounts[0] || batch.Source != source || batch.Revision != revision {
		return errBatchReceipt
	}
	var msg accountingpb.CustodianStatement
	if protojson.Unmarshal(batch.Statement, &msg) != nil {
		return errBatchReceipt
	}
	msg.StatementId, msg.ReceivedAt = expectedID, timestamppb.New(received)
	wire, err := proto.Marshal(&msg)
	if err != nil {
		return errBatchReceipt
	}
	actual := sha256.Sum256(wire)
	retained, err := statementFromProto(&msg)
	if err != nil || !bytes.Equal(actual[:], wireDigest) || !sameComparison(retained, *s) {
		return errBatchReceipt
	}
	return nil
}
