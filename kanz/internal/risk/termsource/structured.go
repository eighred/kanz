package termsource

import (
	"context"
	"errors"
	decutil "github.com/eighred/kanz/internal/dec"
	"math"
	"sort"
	"time"

	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"

	"github.com/eighred/kanz/internal/marketdata/terms"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/pricing/structured"
)

// The structured half of the contract-terms provider (#572) — the production
// compute.StructuredProvider that RegisterStructuredRisk had none of, which is
// why StructDuration, StructConvexity and StructWAL were implemented, benchmarked
// and unreachable.
//
// It reads the SAME row the bond and option halves read: one contract_terms
// record per instrument_id, point-in-time by as_of, with the `structured` case
// of the reference.v1.ContractTerms oneof set and terms.KindStructured as its
// label. There is no second store and no second read path, because the fact
// being resolved is the same one — instrument_id to slowly-changing terms, as of
// a time.
//
// # What resolves here, and what deliberately never will
//
// SECURITIZED STRUCTURES: a collateral pool, a capital structure of tranches,
// and a prepayment/default assumption — MBS, ABS, CMO, tranched credit.
//
// STRUCTURED NOTES DO NOT RESOLVE HERE AND ARE NOT MEANT TO. A barrier, a memory
// coupon, an autocall observation schedule or a participation leg describes
// WHETHER OR WHEN a cashflow happens along a path; reference.v1.StructuredTerms
// cannot express one, so such a note has no record, resolves TermsUnknown, and
// is excluded from all three measures with its reason attached. That is the
// intended outcome of the #572 ruling: a payoff program on the valuation path
// was rejected, and refusing a note is the alternative to mispricing it.
//
// # Every refusal below is a refusal rather than a default
//
// #585 made the pricing layer return structured.ErrUnpriceable instead of a
// confident zero, and that only holds if the layer above stops feeding it
// plausible-looking rubbish. So a record that cannot be turned into a spec
// returns compute.TermsUnusable — never a spec with a zero filled in — and
// compute.structMeasure records it as an input exclusion. A DEAL THIS STORE
// CANNOT DESCRIBE IS DECLINED, which is what makes supporting only part of the
// family safe.

// Compile-time assertion for the structured seam, alongside the option and bond
// ones in provider.go. Without it a signature drift in compute.StructuredProvider
// would be discovered at the composition root rather than here.
var _ compute.StructuredProvider = (*Provider)(nil)

// Structured resolves an instrument's securitized-deal spec as of a point in
// time. See compute.TermsResolution for what each answer licenses; only
// TermsResolved carries a usable StructuredSpec.
func (p *Provider) Structured(ctx context.Context, instrumentID string, asOf time.Time) (compute.StructuredSpec, compute.TermsResolution) {
	if p == nil || p.store == nil {
		return compute.StructuredSpec{}, compute.TermsUnknown
	}
	rec, err := p.store.LatestAsOf(ctx, instrumentID, asOf)
	if err != nil {
		if errors.Is(err, terms.ErrNoTerms) && p.onMissing != nil {
			p.onMissing(instrumentID)
		}
		// UNKNOWN COVERS A STORE ERROR TOO, for the reason BondTerms gives: a
		// query that failed tells us nothing about the instrument, so claiming
		// "not a securitization" would be a fabrication. An unreadable database
		// fails the readiness Ping long before it fails here.
		return compute.StructuredSpec{}, compute.TermsUnknown
	}
	st := rec.Terms.GetStructured()
	if st == nil {
		// A record EXISTS and carries an option, a swap, a future or a bond. The
		// one answer that licenses a confident absence, so it must not be
		// reachable by any path that merely failed to find something — which is
		// exactly why the arm above returns Unknown for a store error.
		return compute.StructuredSpec{}, compute.TermsOtherVariant
	}
	spec, ok := toStructuredSpec(st)
	if !ok {
		// Terms that exist and cannot be used. Reported to the missing-terms
		// observer because the consequence is the same as never having loaded
		// them: this tranche is about to be absent from every structured measure.
		if p.onMissing != nil {
			p.onMissing(instrumentID)
		}
		return compute.StructuredSpec{}, compute.TermsUnusable
	}
	return spec, compute.TermsResolved
}

