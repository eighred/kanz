package grpcsrv

import (
	"testing"

	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
)

// protoFlags used to be a two-case switch with no default, so any flag it
// had not been taught about was dropped on the floor. That is the #257
// failure mode relocated one layer out: the engine marks a response
// partial, the wire strips the mark, and the gRPC caller sees a clean
// number. Adding QualityFlagCurrencyExcluded to api/v1 without a case
// here would have done exactly that, silently.
//
// This pins that every flag the engine can produce survives translation.
func TestProtoFlags_EveryAPIFlagMaps(t *testing.T) {
	if len(v1.QualityFlags) == 0 {
		t.Fatal("v1.QualityFlags is empty — this test would pass vacuously")
	}
	for _, f := range v1.QualityFlags {
		got := protoFlags([]v1.QualityFlag{f})
		if len(got) != 1 {
			t.Errorf("protoFlags(%q) returned %d flags, want 1 — a flag was dropped "+
				"in translation, so the caller sees a response that no longer admits "+
				"what the engine knew about it", f, len(got))
			continue
		}
		if got[0] == querypb.QualityFlag_QUALITY_FLAG_UNSPECIFIED {
			t.Errorf("protoFlags(%q) = QUALITY_FLAG_UNSPECIFIED — %q has no case in "+
				"protoFlags\n\n"+
				"Add one, and add the matching value to the QualityFlag enum in "+
				"kanz-schemas/proto/query/v1/risk_query.proto. UNSPECIFIED is the "+
				"loud-failure fallback, not an acceptable mapping: a caller filtering "+
				"on known flags would treat it as no flag at all.", f, f)
		}
	}
}

// The specific mapping #257 depends on. Named separately from the
// exhaustiveness sweep so a breakage says which flag, not just "one of them".
func TestProtoFlags_CurrencyExcludedReachesTheWire(t *testing.T) {
	got := protoFlags([]v1.QualityFlag{v1.QualityFlagCurrencyExcluded})
	want := querypb.QualityFlag_QUALITY_FLAG_CURRENCY_EXCLUDED
	if len(got) != 1 || got[0] != want {
		t.Errorf("protoFlags(CURRENCY_EXCLUDED)=%v want [%v] — a gRPC client gating "+
			"on risk numbers cannot see that the measures cover only part of the "+
			"book unless this flag crosses the wire", got, want)
	}
}
