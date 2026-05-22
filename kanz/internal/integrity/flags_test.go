package integrity_test

import (
	"testing"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	"github.com/kanz-eng/kanz/internal/integrity"
)

func TestMark_AddsFlagOnce(t *testing.T) {
	env := &envelopepb.Envelope{}
	if !integrity.Mark(env, envelopepb.QualityFlag_QUALITY_FLAG_SHED) {
		t.Fatal("Mark returned false on first add")
	}
	if integrity.Mark(env, envelopepb.QualityFlag_QUALITY_FLAG_SHED) {
		t.Error("Mark returned true on idempotent re-add")
	}
	if got := len(env.QualityFlags); got != 1 {
		t.Errorf("QualityFlags len=%d want 1", got)
	}
}

func TestMark_PreservesExistingFlags(t *testing.T) {
	env := &envelopepb.Envelope{
		QualityFlags: []envelopepb.QualityFlag{envelopepb.QualityFlag_QUALITY_FLAG_SYNTHETIC},
	}
	integrity.Mark(env, envelopepb.QualityFlag_QUALITY_FLAG_BACKFILLED)
	if len(env.QualityFlags) != 2 ||
		env.QualityFlags[0] != envelopepb.QualityFlag_QUALITY_FLAG_SYNTHETIC ||
		env.QualityFlags[1] != envelopepb.QualityFlag_QUALITY_FLAG_BACKFILLED {
		t.Errorf("flags=%v want [synthetic, backfilled] in order", env.QualityFlags)
	}
}

func TestMark_RejectsReplayed(t *testing.T) {
	env := &envelopepb.Envelope{}
	if integrity.Mark(env, envelopepb.QualityFlag_QUALITY_FLAG_REPLAYED) {
		t.Error("Mark stamped REPLAYED — must be owned by replay tooling only")
	}
	if len(env.QualityFlags) != 0 {
		t.Errorf("QualityFlags=%v want empty", env.QualityFlags)
	}
}

func TestMark_RejectsUnspecified(t *testing.T) {
	env := &envelopepb.Envelope{}
	if integrity.Mark(env, envelopepb.QualityFlag_QUALITY_FLAG_UNSPECIFIED) {
		t.Error("Mark stamped UNSPECIFIED")
	}
	if len(env.QualityFlags) != 0 {
		t.Errorf("QualityFlags=%v want empty", env.QualityFlags)
	}
}

func TestMark_NilSafe(t *testing.T) {
	if integrity.Mark(nil, envelopepb.QualityFlag_QUALITY_FLAG_SHED) {
		t.Error("Mark(nil) returned true")
	}
	if integrity.Has(nil, envelopepb.QualityFlag_QUALITY_FLAG_SHED) {
		t.Error("Has(nil) returned true")
	}
}

func TestNamedWrappers_SetExpectedFlag(t *testing.T) {
	cases := []struct {
		name string
		mark func(*envelopepb.Envelope) bool
		flag envelopepb.QualityFlag
	}{
		{"synthetic", integrity.MarkSynthetic, envelopepb.QualityFlag_QUALITY_FLAG_SYNTHETIC},
		{"backfilled", integrity.MarkBackfilled, envelopepb.QualityFlag_QUALITY_FLAG_BACKFILLED},
		{"degraded", integrity.MarkDegraded, envelopepb.QualityFlag_QUALITY_FLAG_DEGRADED},
		{"revised", integrity.MarkRevised, envelopepb.QualityFlag_QUALITY_FLAG_REVISED},
		{"shed", integrity.MarkShed, envelopepb.QualityFlag_QUALITY_FLAG_SHED},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := &envelopepb.Envelope{}
			if !tc.mark(env) {
				t.Fatal("wrapper returned false on first add")
			}
			if !integrity.Has(env, tc.flag) {
				t.Errorf("flag %v not set", tc.flag)
			}
		})
	}
}

// MarkLate (DATA-03) now delegates to Mark — verify the consolidation kept
// its idempotent, flag-preserving behavior.
func TestMarkLate_DelegatesToMark(t *testing.T) {
	env := &envelopepb.Envelope{
		QualityFlags: []envelopepb.QualityFlag{envelopepb.QualityFlag_QUALITY_FLAG_SYNTHETIC},
	}
	integrity.MarkLate(env)
	integrity.MarkLate(env) // idempotent
	if !integrity.IsLate(env) {
		t.Error("MarkLate did not set LATE")
	}
	if len(env.QualityFlags) != 2 {
		t.Errorf("QualityFlags len=%d want 2 (synthetic preserved + late)", len(env.QualityFlags))
	}
}
