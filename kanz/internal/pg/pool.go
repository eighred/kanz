// Package pg builds the one kind of Postgres pool this platform is allowed to have:
// a TENANT-SCOPED one.
//
// # Why this exists
//
// Every tenant-scoped table enforces isolation with row-level security keyed on the
// `app.tenant_id` GUC, so a connection that does not set it can read nothing. Eight
// services each carried their own copy of the pgxpool.Config + AfterConnect that sets
// it — which meant the platform's most important safety property was a convention,
// repeated eight times, and a ninth service could simply not repeat it.
//
// That is not hypothetical. accounting shipped with `pgxpool.New()` and no
// AfterConnect at all: its ledger reads returned nothing and its writes failed, and
// every test was green, because the tests set the GUC on their own pools. The service
// was silently unable to write a single row of the book of record.
//
// So there is now ONE constructor, it REFUSES an empty tenant, and the database will
// not answer an unscoped connection anyway (MT-01e: app_current_tenant() RAISES
// rather than returning NULL, so an unscoped query ERRORS instead of quietly
// returning zero rows). Belt at the composition root, braces at the engine.
package pg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewTenantPool opens a pool whose every connection is scoped to tenant.
//
// The tenant is set with set_config(..., false) — session-level, not
// transaction-level — so it survives for the life of the connection and applies to
// every query the pool hands out, including ones outside a transaction.
//
// An empty tenant is refused. It would be indistinguishable, at the database, from a
// service that forgot to scope itself at all — and that is the failure this whole
// mechanism exists to make impossible.
func NewTenantPool(ctx context.Context, dsn, tenant string) (*pgxpool.Pool, error) {
	if dsn == "" {
		return nil, errors.New("pg: empty DSN")
	}
	if tenant == "" {
		return nil, errors.New("pg: empty tenant — a pool with no tenant can read nothing (RLS) and would be " +
			"indistinguishable from a service that forgot to scope itself")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("pg: parse DSN: %w", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		// If this fails, pgx discards the connection and the caller gets the error.
		// A connection that could not be scoped must never be handed to a query.
		if _, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", tenant); err != nil {
			return fmt.Errorf("pg: could not scope the connection to tenant %q: %w", tenant, err)
		}
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pg: open pool: %w", err)
	}
	return pool, nil
}
