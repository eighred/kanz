package integrity

import (
	"sync"
	"time"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
)

// StalenessLevel grades one event's ingestion lag.
type StalenessLevel int

const (
	// StalenessUnknown: lag could not be computed (nil envelope, or a
	// missing event_time / ingestion_time). Both are "Required" per
	// envelope.proto; a defensive monitor reports unknown rather than
	// crashing or inventing a lag from an epoch-zero timestamp.
	StalenessUnknown StalenessLevel = iota

	// StalenessFresh: lag within the freshness budget (≤ WarnAfter).
	StalenessFresh

	// StalenessStale: WarnAfter < lag ≤ CriticalAfter. A warning-level
	// signal — the source is lagging but not yet alarming.
	StalenessStale

	// StalenessCritical: lag > CriticalAfter. The data is old enough that
	// consumers acting on it are likely acting on a stale view.
	StalenessCritical

	// StalenessFuture: event_time is ahead of ingestion_time by more than
	// SkewTolerance — clock skew or a bad source timestamp. NOT staleness
	// (the opposite), but a timestamp-integrity anomaly worth its own
	// classification rather than being silently read as "fresh".
	StalenessFuture
)

func (l StalenessLevel) String() string {
	switch l {
	case StalenessUnknown:
		return "unknown"
	case StalenessFresh:
		return "fresh"
	case StalenessStale:
		return "stale"
	case StalenessCritical:
		return "critical"
	case StalenessFuture:
		return "future"
	default:
		return "unknown"
	}
}

// StalenessConfig holds the thresholds. Defaults are conservative
// placeholders — staleness tolerance is per-source (a market tick feed
// wants sub-second budgets; an end-of-day batch wants minutes), tuned
// per deployment.
type StalenessConfig struct {
	// WarnAfter: lag above this is StalenessStale.
	WarnAfter time.Duration
	// CriticalAfter: lag above this is StalenessCritical.
	CriticalAfter time.Duration
	// SkewTolerance: a negative lag (event_time after ingestion_time)
	// beyond this magnitude is StalenessFuture. Small skews are tolerated
	// as Fresh — clocks are never perfectly synced.
	SkewTolerance time.Duration
}

// DefaultStalenessConfig returns conservative defaults.
func DefaultStalenessConfig() StalenessConfig {
	return StalenessConfig{
		WarnAfter:     10 * time.Second,
		CriticalAfter: 60 * time.Second,
		SkewTolerance: 1 * time.Second,
	}
}

// StalenessResult is the outcome of assessing one event.
type StalenessResult struct {
	Subject      string // the stream — env.EventType (e.g. "market.equity.trade")
	PartitionKey string

	EventTime     time.Time
	IngestionTime time.Time

	// Lag is ingestion_time - event_time. Negative ⇒ the event claims an
	// event_time in the future relative to when Kanz received it.
	Lag time.Duration

	Level StalenessLevel

	// Threshold is the configured limit that was exceeded — WarnAfter for
	// Stale, CriticalAfter for Critical. Zero for Fresh / Unknown /
	// Future. Maps to observation.v1.StalenessDetail.threshold (DATA-07).
	Threshold time.Duration

	// LastEventTime is the stale frontier: the most recent event_time seen
	// for this (Subject, PartitionKey), including the current event. Maps
	// to StalenessDetail.last_event_time.
	LastEventTime time.Time
}

// Stale reports whether the level is a staleness alarm (Stale or
// Critical) — the cases DATA-07 turns into a DataQualityEvent.
func (r StalenessResult) Stale() bool {
	return r.Level == StalenessStale || r.Level == StalenessCritical
}

type frontierKey struct {
	subject      string
	partitionKey string
}

// StalenessMonitor assesses per-event ingestion lag (event_time vs
// ingestion_time) and tracks the most-recent event_time per stream (the
// stale frontier). Safe for concurrent use.
//
// Detection only — wiring quality_flags (DATA-06) and emitting
// staleness DataQualityEvents (DATA-07) are later tasks. In-memory and
// per-process, like the GapDetector: the frontier re-baselines on
// restart.
type StalenessMonitor struct {
	cfg      StalenessConfig
	mu       sync.Mutex
	frontier map[frontierKey]time.Time
}

// NewStalenessMonitor returns a monitor. A zero-value field in cfg
// falls back to the default for that field, so callers can override one
// threshold without restating the rest.
func NewStalenessMonitor(cfg StalenessConfig) *StalenessMonitor {
	def := DefaultStalenessConfig()
	if cfg.WarnAfter <= 0 {
		cfg.WarnAfter = def.WarnAfter
	}
	if cfg.CriticalAfter <= 0 {
		cfg.CriticalAfter = def.CriticalAfter
	}
	if cfg.SkewTolerance < 0 {
		cfg.SkewTolerance = def.SkewTolerance
	}
	return &StalenessMonitor{cfg: cfg, frontier: make(map[frontierKey]time.Time)}
}

// Observe assesses one envelope's ingestion lag and updates the
// frontier. A nil envelope or missing event_time / ingestion_time
// yields StalenessUnknown without touching state.
func (m *StalenessMonitor) Observe(env *envelopepb.Envelope) StalenessResult {
	if env == nil || env.EventTime == nil || env.IngestionTime == nil {
		return StalenessResult{Level: StalenessUnknown}
	}
	et := env.EventTime.AsTime()
	it := env.IngestionTime.AsTime()
	lag := it.Sub(et)

	key := frontierKey{subject: env.EventType, partitionKey: env.PartitionKey}

	m.mu.Lock()
	frontier := m.frontier[key]
	if et.After(frontier) {
		frontier = et
		m.frontier[key] = frontier
	}
	m.mu.Unlock()

	res := StalenessResult{
		Subject:       env.EventType,
		PartitionKey:  env.PartitionKey,
		EventTime:     et,
		IngestionTime: it,
		Lag:           lag,
		LastEventTime: frontier,
	}

	switch {
	case lag < -m.cfg.SkewTolerance:
		res.Level = StalenessFuture
	case lag <= m.cfg.WarnAfter:
		res.Level = StalenessFresh
	case lag <= m.cfg.CriticalAfter:
		res.Level = StalenessStale
		res.Threshold = m.cfg.WarnAfter
	default:
		res.Level = StalenessCritical
		res.Threshold = m.cfg.CriticalAfter
	}
	return res
}
