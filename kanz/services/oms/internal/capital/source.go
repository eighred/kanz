package capital

import (
	"context"
	"crypto/sha256"
	"errors"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

// ApplyPayload accepts ONLY an authenticated accounting payload for the pool's
// tenant. The transport must check envelope identity before calling it. Invalid
// or legacy evidence latches the whole tenant closed because its portfolio
// identity may be absent or corrupt. Malformed/legacy evidence is refused only
// after that latch commits; a database failure is retryable, never an ack.
// Valid ordered facts retain Apply's separate gap/conflict semantics.
func ApplyPayload(ctx context.Context, pool *pgxpool.Pool, payload []byte) error {
	var msg accountingpb.PortfolioCashBalance
	err := ErrInvalid
	if len(payload) <= 512<<10 {
		err = proto.Unmarshal(payload, &msg)
	}
	var event CashEvent
	if err == nil {
		event, err = FromBalance(&msg)
	} else {
		err = ErrInvalid
	}
	if err == nil {
		err = Apply(ctx, pool, event)
		if !errors.Is(err, ErrInvalid) {
			return err
		}
	}
	digest := sha256.Sum256(payload)
	// The conflicting UPDATE waits for admitted transactions holding the shared
	// health lock. Once this commits, no subsequent reservation can use old cash.
	_, dbErr := pool.Exec(ctx, `INSERT INTO capital_source_health(fault_digest) VALUES($1)
		ON CONFLICT (tenant_id) DO UPDATE SET fault_digest=COALESCE(capital_source_health.fault_digest,EXCLUDED.fault_digest)`, digest[:])
	if dbErr != nil {
		return dbErr
	}
	return err
}

// sourceFault is locked BEFORE a cash balance. Shared locks let independent
// portfolios admit concurrently, while a source invalidation waits for all
// transactions that already observed healthy evidence to commit or roll back.
// Reductions and execution observations remain possible during quarantine.
func sourceFault(ctx context.Context, tx pgx.Tx) (bool, error) {
	var fault []byte
	err := tx.QueryRow(ctx, `SELECT fault_digest FROM capital_source_health
		WHERE tenant_id=app_current_tenant() FOR SHARE`).Scan(&fault)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, ErrUnknown
	}
	return len(fault) != 0, err
}
