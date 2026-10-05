// Package returnstest dates analytical test vectors on an explicit synthetic
// daily grid. Production providers must retain actual source timestamps.
package returnstest

import (
	"github.com/eighred/kanz/internal/marketdata/returns"
	"github.com/eighred/kanz/internal/marketdata/store"
	"strconv"
	"time"
)

func Series(id string, asOf time.Time, values []float64) returns.Series {
	s := returns.Series{InstrumentID: id, Calendar: returns.ContinuousUTC, Currency: "USD", Kind: store.PriceKindClose, Method: returns.ReturnSimple}
	for i, value := range values {
		end := asOf.UTC().Add(time.Duration(i-len(values)+1) * 24 * time.Hour)
		start := end.Add(-24 * time.Hour)
		s.Observations = append(s.Observations, returns.Observation{Interval: returns.Interval{Start: start, End: end}, Value: value,
			StartKnowledge: start, EndKnowledge: end, StartRevision: "test-" + strconv.Itoa(i), EndRevision: "test-" + strconv.Itoa(i+1)})
	}
	return s
}