// toStructuredSpec converts reference.v1.StructuredTerms to the float working
// shape the waterfall/OAS engine uses.
//
// THE DECIMAL BOUNDARY IS HERE, as it is in toSpec/toBondSpec: balances are
// common.v1.Decimal on the wire because money is exact, and the projection is
// numerical, so the conversion happens once, at the edge.
//
// A TERM THAT CANNOT BE REPRESENTED IS REFUSED RATHER THAN DEFAULTED, and for
// this family the defaults are unusually flattering. A pool with no balance
// projects an empty waterfall, so every tranche receives no principal and WAL
// refuses; a pool with no gross coupon collects no interest at all; a prepayment
// model nobody named would have to become a guess about how fast a mortgage book
// pays down, which is most of the answer.
func toStructuredSpec(st *referencepb.StructuredTerms) (compute.StructuredSpec, bool) {
	pool, ok := toPool(st.GetPool())
	if !ok {
		return compute.StructuredSpec{}, false
	}
	tranches, ok := toTranches(st.GetTranches())
	if !ok {
		return compute.StructuredSpec{}, false
	}
	idx, ok := heldTrancheIndex(tranches, st.GetHeldTranche())
	if !ok {
		return compute.StructuredSpec{}, false
	}
	if st.GetCurrencyCode() == "" {
		// The currency is the KEY the discount curve is looked up by. An empty
		// one resolves no curve, the tranche has no present value at all, and the
		// position drops out of all three measures — refused here, where the
		// observer fires and the reason names the record.
		return compute.StructuredSpec{}, false
	}
	prepay, ok := toPrepay(st.GetBaseAssumption())
	if !ok {
		return compute.StructuredSpec{}, false
	}
	// PRESENCE, NOT VALUE. quoted_oas is `optional double` precisely so that a
	// tranche marked flat to the curve (0) and a record where nobody supplied a
	// mark are different states. Reading the proto3 default here would carry
	// every subordinate tranche on the estate at zero spread — the flattering
	// direction, and indistinguishable from a real mark.
	if st.QuotedOas == nil {
		return compute.StructuredSpec{}, false
	}
	oas := st.GetQuotedOas()
	if math.IsNaN(oas) || math.IsInf(oas, 0) || math.Abs(oas) >= 1 {
		// A spread at or beyond 100% is not a mark; the bisection in
		// structured.OAS searches ±0.5, so a value outside that band could not
		// have come from this platform's own solver either.
		return compute.StructuredSpec{}, false
	}
	return compute.StructuredSpec{
		Deal:         structured.Deal{Pool: pool, Tranches: tranches},
		Prepay:       prepay,
		Currency:     st.GetCurrencyCode(),
		TrancheIndex: idx,
		OAS:          oas,
	}, true
}

// toPool converts the collateral pool.
func toPool(p *referencepb.CollateralPool) (structured.Pool, bool) {
	if p == nil {
		return structured.Pool{}, false
	}
	balance, ok := decutil.Float64(p.GetOriginalBalance())
	if !ok || balance <= 0 {
		return structured.Pool{}, false
	}
	if p.GetTermMonths() == 0 {
		// Deal.Project loops `m := 1; m <= TermMonths`, so a zero term projects
		// nothing: no principal reaches any tranche and every measure refuses.
		// Refusing here says WHICH record is wrong.
		return structured.Pool{}, false
	}
	gross := p.GetGrossCoupon()
	if gross <= 0 || math.IsNaN(gross) || gross >= 1 {
		// UNSET READS AS ZERO and proto3 cannot tell that from a deliberate
		// zero-coupon pool, so this refuses both. A securitization whose
		// collateral pays no interest is overwhelmingly an unloaded field rather
		// than a real deal, and the consequence of guessing wrong is a waterfall
		// with no interest to distribute — every tranche coupon unpaid, and a
		// duration measured on a stream that does not exist.
		return structured.Pool{}, false
	}
	fee := p.GetServicingFee()
	if fee < 0 || fee > gross || math.IsNaN(fee) {
		// netCoupon is gross − fee and is what the waterfall distributes. A fee
		// above the gross coupon makes it negative, which the projection would
		// carry as negative interest rather than reject.
		return structured.Pool{}, false
	}
	freq := p.GetPaymentFrequency()
	if freq != referencepb.PaymentFrequency_PAYMENT_FREQUENCY_MONTHLY &&
		freq != referencepb.PaymentFrequency_PAYMENT_FREQUENCY_UNSPECIFIED {
		// THE ENGINE IS MONTHLY AND SAYS SO — Deal.Project steps one month at a
		// time and PrepayModel is quoted in single-monthly-mortality. A quarterly
		// pool would be projected as if it paid twelve times a year, which shortens
		// its WAL and understates its duration, with nothing to indicate it.
		// UNSPECIFIED is accepted because the field is optional for MBS, whose
		// convention IS monthly; a stated non-monthly frequency is refused.
		return structured.Pool{}, false
	}
	return structured.Pool{
		Balance:      balance,
		GrossCoupon:  gross,
		ServicingFee: fee,
		TermMonths:   int(p.GetTermMonths()),
	}, true
}

