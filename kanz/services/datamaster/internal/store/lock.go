package store

import (
	"context"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresCycleLock lets exactly one replica run a projection cycle.
//
// # Why this exists
//
// Every datamaster pod runs the projector. The writes are idempotent and
// content-identical, so two pods projecting at once is harmless to the DATA — the
// golden record upserts to the same value and the exception add is ON CONFLICT DO
// NOTHING. What it is not is free: N pods make N rounds of VENDOR API calls per
// cycle, and vendor reference data is metered, rate-limited, and billed per call.
// At replicas: 2 that is double the bill and double the rate-limit budget for
// exactly zero additional information.
//
// pg_try_advisory_lock is the right primitive here rather than a lease row:
//
//   - It is TRY, not wait. A pod that loses the race skips the cycle and gets on
//     with serving reads; it does not queue up behind the winner and then run a
//     redundant refresh the moment the lock frees.
//   - It dies with the session. A pod that is OOM-killed mid-refresh does not
//     strand the lock — Postgres releases it when the connection drops, with no
//     TTL to tune and no lease to expire. A lease row would need a heartbeat and a
//     reaper, and would still have to answer "what if the holder is just slow".
//
// The lock is namespaced PER TENANT. A single shared key would mean one tenant's
// deployment permanently starves every other tenant's projector: the loser skips,
// every cycle, forever, and its master would never be refreshed at all.
type PostgresCycleLock struct {
	pool *pgxpool.Pool
	key  int64
}

// NewPostgresCycleLock returns the cycle lock for one tenant's projector.
func NewPostgresCycleLock(pool *pgxpool.Pool, tenant string) *PostgresCycleLock {
	return &PostgresCycleLock{pool: pool, key: cycleLockKey(tenant)}
}

// cycleLockKey derives this tenant's advisory-lock key. Any deterministic value
// works — an advisory lock key is just a namespace both sides agree on.
func cycleLockKey(tenant string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("datamaster/projector/" + tenant))
	return int64(h.Sum64())
}

// TryAcquire takes the cycle lock without waiting. It returns a release func and
// true if this replica won the cycle; false (with a nil error) means another
// replica is already projecting and this one should skip.
//
// The lock is session-level, so it is held on ONE dedicated connection for the
// whole refresh — it cannot be taken "from the pool at large" (the same reason
// kanz-migrate pins its connection).
func (l *PostgresCycleLock) TryAcquire(ctx context.Context) (func(), bool, error) {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire connection for cycle lock: %w", err)
	}
	var won bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, l.key).Scan(&won); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("try cycle lock: %w", err)
	}
	if !won {
		conn.Release()
		return nil, false, nil
	}
	return func() { l.release(conn) }, true, nil
}

// release unlocks and returns the connection to the pool.
//
// If the unlock FAILS, the connection is destroyed rather than returned. The
// session would still hold the lock, and handing it back to the pool would strand
// that lock for the pool's lifetime — after which no replica, including this one,
// could ever acquire the cycle again and the master would quietly stop being
// refreshed. Killing the session makes Postgres release the lock for us.
func (l *PostgresCycleLock) release(conn *pgxpool.Conn) {
	// Not ctx: the caller's context is typically already cancelled by the time a
	// shutdown unwinds the refresh, and the unlock still has to happen.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, l.key); err != nil {
		if c := conn.Hijack(); c != nil {
			_ = c.Close(ctx)
		}
		return
	}
	conn.Release()
}

// Key exposes the tenant's lock key (tests assert the per-tenant namespacing).
func (l *PostgresCycleLock) Key() int64 { return l.key }
