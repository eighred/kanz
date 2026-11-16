package custody

import (
	"context"
	"github.com/jackc/pgx/v5"
)

func (p *Postgres) exactTransaction(ctx context.Context) (pgx.Tx, error) {
	var tx pgx.Tx
	var err error
	if p.tx != nil {
		tx, err = p.tx.Begin(ctx)
	} else {
		tx, err = p.pool.Begin(ctx)
	}
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('app.custody_exact_writer','v1',true)`); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}
