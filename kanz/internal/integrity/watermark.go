package integrity

import (
	"sync"
	"time"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
)

// WatermarkStatus classifies one event against its stream's event-time
// watermark.
type WatermarkStatus int

const (
	// WatermarkUnknown: nil envelope or missing event_time — can't place
	// the event on the event-time axis.
	WatermarkUnknown WatermarkStatus = iota

	// WatermarkFirstSeen: the first event on a stream. There is no prior
	// watermark to judge it against, so it cannot be late — it just seeds
	// the watermark.
	WatermarkFirstSeen

	// WatermarkOnTime: event_time is at or after the current watermark.
	WatermarkOnTime

	// WatermarkLate: event_time is before the current watermark — it
	// arrived after the consumer had already advanced past its event_time
	// (envelope QUALITY_FLAG_LATE). The watermark is NOT moved back.
	WatermarkLate
)

func (s WatermarkStatus) String() string {
	switch s {
	case WatermarkUnknown:
		return "unknown"
	case WatermarkFirstSeen:
		return "first_seen"
	case WatermarkOnTime:
		return "on_time"
	case WatermarkLate:
		return "late"
	default:
		return "unknown"
	}
}

// WatermarkResult is the outcome of placing one event on its stream's
// event-time axis.
type WatermarkResult struct {
	Subject      string // the stream — env.EventType
	PartitionKey string

	EventTime time.Time

	// Watermark is the boundary the event was judged against: the max
	// event_time seen so far minus AllowedLateness. Zero for FirstSeen /
	// Unknown (no watermark established yet).
	Watermark time.Time

	Status WatermarkStatus

	// Lateness is how far behind the watermark the event_time fell
	// (Watermark - EventTime), set only when Status == WatermarkLate.
	Lateness time.Duration
}

// Late reports whether the event should be routed/flagged as late
// (the case DATA-06 stamps QUALITY_FLAG_LATE on and DATA-07 may report).
func (r WatermarkResult) Late() bool { return r.Status == WatermarkLate }

// WatermarkTracker maintains a per-stream event-time watermark and
// classifies each event as on-time or late.
//
// The watermark is a bounded-out-of-orderness estimate: watermark =
// (max event_time seen) - AllowedLateness. AllowedLateness is the grace
// for normal reordering within a partition — an event up to that far
// behind the frontier is still on-time. An event older than the
// watermark is late: the stream has provably moved on, so a consumer
// that already emitted results for that event-time window would have to
// revise them. Late events are flagged, not dropped (the data-integrity
// layer never black-holes), and never pull the watermark backwards.
//
// Detection only — DATA-06 wires the QUALITY_FLAG_LATE stamp
// (MarkLate below) into producers/ingestion. In-memory, per-process,
// concurrent-safe; the watermark re-baselines on restart.
type WatermarkTracker struct {
	allowedLateness time.Duration
	mu              sync.Mutex
	maxSeen         map[frontierKey]time.Time
}

// DefaultAllowedLateness is a conservative grace period. Tune per
// stream: a strictly-ordered single-producer feed wants near-zero; a
// fan-in of many sources wants more.
const DefaultAllowedLateness = 5 * time.Second

// NewWatermarkTracker returns a tracker. A non-positive allowedLateness
// falls back to DefaultAllowedLateness.
func NewWatermarkTracker(allowedLateness time.Duration) *WatermarkTracker {
	if allowedLateness <= 0 {
		allowedLateness = DefaultAllowedLateness
	}
	return &WatermarkTracker{
		allowedLateness: allowedLateness,
		maxSeen:         make(map[frontierKey]time.Time),
	}
}

// Observe places one envelope on its stream's event-time axis and
// returns the classification, advancing the watermark for on-time
// events. A nil envelope or missing event_time yields WatermarkUnknown
// without touching state.
func (w *WatermarkTracker) Observe(env *envelopepb.Envelope) WatermarkResult {
	if env == nil || env.EventTime == nil {
		return WatermarkResult{Status: WatermarkUnknown}
	}
	et := env.EventTime.AsTime()
	key := frontierKey{subject: env.EventType, partitionKey: env.PartitionKey}

	res := WatermarkResult{
		Subject:      env.EventType,
		PartitionKey: env.PartitionKey,
		EventTime:    et,
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	maxSeen, seen := w.maxSeen[key]
	if !seen {
		w.maxSeen[key] = et
		res.Status = WatermarkFirstSeen
		return res
	}

	watermark := maxSeen.Add(-w.allowedLateness)
	res.Watermark = watermark
	if et.Before(watermark) {
		res.Status = WatermarkLate
		res.Lateness = watermark.Sub(et)
		return res // late events never advance the watermark
	}

	res.Status = WatermarkOnTime
	if et.After(maxSeen) {
		w.maxSeen[key] = et
	}
	return res
}

// MarkLate sets QUALITY_FLAG_LATE on the envelope if not already present.
// Idempotent (mirrors replay.StampReplayed). This is the "route late
// data" primitive: a consumer/ingestor that finds an event late per its
// WatermarkTracker stamps the flag so payload-blind tooling downstream
// (DATA-05 reconciliation, DATA-07 reporting) sees it without re-running
// watermark logic. DATA-06 wires this into the producers.
func MarkLate(env *envelopepb.Envelope) {
	if env == nil || IsLate(env) {
		return
	}
	env.QualityFlags = append(env.QualityFlags, envelopepb.QualityFlag_QUALITY_FLAG_LATE)
}

// IsLate reports whether the envelope carries QUALITY_FLAG_LATE.
func IsLate(env *envelopepb.Envelope) bool {
	if env == nil {
		return false
	}
	for _, qf := range env.QualityFlags {
		if qf == envelopepb.QualityFlag_QUALITY_FLAG_LATE {
			return true
		}
	}
	return false
}
