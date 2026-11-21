package main

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/dec"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

const (
	interval   = 5 * time.Minute
	warmupBars = 20
	dayBars    = 24 * time.Hour / interval
)

type bar struct {
	start time.Time
	open  *big.Rat
	close *commonpb.Decimal
}

func positiveDecimal(field json.RawMessage) (*commonpb.Decimal, error) {
	var value string
	if err := json.Unmarshal(field, &value); err != nil || strings.Contains(value, "/") {
		return nil, fmt.Errorf("expected decimal string")
	}
	d, ok := dec.ParseProtoExact(value)
	if !ok || d.Coefficient <= 0 {
		return nil, fmt.Errorf("expected positive exact decimal")
	}
	return d, nil
}

// An archived Binance kline has no symbol, feed identity or publication time.
// Check the requested interval and the fields used for valuation and activity.
func readBars(data []byte, day time.Time) ([]bar, error) {
	var rows [][]json.RawMessage
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, err
	}
	if len(rows) != warmupBars+int(dayBars) {
		return nil, fmt.Errorf("expected 20 warm-up and 288 daily bars, got %d", len(rows))
	}
	bars := make([]bar, len(rows))
	for i, row := range rows {
		if len(row) != 12 {
			return nil, fmt.Errorf("row %d: expected 12 fields", i)
		}
		start := day.Add(time.Duration(i-warmupBars) * interval)
		var opened, closed, trades int64
		if json.Unmarshal(row[0], &opened) != nil || opened != start.UnixMilli() ||
			json.Unmarshal(row[6], &closed) != nil || closed != start.Add(interval).UnixMilli()-1 {
			return nil, fmt.Errorf("row %d: invalid or discontinuous 5m interval", i)
		}
		if json.Unmarshal(row[8], &trades) != nil || trades <= 0 {
			return nil, fmt.Errorf("row %d: missing trading activity", i)
		}
		var fields [5]*commonpb.Decimal
		for j := range fields {
			v, err := positiveDecimal(row[j+1])
			if err != nil {
				return nil, fmt.Errorf("row %d, field %d: %w", i, j+1, err)
			}
			fields[j] = v
		}
		open, high, low, closePrice := dec.FromProto(fields[0]), dec.FromProto(fields[1]), dec.FromProto(fields[2]), dec.FromProto(fields[3])
		if low.Cmp(open) > 0 || low.Cmp(closePrice) > 0 || high.Cmp(open) < 0 || high.Cmp(closePrice) < 0 {
			return nil, fmt.Errorf("row %d: inconsistent OHLC bounds", i)
		}
		bars[i] = bar{start: start, open: open, close: fields[3]}
	}
	return bars, nil
}
