package store

import (
	"context"
	"errors"
	"time"

	"github.com/eighred/kanz/internal/refdata"
)

// SectorHistory is the durable extension of the current golden store.
type SectorHistory interface {
	PutSectorHistory(context.Context, []refdata.DatedSector) error
	SectorsAt(context.Context, string, time.Time, []string) ([]refdata.DatedSector, error)
}

var ErrSectorRevisionConflict = errors.New("datamaster: sector source revision has conflicting contents")

func (p *PostgresGolden) PutSectorHistory(ctx context.Context, rows []refdata.DatedSector) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, row := range rows {
		tag, err := tx.Exec(ctx, `INSERT INTO sector_classification_history
			(instrument_id,effective_date,source_as_of,taxonomy,code,name,vendor)
			VALUES ($1,$2::text::date,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`,
			row.InstrumentID, row.EffectiveDate, row.SourceAsOf, row.Taxonomy, row.Code, row.Name, row.Vendor)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			continue
		}
		var unchanged bool
		err = tx.QueryRow(ctx, `SELECT (taxonomy,code,name,vendor)=($4,$5,$6,$7)
			FROM sector_classification_history WHERE tenant_id=public.app_current_tenant()
			AND instrument_id=$1 AND effective_date=$2::text::date AND source_as_of=$3`,
			row.InstrumentID, row.EffectiveDate, row.SourceAsOf, row.Taxonomy, row.Code, row.Name, row.Vendor).Scan(&unchanged)
		if err != nil {
			return err
		}
		if !unchanged {
			return ErrSectorRevisionConflict
		}
	}
	return tx.Commit(ctx)
}

func (p *PostgresGolden) SectorsAt(ctx context.Context, date string, asOf time.Time, ids []string) ([]refdata.DatedSector, error) {
	rows, err := p.pool.Query(ctx, `SELECT DISTINCT ON (instrument_id)
		instrument_id,source_as_of,known_at,taxonomy,code,name,vendor
		FROM sector_classification_history
		WHERE tenant_id=public.app_current_tenant() AND effective_date=$1::text::date
		  AND known_at <= $2 AND instrument_id=ANY($3::text[])
		ORDER BY instrument_id, source_as_of DESC`, date, asOf, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]refdata.DatedSector, 0, len(ids))
	for rows.Next() {
		row := refdata.DatedSector{EffectiveDate: date}
		if err := rows.Scan(&row.InstrumentID, &row.SourceAsOf, &row.KnownAt, &row.Taxonomy, &row.Code, &row.Name, &row.Vendor); err != nil {
			return nil, err
		}
		row.SourceAsOf, row.KnownAt = row.SourceAsOf.UTC(), row.KnownAt.UTC()
		out = append(out, row)
	}
	return out, rows.Err()
}
