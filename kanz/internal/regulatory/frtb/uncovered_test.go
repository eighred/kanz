package frtb

import (
	"errors"
	"math"
	"testing"
)

// A CLASS OR BUCKET THE TABLE DOES NOT COVER IS A REFUSAL, NOT A ZERO (#565).
//
// Charge used to `continue` past any risk class missing from the supervisory
// table, so the class contributed nothing and nothing said so. The result is a
// one-directional understatement — always smaller, never larger — in the
// direction a filer benefits from, on a report that gets signed.
//
// It is the estate's stated standard failing in the direction it names: a book
// with no commodity risk and a table missing Commodity produced the same number.

const covered = "Equity"

func coveredParams() Params {
	return Params{covered: ClassParams{
		RiskWeight: map[string]float64{"1": 1, "2": 1},
		IntraCorr:  0.5,
		InterCorr:  0.5,
	}}
}

func TestChargeRefusesARiskClassTheTableDoesNotCover(t *testing.T) {
	sens := []Sensitivity{
		{RiskClass: covered, Bucket: "1", Factor: "A", Amount: 5},
		{RiskClass: "Commodity", Bucket: "1", Factor: "B", Amount: 100},
	}

	got, err := Charge(sens, coveredParams())

	if err == nil {
		t.Fatalf("Charge returned %v and no error for a book carrying Commodity risk the table "+
			"does not price.\n\nThat number is indistinguishable from the same book with no "+
			"commodity risk at all, and it is smaller than the truth — which is the direction "+
			"that gets signed rather than questioned.", got)
	}
	if !errors.Is(err, ErrUncovered) {
		t.Errorf("error is not ErrUncovered: %v — callers cannot distinguish an uncovered table "+
			"from an arithmetic failure", err)
	}
	// THE CLASS MUST BE NAMED. "Uncovered" alone sends a filer to diff two tables.
	if !contains(err.Error(), "Commodity") {
		t.Errorf("error does not name the offending class: %v", err)
	}
	if got != 0 {
		t.Errorf("Charge returned %v alongside an error — a partial capital number is the thing "+
			"most likely to be used anyway", got)
	}
}

// THE QUIETER HALF, and the one a class-level check alone would miss: the class
// is covered, one of its BUCKETS is not. rw[bucket] on a missing key is 0, so
// the sensitivity is weighted to nothing and vanishes inside an aggregate that
// is otherwise computed correctly — no branch is skipped, so nothing looks wrong.
func TestChargeRefusesABucketTheTableDoesNotCover(t *testing.T) {
	sens := []Sensitivity{
		{RiskClass: covered, Bucket: "1", Factor: "A", Amount: 5},
		{RiskClass: covered, Bucket: "99", Factor: "B", Amount: 100},
	}

	got, err := Charge(sens, coveredParams())

	if err == nil {
		t.Fatalf("Charge returned %v for a covered class with an UNCOVERED bucket. The bucket-99 "+
			"sensitivity was weighted by a missing risk weight — zero — and disappeared into a "+
			"correctly computed aggregate. A class-level check alone would not have caught this.", got)
	}
	if !errors.Is(err, ErrUncovered) {
		t.Errorf("error is not ErrUncovered: %v", err)
	}
	if !contains(err.Error(), "99") {
		t.Errorf("error does not name the offending bucket: %v", err)
	}
}

func TestCurvatureChargeRefusesARiskClassTheTableDoesNotCover(t *testing.T) {
	sens := []CurvatureSensitivity{
		{RiskClass: covered, Bucket: "1", Up: 1, Down: -1},
		{RiskClass: "Commodity", Bucket: "1", Up: 50, Down: -50},
	}

	if got, err := CurvatureCharge(sens, coveredParams()); err == nil {
		t.Fatalf("CurvatureCharge returned %v for an uncovered class — same silent understatement "+
			"as the delta path, in a leg that is totalled beside it", got)
	} else if !errors.Is(err, ErrUncovered) {
		t.Errorf("error is not ErrUncovered: %v", err)
	}
}

// AND A FULLY COVERED BOOK STILL PRICES. Without this the refusals above are
// satisfied by a Charge that refuses everything, which would be a different and
// louder defect but a defect all the same.
func TestAFullyCoveredBookStillProducesACharge(t *testing.T) {
	sens := []Sensitivity{
		{RiskClass: covered, Bucket: "1", Factor: "A", Amount: 5},
		{RiskClass: covered, Bucket: "2", Factor: "B", Amount: 5},
	}

	got, err := Charge(sens, coveredParams())
	if err != nil {
		t.Fatalf("a book whose every class and bucket is priced was refused: %v", err)
	}
	if got <= 0 || math.IsNaN(got) {
		t.Fatalf("charge = %v, want a positive number", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// mustCharge is Charge for cases whose table covers their inputs by construction.
//
// IT FATALS RATHER THAN DISCARDING THE ERROR (#565). Charge now refuses an
// uncovered class or bucket, and in these tests the table is written beside the
// sensitivities — so a refusal means the CASE is malformed, and swallowing it
// would let a test go on asserting a number Charge never produced.
func mustCharge(t *testing.T, sens []Sensitivity, p Params) float64 {
	t.Helper()
	v, err := Charge(sens, p)
	if err != nil {
		t.Fatalf("Charge refused a fixture whose table should cover it: %v", err)
	}
	return v
}

func mustCurvature(t *testing.T, sens []CurvatureSensitivity, p Params) float64 {
	t.Helper()
	v, err := CurvatureCharge(sens, p)
	if err != nil {
		t.Fatalf("CurvatureCharge refused a fixture whose table should cover it: %v", err)
	}
	return v
}
