package posttrade_test

import (
	"strings"
	"testing"
	"time"

	settlementpb "github.com/eighred/kanz/kanz-schemas-go/settlement/v1"
	"github.com/eighred/kanz/services/oms/internal/posttrade"
)

// EVERY SEVERITY THE AGING POLICY PRODUCES HAS A WIRE MAPPING (#589).
//
// AUTO-01 routes remediation on this enum. A tier with no mapping would either
// publish as the zero value — FAIL_SEVERITY_UNSPECIFIED, which reads as "not
// set" rather than "unknown tier" — or fail at publish time, at 3am, on the one
// event that matters. The table below is exhaustive over fails.go's constants,
// so adding a fifth tier without a mapping fails here.
func TestEncodeFailMapsEverySeverity(t *testing.T) {
	for _, tc := range []struct {
		sev  posttrade.Severity
		want settlementpb.FailSeverity
	}{
		{posttrade.SeverityWarning, settlementpb.FailSeverity_FAIL_SEVERITY_WARNING},
		{posttrade.SeverityCritical, settlementpb.FailSeverity_FAIL_SEVERITY_CRITICAL},
		{posttrade.SeverityEscalated, settlementpb.FailSeverity_FAIL_SEVERITY_ESCALATED},
	} {
		t.Run(tc.sev.String(), func(t *testing.T) {
			f := validFail("i1")
			f.Severity = tc.sev
			got, err := posttrade.EncodeFail(f)
			if err != nil {
				t.Fatalf("EncodeFail: %v", err)
			}
			if got.GetSeverity() != tc.want {
				t.Errorf("severity = %v want %v", got.GetSeverity(), tc.want)
			}
		})
	}
}

// THE SEVERITY SET IS EXHAUSTIVE. If a fifth tier is added to fails.go, this
// fails — rather than the mapping silently not covering it and the new tier
// erroring only when a real fail carrying it is published.
//
// Severity is an unexported-value iota with no Values() method, so the sweep is
// over the integer range: every value with a String() other than "none" must
// encode, and every one that reports "none" must not.
func TestEncodeFailSeveritySetIsExhaustive(t *testing.T) {
	named := 0
	for i := range 8 {
		s := posttrade.Severity(i)
		f := validFail("i1")
		f.Severity = s
		_, err := posttrade.EncodeFail(f)
		if s.String() == "none" {
			if err == nil {
				t.Errorf("Severity(%d) reports %q and still encoded — an unnamed tier must be "+
					"refused, not published as the zero enum", i, s.String())
			}
			continue
		}
		named++
		if err != nil {
			t.Errorf("Severity(%d) reports %q and has NO settlement.v1 mapping: %v", i, s.String(), err)
		}
	}
	if named != 3 {
		t.Fatalf("found %d named severities, expected 3 (warning/critical/escalated) — fails.go "+
			"gained or lost a tier and this mapping was not revisited", named)
	}
}

// A FAIL DETECTED IN ANY ZONE PUBLISHES THE SAME INSTANT.
//
// Two deployments in different zones must not disagree about when a settlement
// was due — IBOR-01e reconciles against settlement_date, and a date that shifted
// by the pod's TZ would break against a custodian's own records.
//
// THIS DOES NOT ASSERT THE DECODED LOCATION, and an earlier version did. That
// assertion could not fail: timestamppb carries an instant and no zone, so
// AsTime always returns UTC whatever was encoded — removing the .UTC()
// conversion from the encoder left the test green. What is worth asserting is
// that the INSTANT survives, which is what a wrong conversion would actually
// break.
func TestEncodeFailPublishesTheSameInstantFromAnyZone(t *testing.T) {
	east := time.FixedZone("UTC+9", 9*3600)
	west := time.FixedZone("UTC-5", -5*3600)
	settle := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	detect := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

	for _, zone := range []*time.Location{time.UTC, east, west} {
		f := validFail("i1")
		f.SettlementDate = settle.In(zone)
		f.DetectedAt = detect.In(zone)

		got, err := posttrade.EncodeFail(f)
		if err != nil {
			t.Fatalf("EncodeFail in %v: %v", zone, err)
		}
		if d := got.GetSettlementDate().AsTime(); !d.Equal(settle) {
			t.Errorf("settlement_date from %v = %v, want the instant %v", zone, d, settle)
		}
		if d := got.GetDetectedAt().AsTime(); !d.Equal(detect) {
			t.Errorf("detected_at from %v = %v, want the instant %v", zone, d, detect)
		}
	}
}

// A DETECTED FAIL ENCODES. The refusals above are for hand-assembled Fails; the
// ones DetectFails actually produces must all be publishable, or the plane would
// refuse its own output the day it is armed.
func TestEveryDetectedFailEncodes(t *testing.T) {
	settleDate := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	// Built directly rather than through NewInstruction: the fill fixture lives
	// in the internal test package, and every field DetectFails reads is
	// exported. NewInstruction already returns StatusAffirmed, so the only
	// transition needed is Instruct.
	var settlements []*posttrade.Settlement
	for _, id := range []string{"i1", "i2", "i3"} {
		settlements = append(settlements, &posttrade.Settlement{
			InstructionID:  id,
			InstrumentID:   "AAPL",
			Counterparty:   "CP-GOLDMAN",
			Custodian:      "CUST-BNY",
			Currency:       "USD",
			SettlementDate: settleDate,
			Status:         posttrade.StatusInstructed,
		})
	}
	// Far enough past the settlement date to reach every escalation tier.
	for _, days := range []int{3, 8, 30} {
		now := settleDate.AddDate(0, 0, days)
		fails := posttrade.DetectFails(settlements, posttrade.AgingPolicy{}, now)
		if len(fails) == 0 {
			t.Fatalf("no fails detected %d days past settlement", days)
		}
		for _, f := range fails {
			if _, err := posttrade.EncodeFail(f); err != nil {
				t.Errorf("a fail DetectFails produced cannot be published (%d days): %v\n  %+v",
					days, err, f)
			}
		}
	}
}

// The refusal names the field, because the caller's next question is which one.
func TestEncodeFailNamesTheMissingField(t *testing.T) {
	f := validFail("i1")
	f.InstrumentID = ""
	_, err := posttrade.EncodeFail(f)
	if err == nil {
		t.Fatal("a Fail with no instrument encoded")
	}
	if !strings.Contains(err.Error(), "instrument_id") {
		t.Errorf("error %q does not name the field", err)
	}
	if !strings.Contains(err.Error(), "i1") {
		t.Errorf("error %q does not name the instruction, so an operator cannot find it", err)
	}
}
