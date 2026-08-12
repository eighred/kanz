package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

// PutBars stores bars idempotently. See BarStore.
//
// ONE TRANSACTION FOR THE BATCH, and every bar validated BEFORE any of it is
// sent: a batch half-written is a series with a hole in it, and a hole in a bar
// series is not visible to anything downstream — an indicator just computes a
// different number, and nothing says why.
func (p *Postgres) PutBars(ctx context.Context, bars []Bar) error {
	if len(bars) == 0 {
		return nil
	}
	for i := range bars {
		if err := bars[i].Validate(); err != nil {
			return err
		}
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	batch := &pgx.Batch{}
	for _, b := range bars {
		cols, err := encodeBarDecimals(b)
		if err != nil {
			return err
		}
		batch.Queue(`
			INSERT INTO ohlcv_bars
				(instrument_id, venue, resolution, bucket_start,
				 open, high, low, close, volume, trade_count, knowledge_time)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (instrument_id, venue, resolution, bucket_start, knowledge_time)
			DO NOTHING
		`, b.InstrumentID, b.Venue, string(b.Resolution), b.BucketStart.UTC(),
			cols.open, cols.high, cols.low, cols.close, cols.volume,
			b.TradeCount, b.KnowledgeTime.UTC())
	}
	br := tx.SendBatch(ctx, batch)
	for range bars {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return fmt.Errorf("insert bar: %w", err)
		}
	}
	if err := br.Close(); err != nil {
		return fmt.Errorf("close batch: %w", err)
	}
	return tx.Commit(ctx)
}

// Bars returns the point-in-time-correct series. See BarStore.
//
// DISTINCT ON (bucket_start) with knowledge_time DESC is the honest read: for
// every interval it takes the newest version KNOWN BY AsOf and no later one. A
// query that dropped the AsOf bound would return today's corrections in a run
// dated last year, and report a strategy that could not have existed.
func (p *Postgres) Bars(ctx context.Context, q BarQuery) ([]Bar, error) {
	if q.InstrumentID == "" {
		return nil, errors.New("store: bars with empty instrument_id")
	}
	if q.Venue == "" {
		return nil, errors.New("store: bars with empty venue — a series is per venue, and " +
			"collapsing venues would silently blend two different markets into one candle")
	}
	if !q.Resolution.Valid() {
		return nil, fmt.Errorf("store: bars with resolution %q, which is not one this platform stores",
			q.Resolution)
	}
	rows, err := p.pool.Query(ctx, `
		SELECT DISTINCT ON (bucket_start)
		       bucket_start, open, high, low, close, volume, trade_count, knowledge_time
		FROM ohlcv_bars
		WHERE instrument_id = $1
		  AND venue = $2
		  AND resolution = $3
		  AND ($4::timestamptz IS NULL OR bucket_start >= $4)
		  AND ($5::timestamptz IS NULL OR bucket_start < $5)
		  AND ($6::timestamptz IS NULL OR knowledge_time <= $6)
		ORDER BY bucket_start, knowledge_time DESC
	`, q.InstrumentID, q.Venue, string(q.Resolution),
		nullTime(q.From), nullTime(q.To), nullTime(q.AsOf))
	if err != nil {
		return nil, fmt.Errorf("query bars %s@%s: %w", q.InstrumentID, q.Venue, err)
	}
	defer rows.Close()

	var out []Bar
	for rows.Next() {
		b := Bar{InstrumentID: q.InstrumentID, Venue: q.Venue, Resolution: q.Resolution}
		var open, high, low, cl, vol []byte
		if err := rows.Scan(&b.BucketStart, &open, &high, &low, &cl, &vol,
			&b.TradeCount, &b.KnowledgeTime); err != nil {
			return nil, fmt.Errorf("scan bar: %w", err)
		}
		for _, f := range []struct {
			raw []byte
			dst **commonpb.Decimal
		}{{open, &b.Open}, {high, &b.High}, {low, &b.Low}, {cl, &b.Close}, {vol, &b.Volume}} {
			d := &commonpb.Decimal{}
			if err := proto.Unmarshal(f.raw, d); err != nil {
				return nil, fmt.Errorf("decode bar %s@%s: %w", q.InstrumentID, b.BucketStart, err)
			}
			*f.dst = d
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// barDecimals are one bar's marshaled price columns.
type barDecimals struct{ open, high, low, close, volume []byte }

func encodeBarDecimals(b Bar) (barDecimals, error) {
	var out barDecimals
	for _, f := range []struct {
		name string
		src  *commonpb.Decimal
		dst  *[]byte
	}{
		{"open", b.Open, &out.open}, {"high", b.High, &out.high}, {"low", b.Low, &out.low},
		{"close", b.Close, &out.close}, {"volume", b.Volume, &out.volume},
	} {
		raw, err := proto.Marshal(f.src)
		if err != nil {
			return barDecimals{}, fmt.Errorf("encode %s %s@%s: %w", f.name, b.InstrumentID, b.BucketStart, err)
		}
		*f.dst = raw
	}
	return out, nil
}
