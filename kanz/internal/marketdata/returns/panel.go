package returns

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/marketdata/store"
)

// ContinuousUTC is an explicit one-day sampling convention, not an inferred
// exchange calendar. Missing days, holidays and unequal sessions cannot be
// bridged into a daily shock. New calendar conventions require their own proof.
const ContinuousUTC = "continuous_utc_24h"
const IntersectionPolicy = "exact_interval_intersection_v1"

var ErrInsufficientPanel = errors.New("returns: fewer than two common dated scenarios")

type Interval struct{ Start, End time.Time }

// Observation retains both source versions: a correction to either endpoint
// changes the input artifact even when the resulting return is unchanged.
type Observation struct {
	Interval
	Value                        float64
	StartKnowledge, EndKnowledge time.Time
	StartRevision, EndRevision   string
}

type Series struct {
	InstrumentID string
	Calendar     string
	Currency     string
	Kind         store.PriceKind
	Method       ReturnMethod
	Observations []Observation
	Rejected     int
}

type DatedProvider interface {
	DatedReturns(context.Context, string, time.Time, int) (Series, error)
}

// DatedReturns preserves source times instead of assigning dates to array
// positions. The scalar Returns method remains for single-series feature users.
func (p *StoreReturnsProvider) DatedReturns(ctx context.Context, id string, asOf time.Time, window int) (Series, error) {
	s := Series{InstrumentID: id, Calendar: ContinuousUTC, Kind: p.cfg.Kind, Method: p.cfg.Method}
	if asOf.IsZero() || (s.Method != ReturnSimple && s.Method != ReturnLog) {
		return s, errors.New("returns: explicit horizon and valid return convention required")
	}
	if window <= 0 {
		window = p.cfg.Window
	}
	obs, err := p.store.History(ctx, store.Query{InstrumentID: id, Kind: p.cfg.Kind, End: asOf, AsOf: asOf})
	if err != nil {
		return s, err
	}
	if window < len(obs)-1 {
		obs = obs[len(obs)-window-1:]
	}
	for i := 1; i < len(obs); i++ {
		a, b := obs[i-1], obs[i]
		prev, pok := dec.Float64(a.Price)
		cur, cok := dec.Float64(b.Price)
		if !pok || !cok || prev <= 0 || cur <= 0 || !finite(prev) || !finite(cur) || b.ObservationTime.Sub(a.ObservationTime) != 24*time.Hour || a.CurrencyCode != b.CurrencyCode {
			s.Rejected++
			continue
		}
		value := computeReturns([]float64{prev, cur}, s.Method)[0]
		if !finite(value) {
			s.Rejected++
			continue
		}
		if len(s.Observations) != 0 && s.Currency != b.CurrencyCode {
			return s, errors.New("returns: price currency changed within series")
		}
		s.Currency = b.CurrencyCode
		s.Observations = append(s.Observations, Observation{
			Interval: Interval{a.ObservationTime.UTC(), b.ObservationTime.UTC()}, Value: value,
			StartKnowledge: a.KnowledgeTime.UTC(), EndKnowledge: b.KnowledgeTime.UTC(),
			StartRevision: revision(a), EndRevision: revision(b),
		})
	}
	return s, nil
}

