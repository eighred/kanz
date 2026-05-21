// Package integrity is the data-integrity layer (DATA-01..11): it
// watches the event stream for correctness signals the broker cannot
// give — sequence gaps, staleness, late data, distribution drift —
// and (later tasks) emits data-quality events + quality_flags.
//
// First tenant: producer_sequence gap detection (DATA-01).
package integrity

import (
	"sync"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
)

// Status classifies one observation against the per-stream sequence
// history.
type Status int

const (
	// StatusNotApplicable: producer_sequence == 0. The producer did not
	// sequence this event (no partition_key, per envelope.proto), so
	// there is nothing to check. Not an anomaly.
	StatusNotApplicable Status = iota

	// StatusFirstSeen: the first event observed for a stream. Establishes
	// the baseline — NOT reported as a gap even if the sequence is > 1,
	// because a consumer legitimately starts mid-stream (late join,
	// partition reassignment, replay window). A caller that bootstraps
	// from the start and expects sequence==1 can inspect Sequence.
	StatusFirstSeen

	// StatusOK: in order — sequence == previous + 1.
	StatusOK

	// StatusGap: sequence > previous + 1. One or more events between
	// previous and this one were never delivered. MissingFrom..MissingTo
	// (inclusive) name them.
	StatusGap

	// StatusDuplicate: sequence == previous. A redelivery of the
	// high-water-mark event (the broker re-sent, dedup let it through, or
	// an at-least-once seam). Not a gap.
	StatusDuplicate

	// StatusRegression: sequence < previous. An out-of-order or stale
	// redelivery — the per-partition ordering guarantee (event-class-
	// rules §1) says this should not happen within a stream, so it is an
	// integrity anomaly worth surfacing. The high-water mark is kept.
	StatusRegression
)

func (s Status) String() string {
	switch s {
	case StatusNotApplicable:
		return "not_applicable"
	case StatusFirstSeen:
		return "first_seen"
	case StatusOK:
		return "ok"
	case StatusGap:
		return "gap"
	case StatusDuplicate:
		return "duplicate"
	case StatusRegression:
		return "regression"
	default:
		return "unknown"
	}
}

// StreamKey identifies the scope within which producer_sequence is
// monotonic. The Producer (EVT-17b) keeps a 1-based counter per
// (event_type, partition_key); each Producer instance carries one
// source. So a sequence is only comparable within (source, event_type,
// partition_key): two sources writing the same partition_key each run
// their own 1,2,3…, and interleaving them under a single key would
// manufacture phantom gaps. (envelope.proto's "per-(source,
// partition_key)" comment predates the per-event_type counter; the
// implementation is authoritative.)
type StreamKey struct {
	Source       string
	EventType    string
	PartitionKey string
}

// GapResult is the outcome of one Observe call.
type GapResult struct {
	Key      StreamKey
	Status   Status
	Sequence uint64 // the observed producer_sequence
	Previous uint64 // the high-water mark before this observation (0 if first)

	// MissingFrom / MissingTo bound the missing sequences, inclusive.
	// Valid only when Status == StatusGap.
	MissingFrom uint64
	MissingTo   uint64
}

// MissingCount is how many sequence numbers the gap spans (0 unless
// Status == StatusGap).
func (r GapResult) MissingCount() uint64 {
	if r.Status != StatusGap {
		return 0
	}
	return r.MissingTo - r.MissingFrom + 1
}

// GapDetector tracks the high-water producer_sequence per stream and
// reports gaps as events are observed. Safe for concurrent use — a
// consumer fanning partitions across goroutines shares one detector.
//
// State is in-memory and per-process: it reports gaps in the stream
// THIS detector observes, from the first event it sees. It does not
// persist across restarts (a restart re-baselines via StatusFirstSeen)
// and does not coordinate across consumer instances — cross-instance
// gap accounting is a downstream concern (DATA-07 emits per-detector
// findings; an aggregator reconciles).
type GapDetector struct {
	mu       sync.Mutex
	lastSeen map[StreamKey]uint64
}

// NewGapDetector returns an empty detector.
func NewGapDetector() *GapDetector {
	return &GapDetector{lastSeen: make(map[StreamKey]uint64)}
}

// Observe checks one envelope against the sequence history for its
// stream and returns the classification. A nil envelope is treated as
// not-applicable.
func (d *GapDetector) Observe(env *envelopepb.Envelope) GapResult {
	if env == nil {
		return GapResult{Status: StatusNotApplicable}
	}
	key := StreamKey{
		Source:       env.Source,
		EventType:    env.EventType,
		PartitionKey: env.PartitionKey,
	}
	seq := env.ProducerSequence

	if seq == 0 {
		return GapResult{Key: key, Status: StatusNotApplicable, Sequence: 0}
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	prev, seen := d.lastSeen[key]
	if !seen {
		d.lastSeen[key] = seq
		return GapResult{Key: key, Status: StatusFirstSeen, Sequence: seq}
	}

	switch {
	case seq == prev+1:
		d.lastSeen[key] = seq
		return GapResult{Key: key, Status: StatusOK, Sequence: seq, Previous: prev}
	case seq > prev+1:
		d.lastSeen[key] = seq // advance so the gap is reported once
		return GapResult{
			Key:         key,
			Status:      StatusGap,
			Sequence:    seq,
			Previous:    prev,
			MissingFrom: prev + 1,
			MissingTo:   seq - 1,
		}
	case seq == prev:
		// Keep the high-water mark; a duplicate does not move it.
		return GapResult{Key: key, Status: StatusDuplicate, Sequence: seq, Previous: prev}
	default: // seq < prev
		return GapResult{Key: key, Status: StatusRegression, Sequence: seq, Previous: prev}
	}
}
