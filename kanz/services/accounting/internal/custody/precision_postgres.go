package custody

import (
	"context"
	"github.com/jackc/pgx/v5"
)

func (p *Postgres) exactTransaction(ctx context.Context) (pgx.Tx, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('app.custody_exact_writer','v1',true)`); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}
