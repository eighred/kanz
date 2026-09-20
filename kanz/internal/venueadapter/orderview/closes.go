package orderview

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/eighred/kanz/internal/execution"
)

// Track commits ownership before any venue request. Redelivery cannot reset its
// age or replace the identity of the order being watched.
func (p *Postgres) Track(ctx context.Context, ci execution.CloseIntent) error {
	if ci.OrderID == "" || ci.InstrumentID == "" {
		return errors.New("close intent requires order and instrument")
	}
	if ci.RequestedAt.IsZero() {
		ci.RequestedAt = time.Now().UTC()
	}
	payload, err := json.Marshal(ci)
	if err != nil {
		return err
	}
	tag, err := p.pool.Exec(ctx, `INSERT INTO venue_pending_closes
		(order_id, instrument_id, requested_at, intent) VALUES ($1,$2,$3,$4)
		ON CONFLICT (tenant_id, order_id) DO UPDATE SET order_id=EXCLUDED.order_id
		WHERE venue_pending_closes.instrument_id=EXCLUDED.instrument_id`,
		ci.OrderID, ci.InstrumentID, ci.RequestedAt, payload)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("close intent identity conflict")
	}
	return nil
}

// DueCloses claims a bounded batch and persists retry scheduling before querying
// the venue. A process crash delays retry by at most one minute; it loses no intent.
func (p *Postgres) DueCloses(ctx context.Context, now time.Time, timeout time.Duration) ([]execution.CloseIntent, error) {
	rows, err := p.pool.Query(ctx, `WITH due AS (
		SELECT tenant_id, order_id FROM venue_pending_closes
		WHERE requested_at <= $1 AND next_attempt_at <= $2
		ORDER BY next_attempt_at, requested_at, order_id LIMIT 100 FOR UPDATE SKIP LOCKED
	) UPDATE venue_pending_closes p SET
		attempts=LEAST(p.attempts+1,7),
		next_attempt_at=$2::timestamptz + make_interval(secs => LEAST(60, power(2, p.attempts))::int)
	FROM due WHERE p.tenant_id=due.tenant_id AND p.order_id=due.order_id
	RETURNING p.intent, p.requested_at`, now.Add(-timeout), now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []execution.CloseIntent
	for rows.Next() {
		var payload []byte
		var at time.Time
		if err := rows.Scan(&payload, &at); err != nil {
			return nil, err
		}
		var ci execution.CloseIntent
		if err := json.Unmarshal(payload, &ci); err != nil {
			return nil, err
		}
		ci.RequestedAt = at
		result = append(result, ci)
	}
	return result, rows.Err()
}

// Resolve follows confirmed terminal evidence and a successful broker publish.
// If deletion fails, ownership remains and the next attempt re-observes truth.
func (p *Postgres) Resolve(ctx context.Context, orderID string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM venue_pending_closes WHERE order_id=$1`, orderID)
	return err
}
