package governed

// Context-coverage tests (#973).
//
// #757 is the reason this exists: a measure whose integrity record was stripped
// reached an agent as if DV01 = 0 were a computed zero. The read plane withholds
// now — correctly — and that made the failure SILENT, because an answer hedged
// because the numbers were missing looks exactly like one hedged for no reason.

import (
	"testing"

	"github.com/eighred/kanz/internal/measureread"
)

func measured(name string, v float64) measureread.Measure {
	return measureread.Measure{Name: name, Status: measureread.StatusMeasured, Value: &v}
}

func withheld(name, reason string) measureread.Measure {
	return measureread.Measure{Name: name, Status: measureread.StatusUnavailable, Reason: reason}
}

// COVERAGE SPLITS THE READING THE WAY THE READ PLANE ALREADY DID.
func TestCoverageCountsMeasuredAndWithheldSeparately(t *testing.T) {
	r := Reading{Measures: []measureread.Measure{
		measured("VaR99", 1_250_000),
		withheld("DV01", "integrity record absent"),
		withheld("CS01", "no marks in the window"),
	}}
	gotM, gotU := r.Coverage()
	if gotM != 1 || gotU != 2 {
		t.Fatalf("Coverage() = (%d measured, %d withheld), want (1, 2). The agent was shown three "+
			"measures and could state one of them; a count that collapses that is the #757 gap "+
			"with a metric in front of it.", gotM, gotU)
	}
}

// THE SPLIT IS DERIVED FROM Status, NOT FROM Value BEING NIL.
//
// measureread draws that line once, with a Reason attached. A second derivation
// here is how the two drift, and the direction it drifts is always the same: a
// WITHHELD measure that happens to carry a value starts being counted as present,
// which makes the coverage metric under-report exactly the gap it exists to show.
func TestAWithheldMeasureIsNotCountedEvenIfItCarriesAValue(t *testing.T) {
	v := 0.0
	r := Reading{Measures: []measureread.Measure{
		{Name: "DV01", Status: measureread.StatusUnavailable, Reason: "computed over no bonds", Value: &v},
	}}
	gotM, gotU := r.Coverage()
	if gotM != 0 || gotU != 1 {
		t.Fatalf("Coverage() = (%d, %d), want (0 measured, 1 withheld). This measure carries 0.0 and "+
			"the read plane refuses to state it — which is #757 exactly: DV01 = 0 as a computed "+
			"zero rather than as 'computed over no bonds'. Classify by Status.", gotM, gotU)
	}
}

// COVERAGE AND StatedValues MUST AGREE. They answer the same question — may this
// number be stated — and if the count says three and the gate cites two, the
// metric describes a reading the model was never given.
func TestCoverageAgreesWithStatedValues(t *testing.T) {
	r := Reading{Measures: []measureread.Measure{
		measured("VaR99", 1), measured("VaR95", 2),
		withheld("DV01", "absent"),
	}}
	gotM, _ := r.Coverage()
	if n := len(r.StatedValues()); gotM != n {
		t.Fatalf("Coverage() reports %d measured and StatedValues() returns %d. These are one "+
			"judgement read twice; when they disagree the coverage metric is describing a "+
			"reading the model did not receive.", gotM, n)
	}
}

// AN EMPTY READING IS ZERO AND ZERO, not a withheld one. A tool that returned
// nothing at all must not inflate the withheld share and page CopilotContextWithheld
// on a portfolio nobody asked about.
func TestAnEmptyReadingCountsNothing(t *testing.T) {
	gotM, gotU := Reading{}.Coverage()
	if gotM != 0 || gotU != 0 {
		t.Fatalf("Coverage() on an empty reading = (%d, %d), want (0, 0)", gotM, gotU)
	}
}
