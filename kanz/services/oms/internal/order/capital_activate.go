package order

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/capital"
)

// ReconcileLegacyPopulation is an explicit activation operation, never an OMS
// broker handler. The caller supplies an independently approved maximum source
// and an authenticated retained accounting history reader. It imports every
// old order before replaying any cash receipts, then checks that the complete
// current prefix has landed before enabling the tenant write barrier. Partial
// work remains closed and can be resumed with the same authorities.
func (p *Postgres) ReconcileLegacyPopulation(ctx context.Context, authority LegacyAuthority, history capital.HistoryReader, now time.Time) error {
	if authority == nil || history == nil || now.IsZero() || bus.TenantIDFromContext(ctx) == "" {
		return capital.ErrInvalid
	}
	for {
		rows, err := p.pool.Query(ctx, `SELECT o.order_id FROM orders o WHERE NOT EXISTS (
			SELECT 1 FROM capital_members m WHERE m.tenant_id=o.tenant_id AND m.order_id=o.order_id
		) ORDER BY CASE WHEN o.parent_order_id='' THEN 0 ELSE 1 END,o.order_id LIMIT 256`)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(ids) == 0 {
			break
		}
		for _, id := range ids {
			if err := p.ReconcileLegacy(ctx, id, authority, now); err != nil {
				if errors.Is(err, capital.ErrConflict) {
					return fmt.Errorf("legacy order %s changed or already imported concurrently: %w", id, err)
				}
				return fmt.Errorf("legacy order %s: %w", id, err)
			}
		}
	}
	rows, err := p.pool.Query(ctx, `SELECT portfolio_id,currency,revision FROM capital_balances ORDER BY portfolio_id,currency`)
	if err != nil {
		return err
	}
	type currencyHead struct {
		portfolio, currency string
		revision            int64
	}
	var heads []currencyHead
	for rows.Next() {
		var head currencyHead
		if err := rows.Scan(&head.portfolio, &head.currency, &head.revision); err != nil {
			rows.Close()
			return err
		}
		heads = append(heads, head)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, head := range heads {
		page, err := history.ReadCashCommits(ctx, head.portfolio, head.currency, head.revision, 0, 1)
		if err != nil {
			return err
		}
		if page.TenantID != bus.TenantIDFromContext(ctx) || page.PortfolioID != head.portfolio || page.Currency != head.currency || page.Through <= 0 || page.Through < head.revision {
			return capital.ErrInvalid
		}
		for target := head.revision; target < page.Through; {
			through := page.Through
			if through-target > 4096 {
				through = target + 4096
			}
			if err := capital.RecoverCashPrefix(ctx, p.pool, history, head.portfolio, head.currency, through); err != nil {
				return err
			}
			target = through
		}
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var incomplete bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM capital_balances WHERE revision=0 OR coverage_complete=false OR conflicted=true OR gap_revision>revision OR observed_at_ns IS NULL OR observed_at_ns<$1)`, now.Add(-time.Minute).UnixNano()).Scan(&incomplete); err != nil {
		return err
	}
	if incomplete {
		return capital.ErrUnknown
	}
	if err := armCapitalAdmission(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
