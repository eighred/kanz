package integrity

import envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

// Mark sets a data-integrity quality flag on the envelope's QualityFlags
// slice if not already present. Idempotent, nil-safe, and order-preserving
// — the package convention for stamping a QualityFlag (cf.
// replay.StampReplayed, and MarkLate below which now delegates here).
// Returns true if the flag was newly added.
//
// This is the producer/ingestion-side half of the detect-then-emit split:
// the detectors (DATA-01 gap, DATA-02 staleness, DATA-03 watermark, DATA-04
// drift, DATA-05 reconciliation) classify a condition; the producer or
// ingestor that owns the envelope stamps the corresponding flag here so
// payload-blind tooling — DATA-07 reporting, the data-observability layer,
// any consumer — sees the condition off the envelope without re-running
// detection or parsing the payload (envelope.proto §quality_flags).
//
// Two flags are deliberately not stampable here:
//   - QUALITY_FLAG_REPLAYED is owned exclusively by replay tooling
//     (replay.StampReplayed) and forbidden on the live publish path
//     (bus.Validate). Stamping it from ingestion/producers would let a live
//     event masquerade as replayed and corrupt the live/replay boundary, so
//     Mark refuses it.
//   - QUALITY_FLAG_UNSPECIFIED is the zero value, never set explicitly.
//
// Both are no-ops returning false rather than errors: a misrouted stamp
// must not abort the publish of an otherwise-valid event.
func Mark(env *envelopepb.Envelope, flag envelopepb.QualityFlag) bool {
	if env == nil ||
		flag == envelopepb.QualityFlag_QUALITY_FLAG_UNSPECIFIED ||
		flag == envelopepb.QualityFlag_QUALITY_FLAG_REPLAYED ||
		Has(env, flag) {
		return false
	}
	env.QualityFlags = append(env.QualityFlags, flag)
	return true
}

// Has reports whether the envelope already carries the given flag. Nil-safe.
func Has(env *envelopepb.Envelope, flag envelopepb.QualityFlag) bool {
	if env == nil {
		return false
	}
	for _, qf := range env.QualityFlags {
		if qf == flag {
			return true
		}
	}
	return false
}

// The named wrappers below are the conditions ingestion + producers stamp.
// Each is a thin, intent-revealing call to Mark so call sites read as the
// condition rather than the enum constant, and a future rename surfaces as
// a Go compile error rather than a silently-wrong flag.

// MarkSynthetic flags an event Kanz generated rather than observed from a
// source (QUALITY_FLAG_SYNTHETIC).
func MarkSynthetic(env *envelopepb.Envelope) bool {
	return Mark(env, envelopepb.QualityFlag_QUALITY_FLAG_SYNTHETIC)
}

// MarkBackfilled flags an event inserted after the fact to fill a known gap
// (QUALITY_FLAG_BACKFILLED) — the emission counterpart to a DATA-01 gap.
func MarkBackfilled(env *envelopepb.Envelope) bool {
	return Mark(env, envelopepb.QualityFlag_QUALITY_FLAG_BACKFILLED)
}

// MarkDegraded flags an event produced from a degraded or partial source
// (QUALITY_FLAG_DEGRADED) — the envelope-level signal a degraded prediction
// (PRED-02 §3) or risk output (RISK-11) sets so payload-blind observability
// sees it.
func MarkDegraded(env *envelopepb.Envelope) bool {
	return Mark(env, envelopepb.QualityFlag_QUALITY_FLAG_DEGRADED)
}

// MarkRevised flags an event that corrects a previously emitted event
// (QUALITY_FLAG_REVISED).
func MarkRevised(env *envelopepb.Envelope) bool {
	return Mark(env, envelopepb.QualityFlag_QUALITY_FLAG_REVISED)
}

// MarkShed flags an event whose stream was lossy at production time due to
// upstream backpressure shedding (QUALITY_FLAG_SHED).
func MarkShed(env *envelopepb.Envelope) bool {
	return Mark(env, envelopepb.QualityFlag_QUALITY_FLAG_SHED)
}