func revision(o store.Observation) string {
	o.ObservationTime, o.KnowledgeTime = o.ObservationTime.UTC(), o.KnowledgeTime.UTC()
	b, _ := json.Marshal(o) // store fields contain no floats or unsupported values
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// Panel rows have exactly the same intervals. Missing rows are named, never
// zero-filled. Estimators decide whether partial instrument coverage is allowed.
type Panel struct {
	Instruments []string
	Intervals   []Interval
	Values      [][]float64
	Missing     map[string]string
	Dropped     int
	Digest      string
	Method      string
	PriceKind   store.PriceKind
}

func (p Panel) Params() map[string]string {
	return map[string]string{"panel_digest": p.Digest, "panel_policy": IntersectionPolicy,
		"panel_calendar": ContinuousUTC, "panel_scenarios": strconv.Itoa(len(p.Intervals)),
		"panel_return_method": p.Method, "panel_price_kind": strconv.Itoa(int(p.PriceKind)),
		"panel_dropped_intervals": strconv.Itoa(p.Dropped), "panel_missing_instruments": strconv.Itoa(len(p.Missing))}
}

// Contiguous is required for a path-dependent statistic such as drawdown.
// An intersection can serve empirical shocks without claiming the missing
// days were a flat segment of a realized value path.
func (p Panel) Contiguous() bool {
	for i := 1; i < len(p.Intervals); i++ {
		if !p.Intervals[i].Start.Equal(p.Intervals[i-1].End) {
			return false
		}
	}
	return len(p.Intervals) >= 2
}

// Load rejects undated providers. There is deliberately no legacy tail-alignment
// fallback: dates cannot be recovered from a list of numbers.
func Load(ctx context.Context, provider any, instruments []string, asOf time.Time, window int) (Panel, error) {
	p := Panel{Missing: map[string]string{}}
	ids := append([]string(nil), instruments...)
	sort.Strings(ids)
	ids = compact(ids)
	rp, dated := provider.(DatedProvider)
	var series []Series
	for _, id := range ids {
		if !dated {
			p.Missing[id] = "undated_returns"
			continue
		}
		s, err := rp.DatedReturns(ctx, id, asOf, window)
		s.Observations = append([]Observation(nil), s.Observations...)
		if err != nil || !validSeries(s, id, asOf) || len(s.Observations) < 2 {
			p.Missing[id] = "no_returns"
			continue
		}
		series = append(series, s)
		p.Instruments = append(p.Instruments, id)
	}
	if len(series) == 0 {
		return p, ErrInsufficientPanel
	}
	p.Method = "simple"
	if series[0].Method == ReturnLog {
		p.Method = "log"
	}
	p.PriceKind = series[0].Kind
	// Different return/price conventions cannot share one covariance even when
	// their timestamps happen to match. Currency is preserved per row, not converted.
	counts := map[Interval]int{}
	for _, s := range series {
		if s.Method != series[0].Method || s.Kind != series[0].Kind {
			return p, errors.New("returns: incompatible panel conventions")
		}
		for _, o := range s.Observations {
			counts[o.Interval]++
		}
	}
	for _, o := range series[0].Observations {
		if counts[o.Interval] == len(series) {
			p.Intervals = append(p.Intervals, o.Interval)
		}
	}
	if window > 0 && len(p.Intervals) > window {
		p.Intervals = p.Intervals[len(p.Intervals)-window:]
	}
	for _, s := range series {
		byInterval := make(map[Interval]Observation, len(s.Observations))
		for _, o := range s.Observations {
			byInterval[o.Interval] = o
		}
		row := make([]float64, len(p.Intervals))
		for j, interval := range p.Intervals {
			row[j] = byInterval[interval].Value
		}
		p.Values = append(p.Values, row)
		p.Dropped += s.Rejected + len(s.Observations) - len(p.Intervals)
	}
	// Hash the canonical source series as well as selected intervals and policy;
	// excluded observations and source revisions remain part of reproducibility.
	b, err := json.Marshal(struct {
		Policy string
		AsOf   time.Time
		Series []Series
		Panel  Panel
	}{IntersectionPolicy, asOf.UTC(), series, p})
	if err != nil {
		return p, err
	}
	h := sha256.Sum256(b)
	p.Digest = hex.EncodeToString(h[:])
	if len(p.Intervals) < 2 {
		return p, ErrInsufficientPanel
	}
	return p, nil
}

func compact(ids []string) []string {
	var out []string
	for _, id := range ids {
		if len(out) == 0 || id != out[len(out)-1] {
			out = append(out, id)
		}
	}
	return out
}

func validSeries(s Series, id string, asOf time.Time) bool {
	if asOf.IsZero() || id == "" || s.InstrumentID != id || s.Calendar != ContinuousUTC || s.Kind < store.PriceKindClose || s.Kind > store.PriceKindLast || s.Rejected < 0 || (s.Method != ReturnSimple && s.Method != ReturnLog) {
		return false
	}
	var end time.Time
	for i := range s.Observations {
		o := &s.Observations[i]
		// Normalize equivalent instants before using intervals as map keys.
		o.Start, o.End = o.Start.UTC(), o.End.UTC()
		o.StartKnowledge, o.EndKnowledge = o.StartKnowledge.UTC(), o.EndKnowledge.UTC()
		if o.Start.IsZero() || o.End.Sub(o.Start) != 24*time.Hour || o.Start.Before(end) || o.End.After(asOf) || o.StartKnowledge.IsZero() || o.EndKnowledge.IsZero() || o.StartKnowledge.After(asOf) || o.EndKnowledge.After(asOf) || o.StartRevision == "" || o.EndRevision == "" || !finite(o.Value) {
			return false
		}
		end = o.End
	}
	return true
}
