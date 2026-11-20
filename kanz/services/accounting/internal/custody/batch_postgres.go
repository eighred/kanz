package custody

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/eighred/kanz/internal/outbox"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type BatchImporter struct {
	verify batchVerifier
	pool   *pgxpool.Pool
	relay  *outbox.Relay
}

func NewBatchImporter(a BatchAuthority, tenant string, pairs []string, accounts string, pool *pgxpool.Pool, publisher outbox.Publisher, logger *slog.Logger) (*BatchImporter, error) {
	if pool == nil || publisher == nil {
		return nil, errors.New("custody batch: durable storage and publisher required")
	}
	v, err := newBatchVerifier(a, tenant, pairs, accounts)
	if err != nil {
		return nil, err
	}
	relay, err := outbox.NewRelay(outbox.NewPostgres(pool, BatchProducer), publisher, logger)
	if err != nil {
		return nil, err
	}
	return &BatchImporter{v, pool, relay}, nil
}

// Import commits the original signed record and queues one canonical statement.
// Retrying the same file resumes its partition's outbox after a process restart.
func (i *BatchImporter) Import(ctx context.Context, raw, signature []byte) (string, error) {
	b, err := i.verify.verify(raw, signature)
	if err != nil {
		return "", err
	}
	a := i.verify.authority
	if bus.TenantIDFromContext(ctx) != a.Tenant {
		return "", errors.New("custody batch: unauthorized tenant context")
	}
	tx, err := i.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var tenant string
	if err = tx.QueryRow(ctx, "SELECT app_current_tenant()").Scan(&tenant); err != nil {
		return "", err
	}
	if tenant != a.Tenant {
		return "", errors.New("custody batch: database tenant differs from source grant")
	}
	day := b.statement.BusinessDate.AsTime().Format(time.DateOnly)
	part := b.statement.PortfolioId
	key := fmt.Sprintf("%s/%s/%s/%s", tenant, part, a.Custodian, day)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "custody-batch/"+key); err != nil {
		return "", err
	}
	var stored string
	err = tx.QueryRow(ctx, `SELECT sha256 FROM custody_batch_receipts WHERE tenant_id=$1 AND portfolio_id=$2 AND custodian_id=$3 AND business_date=$4::text::date AND revision=$5`, tenant, part, a.Custodian, day, b.header.Revision).Scan(&stored)
	switch {
	case err == nil:
		if stored != b.digest {
			return "", errors.New("custody batch: conflicting immutable revision")
		}
	case errors.Is(err, pgx.ErrNoRows):
		if err = i.append(ctx, tx, b, raw, signature, day); err != nil {
			return "", err
		}
	default:
		return "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	if _, err = i.relay.Flush(ctx, part); err != nil {
		return b.statement.StatementId, fmt.Errorf("custody batch: receipt committed, publication pending: %w", err)
	}
	return b.statement.StatementId, nil
}

func (i *BatchImporter) append(ctx context.Context, tx pgx.Tx, b batchInput, raw, signature []byte, day string) error {
	a := i.verify.authority
	var revision int64
	var digest, source, account string
	var last time.Time
	err := tx.QueryRow(ctx, `SELECT revision,sha256,source_id,account_id,received_at FROM custody_batch_receipts WHERE tenant_id=$1 AND portfolio_id=$2 AND custodian_id=$3 AND business_date=$4::text::date ORDER BY revision DESC LIMIT 1`, a.Tenant, a.Portfolio, a.Custodian, day).Scan(&revision, &digest, &source, &account, &last)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if b.header.Revision-1 != revision || b.header.Previous != digest || revision > 0 && (source != a.Source || account != a.Account) {
		return errors.New("custody batch: previous signed source revision missing or different")
	}
	var received time.Time
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&received); err != nil {
		return err
	}
	if !received.After(last) {
		return errors.New("custody batch: database receipt clock has not advanced")
	}
	if err = b.stamp(received); err != nil {
		return err
	}
	record, err := outbox.From(ctx, bus.Event{Subject: SubjectStatement, EventType: SubjectStatement, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion: 1, Domain: "accounting", TenantID: a.Tenant, EventTime: received, PartitionKey: a.Portfolio, PayloadSchemaRef: schemaRefStatement, Payload: b.statement})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO custody_batch_receipts (portfolio_id,custodian_id,account_id,source_id,business_date,revision,previous_sha256,sha256,public_key,signature,signed_batch,received_at,statement_id)
		VALUES($1,$2,$3,$4,$5::text::date,$6,$7,$8,$9,$10,$11,$12,$13)`, a.Portfolio, a.Custodian, a.Account, a.Source, day, b.header.Revision, b.header.Previous, b.digest, []byte(i.verify.key), signature, raw, received, b.statement.StatementId)
	if err != nil {
		return err
	}
	return outbox.Enqueue(ctx, tx, BatchProducer, record)
}
