package consume

import (
	"testing"
	"time"

	altpb "github.com/kanz-eng/kanz-schemas-go/alternatives/v1"
	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	alt "github.com/kanz-eng/kanz/internal/alternatives"
)

func t0() time.Time { return time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC) }

func TestDecodeProto_CapitalCall(t *testing.T) {
	body, err := proto.Marshal(&altpb.CapitalCall{
		CallId:       "call-1",
		CommitmentId: "c-1",
		Amount:       &commonpb.Decimal{Coefficient: 250000, Exponent: 0},
		CallDate:     timestamppb.New(t0()),
	})
	if err != nil {
		t.Fatal(err)
	}
	e, err := DecodeProto(alt.SubjectCalled)(body)
	if err != nil {
		t.Fatalf("DecodeProto: %v", err)
	}
	if e.EventID != "call-1" || e.CommitmentID != "c-1" {
		t.Fatalf("identity = %q/%q, want call-1/c-1", e.EventID, e.CommitmentID)
	}
	if e.Type != alt.EventCall {
		t.Fatalf("type = %v, want EventCall", e.Type)
	}
	if e.Amount.RatString() != "250000" {
		t.Fatalf("amount = %s, want 250000", e.Amount.RatString())
	}
}

// AN OUT-OF-RANGE EXPONENT MUST BE REFUSED, NOT COERCED.
//
// dec.FromProtoChecked exists precisely because this payload is untrusted wire
// input: an exponent outside +/-64 is not a small number, it is a number this
// platform cannot represent, and substituting zero would book a capital call of
// nothing while reporting success.
func TestDecodeProto_RefusesUnrepresentableAmount(t *testing.T) {
	body, err := proto.Marshal(&altpb.CapitalCall{
		CallId:       "call-2",
		CommitmentId: "c-1",
		Amount:       &commonpb.Decimal{Coefficient: 1, Exponent: 9999},
		CallDate:     timestamppb.New(t0()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeProto(alt.SubjectCalled)(body); err == nil {
		t.Fatal("an unrepresentable amount decoded without error — a call of an " +
			"amount we cannot represent must be refused, never silently zeroed")
	}
}

func TestDecodeProto_UnknownSubjectIsRefused(t *testing.T) {
	if _, err := DecodeProto("alternatives.commitment.invented")(nil); err == nil {
		t.Fatal("an unknown event type produced a decoder instead of an error")
	}
}

func TestDecodeProto_Commitment(t *testing.T) {
	body, err := proto.Marshal(&altpb.Commitment{
		CommitmentId:    "c-1",
		FundId:          "f-1",
		InvestorId:      "inv-1",
		CommittedAmount: &commonpb.Decimal{Coefficient: 1000000, Exponent: 0},
		CommitmentDate:  timestamppb.New(t0()),
	})
	if err != nil {
		t.Fatal(err)
	}
	e, err := DecodeProto(alt.SubjectCommitted)(body)
	if err != nil {
		t.Fatalf("DecodeProto: %v", err)
	}
	if e.EventID != "c-1" || e.CommitmentID != "c-1" {
		t.Fatalf("identity = %q/%q, want c-1/c-1", e.EventID, e.CommitmentID)
	}
	if e.Type != alt.EventCommit {
		t.Fatalf("type = %v, want EventCommit", e.Type)
	}
	if e.Amount.RatString() != "1000000" {
		t.Fatalf("amount = %s, want 1000000", e.Amount.RatString())
	}
}

func TestDecodeProto_Distribution(t *testing.T) {
	body, err := proto.Marshal(&altpb.Distribution{
		DistributionId:   "dist-1",
		CommitmentId:     "c-1",
		Amount:           &commonpb.Decimal{Coefficient: 50000, Exponent: 0},
		Kind:             altpb.DistributionKind_DISTRIBUTION_KIND_RETURN_OF_CAPITAL,
		DistributionDate: timestamppb.New(t0()),
	})
	if err != nil {
		t.Fatal(err)
	}
	e, err := DecodeProto(alt.SubjectDistributed)(body)
	if err != nil {
		t.Fatalf("DecodeProto: %v", err)
	}
	if e.EventID != "dist-1" || e.CommitmentID != "c-1" {
		t.Fatalf("identity = %q/%q, want dist-1/c-1", e.EventID, e.CommitmentID)
	}
	if e.Type != alt.EventDistribution {
		t.Fatalf("type = %v, want EventDistribution", e.Type)
	}
	if e.Amount.RatString() != "50000" {
		t.Fatalf("amount = %s, want 50000", e.Amount.RatString())
	}
}

// NAVMark carries an explicit mark_id (see alternatives.proto), exactly as its
// three sibling messages carry commitment_id/call_id/distribution_id. The
// decoder must pass it straight through as EventID — no synthesis — so that
// redelivery of the SAME mark is idempotent (Position.Apply dedupes on
// EventID) while a LATER, distinctly-identified mark on the same commitment
// is never mistaken for a duplicate of an earlier one.
func TestDecodeProto_NAVMark(t *testing.T) {
	body, err := proto.Marshal(&altpb.NAVMark{
		MarkId:       "mark-1",
		CommitmentId: "c-1",
		Nav:          &commonpb.Decimal{Coefficient: 900000, Exponent: 0},
		AsOf:         timestamppb.New(t0()),
	})
	if err != nil {
		t.Fatal(err)
	}
	e, err := DecodeProto(alt.SubjectMarked)(body)
	if err != nil {
		t.Fatalf("DecodeProto: %v", err)
	}
	if e.EventID != "mark-1" {
		t.Fatalf("EventID = %q, want mark-1", e.EventID)
	}
	if e.CommitmentID != "c-1" {
		t.Fatalf("commitment = %q, want c-1", e.CommitmentID)
	}
	if e.Type != alt.EventNAVMark {
		t.Fatalf("type = %v, want EventNAVMark", e.Type)
	}
	if e.Amount.RatString() != "900000" {
		t.Fatalf("amount = %s, want 900000", e.Amount.RatString())
	}

	// A second decode of the identical mark must produce the identical EventID
	// (redelivery is a no-op fold), but a mark carrying a different mark_id
	// must produce a different EventID (a corrected restatement is a new
	// fact, not a dup).
	e2, err := DecodeProto(alt.SubjectMarked)(body)
	if err != nil {
		t.Fatalf("DecodeProto (redelivery): %v", err)
	}
	if e2.EventID != e.EventID {
		t.Fatalf("redelivered mark got a different EventID: %q vs %q", e2.EventID, e.EventID)
	}

	laterBody, err := proto.Marshal(&altpb.NAVMark{
		MarkId:       "mark-2",
		CommitmentId: "c-1",
		Nav:          &commonpb.Decimal{Coefficient: 950000, Exponent: 0},
		AsOf:         timestamppb.New(t0().AddDate(0, 1, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}
	e3, err := DecodeProto(alt.SubjectMarked)(laterBody)
	if err != nil {
		t.Fatalf("DecodeProto (later mark): %v", err)
	}
	if e3.EventID == e.EventID {
		t.Fatalf("a later mark on the same commitment collided on EventID %q", e.EventID)
	}
}

// A NAVMark WITHOUT a mark_id must be refused, not silently accepted with a
// synthesized id. This is the whole reason mark_id exists: before it, the
// decoder synthesized an id from commitment_id + as_of, so a corrected NAV
// restatement for the SAME as_of date (routine when a GP restates a quarter)
// was indistinguishable from a redelivery of the original mark, and the
// fold's `ON CONFLICT (tenant_id, event_id) DO NOTHING` silently dropped the
// correction — reporting success while leaving a stale valuation in place.
// Requiring mark_id here, and refusing its absence, closes that hole.
func TestDecodeProto_NAVMarkWithoutIdIsRefused(t *testing.T) {
	body, err := proto.Marshal(&altpb.NAVMark{
		CommitmentId: "c-1",
		Nav:          &commonpb.Decimal{Coefficient: 900000, Exponent: 0},
		AsOf:         timestamppb.New(t0()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeProto(alt.SubjectMarked)(body); err == nil {
		t.Fatal("a NAVMark with no mark_id decoded without error — without an " +
			"explicit id, a corrected restatement for the same as_of is " +
			"indistinguishable from a redelivery of the original and is silently " +
			"swallowed by the fold's ON CONFLICT DO NOTHING, leaving a stale " +
			"valuation while reporting success")
	}
}
