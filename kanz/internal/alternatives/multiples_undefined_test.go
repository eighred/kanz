package alternatives

import (
	"errors"
	"testing"
)

// AN UNDEFINED MULTIPLE IS NOT A MULTIPLE OF ZERO (#623).
//
// ComputeMultiples returned Multiples{} — TVPI, DPI and RVPI all 0.0 — for a
// commitment with nothing drawn, and the doc justified it as "the same
// degraded-to-zero discipline the risk measures use". The premise fails against
// the sibling metric in the same response body: IRR handles its undefined case by
// returning an error, and the handler OMITS the key. Five lines apart, two
// treatments of the same question.
//
// The two states a zero conflates are not close together:
//
//	nothing drawn yet      the multiple does not exist
//	drew and lost it all   TVPI 0.0, and that is a measured fact
//
// One is a fund that has not started. The other is a total loss. They are the
// same three bytes on the wire.

func TestUndefinedMultiplesAreRefusedNotReportedAsZero(t *testing.T) {
	p := Replay("C1", []*Event{ev("e1", EventCommit, "1000", day(2020, 1, 1))})

	_, err := ComputeMultiples(p)
	if err == nil {
		t.Fatal("a commitment with nothing drawn returned multiples — a fund that has not started " +
			"and one that lost everything must not both report 0.0")
	}
	if !errors.Is(err, ErrNoMultiples) {
		t.Fatalf("err = %v, want ErrNoMultiples", err)
	}
}

// A TOTAL LOSS IS A MEASUREMENT AND MUST STILL BE REPORTED. The repair must not
// swallow the case it exists to distinguish: capital drawn and nothing returned
// is TVPI 0.0, and refusing that would be the same collapse in the other
// direction.
func TestATotalLossStillReportsZeroMultiples(t *testing.T) {
	p := Replay("C1", []*Event{
		ev("e1", EventCommit, "1000", day(2020, 1, 1)),
		ev("e2", EventCall, "500", day(2020, 6, 1)),
	})

	m, err := ComputeMultiples(p)
	if err != nil {
		t.Fatalf("capital was drawn, so the multiples are defined: %v", err)
	}
	if m.TVPI != 0 || m.DPI != 0 || m.RVPI != 0 {
		t.Fatalf("drew 500 and returned nothing should be 0 across the board, got %+v", m)
	}
}

// The ordinary path is unchanged.
func TestDefinedMultiplesAreUnchanged(t *testing.T) {
	p := Replay("C1", []*Event{
		ev("e1", EventCall, "100", day(2020, 1, 1)),
		ev("e2", EventDistribution, "60", day(2021, 1, 1)),
		ev("e3", EventNAVMark, "80", day(2022, 1, 1)),
	})
	m, err := ComputeMultiples(p)
	if err != nil {
		t.Fatal(err)
	}
	if !approx(m.DPI, 0.6, 1e-9) || !approx(m.RVPI, 0.8, 1e-9) || !approx(m.TVPI, 1.4, 1e-9) {
		t.Fatalf("multiples: %+v", m)
	}
}
