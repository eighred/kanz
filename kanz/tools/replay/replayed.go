package replay

import envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

// StampReplayed sets QUALITY_FLAG_REPLAYED on the envelope's QualityFlags
// slice if it is not already present. Idempotent: a Pipeline that re-runs
// over its own output (degenerate, but possible) does not double-stamp.
// Pipeline calls this once per event before re-marshaling the EventFrame so
// the wire bytes carry the flag — that is the producer-side half of EVT-20c.
func StampReplayed(env *envelopepb.Envelope) {
	if env == nil || IsReplayed(env) {
		return
	}
	env.QualityFlags = append(env.QualityFlags, envelopepb.QualityFlag_QUALITY_FLAG_REPLAYED)
}

// IsReplayed reports whether the envelope carries QUALITY_FLAG_REPLAYED.
// Replay-scoped consumers can use this as a predicate; the canonical
// rejection mechanism is bus.WithValidator(bus.ValidateReplay).
func IsReplayed(env *envelopepb.Envelope) bool {
	if env == nil {
		return false
	}
	for _, qf := range env.QualityFlags {
		if qf == envelopepb.QualityFlag_QUALITY_FLAG_REPLAYED {
			return true
		}
	}
	return false
}
