package posttrade

import (
	"fmt"

	settlementpb "github.com/eighred/kanz/kanz-schemas-go/settlement/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// THE FAIL PAYLOAD IS ENCODED HERE, NOT INJECTED (#589).
//
// This mapping used to be a FailEncoder seam the composition root was supposed
// to fill, justified by a comment reading "settlement.v1 is generated-not-
// committed (EVT-15a — it is NOT in the checked-in gen/go tree, only its
// .proto)". THE PREMISE WAS FALSE, and #589's body already said so:
//
//   - kanz-schemas/gen is not checked in AT ALL — `git ls-files kanz-schemas/gen`
//     returns nothing. EVERY generated package is generated-not-committed, so
//     settlement.v1 is not special in any way.
//   - this package already imports four of them — order/v1, common/v1,
//     envelope/v1 and observation/v1. settle.go's NewInstruction takes an
//     *orderpb.Fill. The seam's stated reason contradicted the package's own
//     import block.
//
// So the seam was an indirection with exactly ONE possible implementation, whose
// justification did not hold, and which a composition root had to supply before
// a BusFailSink could exist at all. CLAUDE.md's rule is one implementation per
// concept; this is that one implementation, in the package that owns the type.
//
// # It does not arm anything
//
// Closing the seam does NOT start the post-trade plane, and must not be read as
// doing so. Nothing constructs a Settlement, so nothing detects a fail, so
// nothing calls this — see settlement_posture.go in the OMS composition root,
// which reports every stage as not-running and names the counterparty
// confirmation feed as what would arm them. What changes here is that the next
// person to arm it has one less false blocker in front of them.
//
// # Every field is required, and an unencodable Fail is REFUSED
//
// settlement.v1.SettlementFail marks all eight fields Required. A FACT is the
// permanent record that a settlement failed; one published with an empty
// instruction id or a zero settlement date is a record nobody can act on or
// reconcile against, and it would sit in the SETTLEMENT stream looking like
// evidence. Refusing the encode aborts the publish and surfaces the fault at the
// producer instead.

// EncodeFail maps a Fail onto the settlement.v1.SettlementFail FACT.
//
// It refuses rather than emitting a partial record — see the file header. The
// returned error names the field, because the caller's next question is always
// which one.
func EncodeFail(f Fail) (*settlementpb.SettlementFail, error) {
	if f.InstructionID == "" {
		return nil, fmt.Errorf("posttrade: encode fail: instruction_id is empty; a settlement " +
			"fail FACT that references no instruction cannot be remediated or reconciled")
	}
	if f.InstrumentID == "" {
		return nil, fmt.Errorf("posttrade: encode fail %s: instrument_id is empty", f.InstructionID)
	}
	if f.Counterparty == "" {
		return nil, fmt.Errorf("posttrade: encode fail %s: counterparty is empty; IBOR-01e "+
			"reconciliation keys on it", f.InstructionID)
	}
	if f.Reason == "" {
		return nil, fmt.Errorf("posttrade: encode fail %s: reason is empty", f.InstructionID)
	}
	if f.SettlementDate.IsZero() {
		return nil, fmt.Errorf("posttrade: encode fail %s: settlement_date is zero; age_days is "+
			"measured against it, so a zero date makes the age meaningless", f.InstructionID)
	}
	if f.DetectedAt.IsZero() {
		return nil, fmt.Errorf("posttrade: encode fail %s: detected_at is zero", f.InstructionID)
	}
	// A NEGATIVE AGE IS NOT A FAIL. age_days counts days PAST the settlement
	// date; a negative one means the instruction is not yet due, and publishing
	// it would tell AUTO-01 to remediate a settlement that still has time to
	// settle normally.
	if f.AgeDays < 0 {
		return nil, fmt.Errorf("posttrade: encode fail %s: age_days is %d; a settlement that has "+
			"not reached its settlement date has not failed", f.InstructionID, f.AgeDays)
	}
	sev, err := encodeSeverity(f.Severity, f.InstructionID)
	if err != nil {
		return nil, err
	}
	return &settlementpb.SettlementFail{
		InstructionId: f.InstructionID,
		InstrumentId:  f.InstrumentID,
		Counterparty:  f.Counterparty,
		// NO .UTC() ON EITHER, deliberately. timestamppb carries an INSTANT —
		// seconds and nanos since the epoch — and no zone, so converting first
		// changes nothing on the wire. It was written here and a test asserted the
		// decoded Location was UTC, which is true of every timestamppb value and
		// therefore proved nothing: removing the conversion left that test green.
		SettlementDate: timestamppb.New(f.SettlementDate),
		AgeDays:        int32(f.AgeDays), //nolint:gosec // bounded by the age check above and by AgingPolicy
		Severity:       sev,
		Reason:         f.Reason,
		DetectedAt:     timestamppb.New(f.DetectedAt),
	}, nil
}

// encodeSeverity maps the working Severity onto the wire enum.
//
// SeverityNone IS REFUSED, not mapped to UNSPECIFIED. DetectFails never returns
// a Fail carrying it — an explicit StatusFailed is floored at Warning, and an
// aged instruction below the grace window is not returned at all — so a None
// arriving here means the caller assembled a Fail by hand and left the tier
// unset. Publishing it as UNSPECIFIED would hand AUTO-01 a fail with no
// escalation tier to route on, which it would read as the zero value rather than
// as "unknown".
func encodeSeverity(s Severity, instructionID string) (settlementpb.FailSeverity, error) {
	switch s {
	case SeverityWarning:
		return settlementpb.FailSeverity_FAIL_SEVERITY_WARNING, nil
	case SeverityCritical:
		return settlementpb.FailSeverity_FAIL_SEVERITY_CRITICAL, nil
	case SeverityEscalated:
		return settlementpb.FailSeverity_FAIL_SEVERITY_ESCALATED, nil
	case SeverityNone:
		return 0, fmt.Errorf("posttrade: encode fail %s: severity is none; a fail with no "+
			"escalation tier gives AUTO-01 nothing to route on", instructionID)
	default:
		return 0, fmt.Errorf("posttrade: encode fail %s: severity %d is not a known tier",
			instructionID, int(s))
	}
}
