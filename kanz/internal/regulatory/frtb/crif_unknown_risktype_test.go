package frtb

import (
	"errors"
	"strings"
	"testing"
)

// A CRIF ROW NOBODY MAPPED MUST NOT VANISH (#623).
//
// mapSensitivities did `class, ok := classOf[r.RiskType]; if !ok { continue }`.
// A row whose RiskType is absent from the tables never became a Sensitivity, so
// Charge never saw it and #565's ErrUncovered — which exists precisely to stop a
// class being silently dropped from a filing — could not fire. The gate was
// downstream of the hole.
//
// The consequence is one-directional and in the filer's favour: a vendor adding a
// risk type, or a typo in an export, REMOVES CAPITAL from a signed regulatory
// report with no signal anywhere. sbm.go states the opposite principle eleven
// lines from here: "IT RETURNS AN ERROR RATHER THAN SKIPPING WHAT IT CANNOT PRICE
// (#565)."
//
// # The closed world is now written down
//
// Some rows genuinely do not belong to the SBM mappers — margin-only rows, and
// DRC/RRAO rows charged elsewhere. That was a real justification and it was an
// UNENFORCED one: nothing distinguished "known, and handled elsewhere" from
// "never heard of it". crifHandledElsewhere is that distinction, and every entry
// has to be added by someone who decided.

func rec(riskType, qualifier string, amount float64) CRIFRecord {
	return CRIFRecord{RiskType: riskType, Qualifier: qualifier, Amount: amount}
}

// A vendor adds a risk type. It must stop the filing, not shrink it.
func TestAnUnknownRiskTypeIsRefusedRatherThanDropped(t *testing.T) {
	recs := []CRIFRecord{
		rec("Risk_IRCurve", "USD", 1500),
		rec("Risk_SomethingNew", "XYZ", 9_000_000),
	}

	_, err := DeltaSensitivities(recs)
	if err == nil {
		t.Fatal("an unmapped RiskType was dropped silently — that removes capital from a signed " +
			"filing with no signal, which is exactly what #565 stopped one layer down")
	}
	if !errors.Is(err, ErrUnknownRiskType) {
		t.Fatalf("err = %v, want ErrUnknownRiskType", err)
	}
	// The message must name the offending type, or an operator cannot act on it.
	if !strings.Contains(err.Error(), "Risk_SomethingNew") {
		t.Fatalf("err %q does not name the unmapped RiskType", err)
	}
}

// VEGA ROWS ARE NOT UNKNOWN TO THE DELTA MAPPER, and getting this wrong would
// make every real CRIF fail. A row belongs to the vocabulary if EITHER family
// maps it; the delta mapper simply does not emit the vega ones.
func TestAVegaRowIsNotUnknownToTheDeltaMapper(t *testing.T) {
	recs := []CRIFRecord{
		rec("Risk_IRCurve", "USD", 1500),
		rec("Risk_EquityVol", "AAPL", 300),
	}

	delta, err := DeltaSensitivities(recs)
	if err != nil {
		t.Fatalf("a vega row must not be unknown to the delta mapper: %v", err)
	}
	if len(delta) != 1 || delta[0].RiskClass != "GIRR" {
		t.Fatalf("delta = %+v, want exactly the one GIRR row", delta)
	}

	vega, err := VegaSensitivities(recs)
	if err != nil {
		t.Fatalf("a delta row must not be unknown to the vega mapper: %v", err)
	}
	if len(vega) != 1 || vega[0].RiskClass != "Equity" {
		t.Fatalf("vega = %+v, want exactly the one Equity row", vega)
	}
}

// A row DELIBERATELY handled elsewhere is skipped without complaint — the
// original justification, now enforced rather than assumed.
func TestARowHandledElsewhereIsSkippedNotRefused(t *testing.T) {
	recs := []CRIFRecord{
		rec("Risk_IRCurve", "USD", 1500),
		rec("Risk_XCcyBasis", "USD", 99), // margin-only
	}

	delta, err := DeltaSensitivities(recs)
	if err != nil {
		t.Fatalf("a known margin-only row must not fail the mapping: %v", err)
	}
	if len(delta) != 1 {
		t.Fatalf("delta = %+v, want only the GIRR row", delta)
	}
}

// EVERY EXEMPTED TYPE CARRIES ITS REASON. An allowlist of bare strings is how
// "handled elsewhere" becomes "nobody remembers" — the state this replaced.
func TestEveryHandledElsewhereEntryStatesWhy(t *testing.T) {
	if len(crifHandledElsewhere) == 0 {
		t.Fatal("crifHandledElsewhere is empty — then every unmapped row errors, including the " +
			"margin-only rows ParseCRIF is documented to accept")
	}
	for riskType, reason := range crifHandledElsewhere {
		if len(strings.TrimSpace(reason)) < 20 {
			t.Errorf("crifHandledElsewhere[%q] = %q — too short to be a reason somebody can check",
				riskType, reason)
		}
		if _, both := crifDeltaClass[riskType]; both {
			t.Errorf("%q is both mapped to a delta class and listed as handled elsewhere", riskType)
		}
		if _, both := crifVegaClass[riskType]; both {
			t.Errorf("%q is both mapped to a vega class and listed as handled elsewhere", riskType)
		}
	}
}
