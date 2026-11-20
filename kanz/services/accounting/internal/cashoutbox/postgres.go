// Package cashoutbox preserves the financial timestamp in accounting's
// immutable cash announcement when the shared SQL outbox stores microseconds.
package cashoutbox

import (
	"context"
	"errors"
	"time"

	"github.com/eighred/kanz/internal/cashview"
	"github.com/eighred/kanz/internal/outbox"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

const cashSchema = "accounting.v1.PortfolioCashBalance:1"

// Postgres prepares accounting cash records for the shared durable relay.
type Postgres struct {
	outbox.Queue
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Postgres {
	return &Postgres{Queue: outbox.NewPostgres(pool, outbox.ServiceAccounting), pool: pool}
}

// Validate checks the original cutoff before pgx persists its microsecond
// projection. A one-nanosecond disagreement rolls back the journal transaction.
func Validate(ctx context.Context, tx pgx.Tx, records []outbox.Record) error {
	for i := range records {
		if err := cashTime(ctx, tx, &records[i], false); err != nil {
			return err
		}
	}
	return nil
}

func (q *Postgres) Pending(ctx context.Context, key string, limit int) ([]outbox.Pending, error) {
	rows, err := q.Queue.Pending(ctx, key, limit)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].RecordError = cashTime(ctx, q.pool, &rows[i].Record, true)
	}
	return rows, nil
}

type reader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func cashTime(ctx context.Context, db reader, r *outbox.Record, stored bool) error {
	if r.Subject != cashview.Subject && r.EventType != cashview.Subject && r.PayloadSchemaRef != cashSchema {
		return nil
	}
	if r.Subject != cashview.Subject || r.EventType != cashview.Subject || r.PayloadSchemaRef != cashSchema ||
		r.Domain != "accounting" || r.SchemaVersion != 1 || r.EventClass != envelopepb.EventClass_EVENT_CLASS_FACT || r.TenantID == "" {
		return errors.New("accounting: invalid cash outbox envelope identity")
	}
	var cash accountingpb.PortfolioCashBalance
	if err := proto.Unmarshal(r.Payload, &cash); err != nil {
		return err
	}
	if cash.PortfolioId == "" || cash.PortfolioId != r.PartitionKey || cash.AsOf.CheckValid() != nil || cash.KnowledgeTime.CheckValid() != nil {
		return errors.New("accounting: invalid cash outbox portfolio or timestamp")
	}
	at := cash.AsOf.AsTime()
	expected := at
	if stored {
		// pgx's binary timestamptz representation floors to whole microseconds,
		// including before 1970. The original nanoseconds remain in the FACT.
		expected = at.Truncate(time.Microsecond)
	}
	if !r.EventTime.Equal(expected) {
		return errors.New("accounting: cash outbox event time disagrees with payload as_of")
	}
	if commit := cash.CashCommit; commit != nil {
		if commit.Revision <= 0 || cash.BaseCurrency == "" || !cash.KnowledgeTime.AsTime().Equal(at) {
			return errors.New("accounting: invalid sealed cash cutoff")
		}
		// The ledger seals deterministic protobuf bytes in the same transaction.
		// The tenant predicate also binds the envelope tenant to the RLS history.
		var matches bool
		err := db.QueryRow(ctx, `SELECT EXISTS (SELECT FROM cash_commit_history
			WHERE tenant_id=$1 AND portfolio_id=$2 AND currency=$3 AND revision=$4 AND payload=$5)`,
			r.TenantID, cash.PortfolioId, cash.BaseCurrency, commit.Revision, r.Payload).Scan(&matches)
		if err != nil {
			return err
		}
		if !matches {
			return errors.New("accounting: sealed cash outbox payload differs from retained history")
		}
	}
	r.EventTime = at
	return nil
}
