package replay

import (
	"testing"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
)

func TestIsReplayed(t *testing.T) {
	if IsReplayed(nil) {
		t.Error("IsReplayed(nil) = true")
	}
	if IsReplayed(&envelopepb.Envelope{}) {
		t.Error("IsReplayed(empty) = true")
	}
	env := &envelopepb.Envelope{QualityFlags: []envelopepb.QualityFlag{
		envelopepb.QualityFlag_QUALITY_FLAG_LATE,
		envelopepb.QualityFlag_QUALITY_FLAG_REPLAYED,
	}}
	if !IsReplayed(env) {
		t.Error("IsReplayed(flagged) = false")
	}
}

func TestStampReplayedIdempotent(t *testing.T) {
	env := &envelopepb.Envelope{}
	StampReplayed(env)
	StampReplayed(env)
	StampReplayed(env)
	if len(env.QualityFlags) != 1 {
		t.Fatalf("QualityFlags=%v want one entry after triple-stamp", env.QualityFlags)
	}
	if env.QualityFlags[0] != envelopepb.QualityFlag_QUALITY_FLAG_REPLAYED {
		t.Errorf("flag=%v want REPLAYED", env.QualityFlags[0])
	}
}

func TestStampReplayedPreservesOtherFlags(t *testing.T) {
	env := &envelopepb.Envelope{QualityFlags: []envelopepb.QualityFlag{
		envelopepb.QualityFlag_QUALITY_FLAG_LATE,
	}}
	StampReplayed(env)
	if len(env.QualityFlags) != 2 {
		t.Fatalf("QualityFlags=%v want LATE + REPLAYED", env.QualityFlags)
	}
	if env.QualityFlags[0] != envelopepb.QualityFlag_QUALITY_FLAG_LATE ||
		env.QualityFlags[1] != envelopepb.QualityFlag_QUALITY_FLAG_REPLAYED {
		t.Errorf("flag order changed: %v", env.QualityFlags)
	}
}

func TestStampReplayedNilSafe(t *testing.T) {
	// Defensive: nil receiver must not panic.
	StampReplayed(nil)
}