// toTranches converts the capital structure, ORDERED BY SENIORITY.
//
// THE ORDER IS THE WATERFALL, not a presentation detail: Deal.Project pays
// interest and principal senior→junior by slice position and allocates losses in
// reverse. structured.Tranche carries no seniority field of its own, so the
// ordering here IS the attachment/detachment information — a capital structure
// read in the wrong order pays the equity piece first, which is the difference
// between a AAA note and a first-loss one.
//
// StructuredTerms documents its `tranches` as already senior→junior AND carries
// an explicit `seniority`. Sorting on the explicit field with a STABLE sort takes
// the stronger statement where both exist and preserves the declared order where
// seniority was never set (all zero).
func toTranches(in []*referencepb.Tranche) ([]structured.Tranche, bool) {
	if len(in) == 0 {
		return nil, false
	}
	ordered := make([]*referencepb.Tranche, len(in))
	copy(ordered, in)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].GetSeniority() < ordered[j].GetSeniority()
	})
	out := make([]structured.Tranche, 0, len(ordered))
	for _, t := range ordered {
		if t == nil || t.GetName() == "" {
			// The name is what held_tranche resolves against. An unnamed tranche
			// cannot be the held one and makes every other index ambiguous.
			return nil, false
		}
		balance, ok := decutil.Float64(t.GetOriginalBalance())
		if !ok || balance <= 0 {
			return nil, false
		}
		// A ZERO COUPON IS VALID HERE and is not a missing one — a principal-only
		// tranche is a real structure. Negative is refused: it is not a note this
		// engine models, and the waterfall would pay negative interest.
		coupon := t.GetCoupon()
		if coupon < 0 || math.IsNaN(coupon) || coupon >= 1 {
			return nil, false
		}
		out = append(out, structured.Tranche{Name: t.GetName(), Balance: balance, Coupon: coupon})
	}
	return out, true
}

// heldTrancheIndex locates the tranche this instrument_id IS.
//
// AMBIGUITY IS REFUSED, NOT RESOLVED BY FIRST MATCH. Two tranches sharing a name
// means the record cannot say which slice of the capital structure is held, and
// picking one would report a senior note's duration for an equity piece — a
// number roughly an order of magnitude wrong, in the direction that passes a
// limit.
func heldTrancheIndex(tranches []structured.Tranche, held string) (int, bool) {
	if held == "" {
		return 0, false
	}
	idx, seen := -1, 0
	for i, t := range tranches {
		if t.Name == held {
			idx, seen = i, seen+1
		}
	}
	if seen != 1 {
		return 0, false
	}
	return idx, true
}

// toPrepay converts the prepayment/default assumption to a structured.PrepayModel.
//
// # BEHAVIORAL IS REFUSED, AND THAT IS THE HONEST ANSWER RATHER THAN A GAP
//
// structured.Behavioral needs Base, Max and Steepness — the S-curve that makes
// prepayment respond to the refinancing incentive, and the source of the negative
// convexity this family exists to measure. reference.v1.PrepaymentAssumption
// carries none of the three. Substituting a house S-curve would put a model
// nobody stated behind a number an operator reads as measured, and defaulting to
// a flat CPR would silently remove the rate-responsiveness that a BEHAVIORAL
// record explicitly asked for — reporting a mortgage tranche as positively
// convex, which is the wrong sign, not merely the wrong size.
//
// So a BEHAVIORAL deal is declined with coverage until the schema carries the
// parameters. Adding those three doubles is additive and squarely inside the
// securitized family; it is left out here because nothing on the estate holds
// such a deal yet, and #572's ruling is explicit that support is taken on
// evidence that we hold the instrument.
func toPrepay(a *referencepb.PrepaymentAssumption) (structured.PrepayModel, bool) {
	if a == nil {
		return nil, false
	}
	cdr, sev := a.GetCdr(), a.GetSeverity()
	if !isRate(cdr) || sev < 0 || sev > 1 || math.IsNaN(sev) {
		return nil, false
	}
	switch a.GetModel() {
	case "CONSTANT":
		cpr := a.GetCpr()
		if !isRate(cpr) {
			return nil, false
		}
		return structured.ConstantCPR{CPR: cpr, CDR: cdr, Sev: sev}, true
	case "PSA":
		mult := a.GetPsaMultiple()
		if mult <= 0 || math.IsNaN(mult) || math.IsInf(mult, 0) {
			// 1.0 is 100 PSA. Zero is not "no prepayment" here, it is an unset
			// field, and it would project the deal at a speed nobody chose.
			return nil, false
		}
		return structured.PSA{Multiple: mult, CDR: cdr, Sev: sev}, true
	default:
		// "BEHAVIORAL" (see above), "" (unset), and anything else. An unrecognized
		// model name is refused rather than falling back to a flat speed: the
		// prepayment assumption is most of a mortgage tranche's duration, and a
		// silent substitution would be a different deal reported under this one's
		// name.
		return nil, false
	}
}

// isRate reports whether r is a usable annualized rate fraction in [0, 1).
// 1 means every loan prepays or defaults in the first month, which projects a
// deal with no cashflows rather than a fast one.
func isRate(r float64) bool { return r >= 0 && r < 1 && !math.IsNaN(r) }
