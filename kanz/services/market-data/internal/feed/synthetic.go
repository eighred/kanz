package feed

import (
	"time"

	marketpb "github.com/kanz-eng/kanz-schemas-go/market/v1"
)

// SyntheticSession builds a deterministic, gap-free Session for the given
// instruments — the default feed a local/dev boot streams through a SimAdapter
// when no vendor Source is bound (the SimVenue/SimFeed stance). It is NOT a
// vendor fixture: it exists so `cmd/market-data` can run the full publish path
// (adapter → Gate → BusSink → spine) offline. Each instrument gets `ticks`
// events, alternating trade/quote, event_time stepping forward by `step` from
// `start`, source_sequence increasing by 1 per instrument so downstream gap
// detection sees a clean stream. Every event is built through the validating
// Trade/Quote constructors, so the session satisfies Validate by construction.
func SyntheticSession(start time.Time, step time.Duration, instruments []string, ticks int) Session {
	if step <= 0 {
		step = time.Second
	}
	var out Session
	for i := 0; i < ticks; i++ {
		for j, id := range instruments {
			m := Meta{
				InstrumentID:   id,
				Symbol:         id,
				MIC:            "XNAS",
				EventTime:      start.Add(time.Duration(i) * step),
				SourceSequence: uint64(i + 1),
			}
			// Deterministic prices: a base per instrument index, drifting by tick.
			base := float64(100 + j)
			px := base + float64(i)*0.01
			var (
				ev  *marketpb.MarketDataEvent
				err error
			)
			if i%2 == 0 {
				ev, err = Trade(m, DecimalFromFloat(px, -2), DecimalFromFloat(100, 0), "")
			} else {
				ev, err = Quote(m,
					DecimalFromFloat(px-0.01, -2), DecimalFromFloat(100, 0),
					DecimalFromFloat(px+0.01, -2), DecimalFromFloat(100, 0))
			}
			if err != nil {
				continue // unreachable for well-formed Meta; skip rather than panic
			}
			out = append(out, ev)
		}
	}
	return out
}
