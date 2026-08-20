package termsource

import (
	"context"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"

	"github.com/eighred/kanz/internal/marketdata/terms"
	"github.com/eighred/kanz/internal/risk/compute"
)

// THE STRUCTURED SEAM AGAINST A REAL POSTGRES (#572) — the half
// structuredspec_test.go cannot cover, because the thing under test is that a
// securitization SURVIVES the round trip through the store: the oneof case, the
// kind label, the proto blob and the point-in-time read.
//
// Gated on TEST_POSTGRES_URL like the rest of provider_test.go. UNGATED IT WOULD
// BE THE FALSE GREEN THIS REPOSITORY KEEPS HITTING — skipping silently and
// reported as ok — so the conversion assertions live in the ungated file and
// only the store-shaped facts live here.

func structuredRec(id string, st *referencepb.StructuredTerms) terms.Record {
	return terms.Record{
		InstrumentID: id,
		AsOf:         t0,
		// THE DEAL ID IS THE GROUPING KEY for a tranche row, which is what makes
		// ChainAsOf(dealID, asOf, KindStructured) list a deal's capital structure.
		// underlying_id means "the id this row is grouped by", and what that is
		// depends on the variant — the underlying for an option, nothing for a
		// swap, the securitization for a tranche.
		UnderlyingID: st.GetDealId(),
		Kind:         terms.KindStructured,
		Terms: &referencepb.ContractTerms{
			InstrumentId: id,
			AsOf:         timestamppb.New(t0),
			Terms:        &referencepb.ContractTerms_Structured{Structured: st},
		},
	}
}

// A STORED SECURITIZATION RESOLVES, AND ITS KIND LABEL IS THE STRUCTURED ONE.
//
// The label is asserted because it is the half that fails SILENTLY: LatestAsOf
// does not filter on kind, so a row stored under the wrong one still reads back
// here while ChainAsOf — the query that lists a deal's tranches — returns
// nothing forever (#509's shape, which terms_kind_covers_the_oneof_test.go now
// guards the const block against).
func TestStructuredResolvesAStoredDeal(t *testing.T) {
	rec := structuredRec("MBS-2026-1-A", aDeal())
	if rec.Kind != terms.KindStructured {
		t.Fatalf("kind = %q, want %q", rec.Kind, terms.KindStructured)
	}
	if rec.UnderlyingID != "DEAL-2026-1" {
		t.Errorf("underlying_id = %q, want the deal id — it is what groups a deal's tranches",
			rec.UnderlyingID)
	}
	p, spy := providerWith(t, rec)

	spec, res := p.Structured(context.Background(), "MBS-2026-1-A", t0)
	if res != compute.TermsResolved {
		t.Fatalf("resolution = %v, want TermsResolved — a stored deal that does not resolve is a "+
			"tranche absent from every structured measure", res)
	}
	if spec.Currency != "USD" || spec.OAS != 0.0125 || spec.TrancheIndex != 0 {
		t.Errorf("spec = %+v, want USD / 0.0125 / index 0 after the round trip", spec)
	}
	if len(spec.Deal.Tranches) != 2 {
		t.Errorf("tranches = %d, want 2 — the capital structure did not survive the blob", len(spec.Deal.Tranches))
	}
	if len(spy.seen) != 0 {
		t.Errorf("missing-terms observer fired for a deal that resolved: %v", spy.seen)
	}
}

// AN INSTRUMENT THE STORE HAS NO RECORD FOR IS UNKNOWN, NOT "NOT A
// SECURITIZATION".
//
// This is the answer that keeps #527's confident zero from coming back through
// this family: the engine has certified nothing, so the position is reported as
// unassessed rather than silently omitted from a weighted average.
func TestStructuredAnUnknownInstrumentIsUnknown(t *testing.T) {
	p, spy := providerWith(t)

	_, res := p.Structured(context.Background(), "NOT-LOADED", t0)
	if res != compute.TermsUnknown {
		t.Fatalf("resolution = %v, want TermsUnknown", res)
	}
	if len(spy.seen) != 1 || spy.seen[0] != "NOT-LOADED" {
		t.Errorf("missing-terms observer saw %v, want [NOT-LOADED] — an unloaded instrument has to "+
			"be countable, or a book with no reference data looks like a book with no tranches",
			spy.seen)
	}
}

// A RECORD OF ANOTHER VARIANT IS THE CONFIDENT ABSENCE, and it is the arm that
// only exists because the oneof now has a `structured` case to be distinguished
// FROM. It is what stops the structured measures flagging every equity and
// option on the book.
func TestStructuredAnOptionRecordCertifiesTheAbsence(t *testing.T) {
	p, spy := providerWith(t, optionRec("BTC-60000-C", "BTC-USD", 60000, t0, 1))

	_, res := p.Structured(context.Background(), "BTC-60000-C", t0)
	if res != compute.TermsOtherVariant {
		t.Fatalf("resolution = %v, want TermsOtherVariant — a stored option positively establishes "+
			"that this instrument holds no tranche", res)
	}
	if len(spy.seen) != 0 {
		t.Errorf("missing-terms observer fired for a record that exists: %v", spy.seen)
	}
}

// A DEAL THE STORE CANNOT DESCRIBE IS DECLINED, END TO END (#572/#585).
//
// The record is present, correctly labelled, and carries a BEHAVIORAL prepayment
// assumption whose S-curve parameters reference.v1.PrepaymentAssumption does not
// carry. It is refused — TermsUnusable, which compute.structMeasure records as
// an exclusion — rather than approximated into a spec. THE REFUSAL IS THE POINT:
// it is what makes supporting only the securitized half of the family safe.
func TestStructuredADealTheSchemaCannotDescribeIsRefusedNotPriced(t *testing.T) {
	behavioral := aDeal()
	behavioral.BaseAssumption = &referencepb.PrepaymentAssumption{
		Model: "BEHAVIORAL", Cpr: 0.08, Cdr: 0.01, Severity: 0.35,
	}
	p, spy := providerWith(t, structuredRec("MBS-BEHAVIORAL-A", behavioral))

	spec, res := p.Structured(context.Background(), "MBS-BEHAVIORAL-A", t0)
	if res != compute.TermsUnusable {
		t.Fatalf("resolution = %v, want TermsUnusable — a BEHAVIORAL deal must be declined, not "+
			"approximated with a flat CPR, which would report a mortgage tranche as positively "+
			"convex", res)
	}
	if spec.Prepay != nil || spec.Currency != "" {
		t.Errorf("spec = %+v, want the zero value — a refused record must not leak a partially "+
			"built spec that a caller could price from", spec)
	}
	if len(spy.seen) != 1 {
		t.Errorf("missing-terms observer saw %v, want one entry — an unusable record has the same "+
			"consequence as an unloaded one and must be equally visible", spy.seen)
	}
}
