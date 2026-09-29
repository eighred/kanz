package compliance

import (
	"context"
	"math/big"
	"sort"
	"strconv"
	"sync"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	"google.golang.org/protobuf/proto"
)

// #812 memoised the book fold every COMP-01 rule reads through. This file is
// the correctness half of that change: a faster gate that decides differently
// is not a faster gate, it is a different control.
//
// The existing suite passing is necessary and not sufficient — it pins the
// verdicts anyone thought to write down. What is pinned here instead is
// EQUIVALENCE: over a sweep of awkward books, the memoised fold must produce
// the same holdings, the same gross, and the same violations (message AND
// evidence) as the un-memoised one.

// referenceHeld is the PRE-#812 heldPositions, verbatim: allocate the whole
// book, drop unmarked and zero-valued positions, sort by instrument. It is the
// oracle the memoised fold is checked against, and it deliberately keeps the
// big.Rat zero test that zeroMark replaced — that substitution is one of the
// two things this file exists to prove.
func referenceHeld(b *Book) []Position {
	out := make([]Position, 0, len(b.Positions))
	for _, p := range b.Positions {
		if unmarkedPosition(p) || absRatFromMoney(p.MarketValue).Sign() == 0 {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InstrumentID < out[j].InstrumentID })
	return out
}

// referenceGross is the PRE-#812 totalGross, verbatim.
func referenceGross(b *Book) *big.Rat {
	sum := new(big.Rat)
	for _, pos := range referenceHeld(b) {
		sum.Add(sum, absRatFromMoney(pos.MarketValue))
	}
	return sum
}

// foldSweepBooks are the books the fold has to get right. Each is a shape that
// distinguishes the memoised fold from the one it replaced, or that the zeroMark
// substitution could plausibly have got wrong.
func foldSweepBooks() map[string]*Book {
	book := func(ps ...Position) *Book {
		return &Book{
			PortfolioID: "p1", BaseCurrency: "USD",
			NAV: money(1_000_000, 0, "USD"), NAVBasis: NAVBasisEquity,
			Positions: ps,
		}
	}
	return map[string]*Book{
		"empty":     book(),
		"one":       book(pos("AAPL", 100, 0, "USD")),
		"unsorted":  book(pos("ZZZ", 100, 0, "USD"), pos("AAA", 100, 0, "USD"), pos("MMM", 100, 0, "USD")),
		"duplicate": book(pos("AAPL", 100, 0, "USD"), pos("AAPL", 250, 0, "USD")),
		// A flat position: PRICED, and found to be worth nothing. The fold drops it.
		"flat":     book(pos("AAPL", 100, 0, "USD"), pos("FLAT", 0, 0, "USD")),
		"all_flat": book(pos("A", 0, 0, "USD"), pos("B", 0, 0, "USD")),
		// A zero coefficient at a NON-ZERO exponent is still zero. zeroMark reads
		// the coefficient alone, so this is the shape that would catch it if the
		// exponent mattered.
		"flat_scaled": book(pos("AAPL", 100, 0, "USD"), pos("FLAT", 0, -8, "USD"), pos("FLAT2", 0, 7, "USD")),
		// A NON-zero coefficient at a large negative exponent is a tiny but real
		// holding, and must NOT be dropped.
		"tiny": book(pos("AAPL", 100, 0, "USD"), pos("DUST", 1, -18, "USD")),
		// Shorts: gross is Σ|market value|, so the sign must survive the fold.
		"short": book(pos("AAPL", 100, 0, "USD"), pos("SHORT", -400, 0, "USD")),
		// A short that is exactly cancelled by arithmetic is still flat.
		"short_flat": book(pos("SHORT", -0, 0, "USD"), pos("AAPL", 100, 0, "USD")),
		"extremes": book(pos("MAX", 1<<62, 0, "USD"), pos("MIN", -(1<<62), 0, "USD"),
			pos("SCALED", 123456789, -9, "USD")),
		// The three spellings of an unmarked holding (#760, #806). The fold drops
		// them; every rule that reads the book refuses the whole book first.
		"unmarked_no_value":    book(pos("AAPL", 100, 0, "USD"), Position{InstrumentID: "DARK", Quantity: dec(1, 0)}),
		"unmarked_no_amount":   book(pos("AAPL", 100, 0, "USD"), Position{InstrumentID: "DARK", Quantity: dec(1, 0), MarketValue: &commonpb.Money{CurrencyCode: "USD"}}),
		"unmarked_no_currency": book(pos("AAPL", 100, 0, "USD"), Position{InstrumentID: "DARK", Quantity: dec(1, 0), MarketValue: money(500, 0, "")}),
		"only_unmarked":        book(Position{InstrumentID: "DARK", Quantity: dec(1, 0)}),
		"mixed_currency":       book(pos("AAPL", 100, 0, "USD"), pos("SAP", 200, 0, "EUR")),
	}
}

func TestFold_AgreesWithTheUnmemoisedFoldOnEveryBook(t *testing.T) {
	for name, b := range foldSweepBooks() {
		t.Run(name, func(t *testing.T) {
			c := &Candidate{Book: b, AsOf: t0}
			wantHeld, wantGross := referenceHeld(b), referenceGross(b)

			// Called repeatedly on purpose: every call after the first is served
			// from the memo, and every one of them must still be the right answer.
			for i := 0; i < 3; i++ {
				gotHeld := heldPositions(c)
				if len(gotHeld) != len(wantHeld) {
					t.Fatalf("call %d: held %d positions, un-memoised fold held %d", i, len(gotHeld), len(wantHeld))
				}
				for j := range gotHeld {
					if gotHeld[j].InstrumentID != wantHeld[j].InstrumentID {
						t.Fatalf("call %d: held[%d] = %q, un-memoised fold = %q",
							i, j, gotHeld[j].InstrumentID, wantHeld[j].InstrumentID)
					}
					if !proto.Equal(gotHeld[j].MarketValue, wantHeld[j].MarketValue) {
						t.Fatalf("call %d: held[%d] %s market value = %v, un-memoised fold = %v",
							i, j, gotHeld[j].InstrumentID, gotHeld[j].MarketValue, wantHeld[j].MarketValue)
					}
				}
				if got := totalGross(c); got.Cmp(wantGross) != 0 {
					t.Fatalf("call %d: gross = %s, un-memoised fold = %s", i, got.RatString(), wantGross.RatString())
				}
			}
		})
	}
}

// TestZeroMark_IsExactlyTheRatSignTest sweeps the substitution #812 made inside
// the fold: zeroMark(p) replaced absRatFromMoney(p.MarketValue).Sign() == 0,
// which allocated a big.Rat per position purely to compare it against zero. The
// two must agree on every (coefficient, exponent) pair, or the fold silently
// changes which positions are holdings — and a holding that stops being a
// holding is exactly the #760 fail-open.
func TestZeroMark_IsExactlyTheRatSignTest(t *testing.T) {
	coeffs := []int64{-(1 << 62), -1_000_000, -7, -1, 0, 1, 7, 1_000_000, 1 << 62}
	checked := 0
	for _, coeff := range coeffs {
		for exp := int32(-18); exp <= 18; exp++ {
			p := Position{InstrumentID: "X", MarketValue: money(coeff, exp, "USD")}
			want := absRatFromMoney(p.MarketValue).Sign() == 0
			if got := zeroMark(p); got != want {
				t.Fatalf("zeroMark(coeff=%d exp=%d) = %v, absRatFromMoney(...).Sign()==0 = %v",
					coeff, exp, got, want)
			}
			checked++
		}
	}
	if checked != len(coeffs)*37 {
		t.Fatalf("swept %d pairs, expected %d — the sweep is not running what it claims", checked, len(coeffs)*37)
	}
}

// TestEvaluate_TheMemoChangesNoVerdictAndNoEvidence is the whole-engine half of
// the equivalence proof, and it needs no reference implementation of the rules.
//
// A CANDIDATE THAT EVALUATES A SINGLE RULE CANNOT REUSE THE MEMO, so running
// each rule against its own fresh Candidate reproduces the pre-#812 behaviour
// exactly — every helper folds the book from scratch, as it used to. Running the
// same rules against ONE shared Candidate is the new behaviour. The two must
// produce identical results down to the evidence map: a compliance refusal is
// consumed by an operator and an audit trail, so an evidence key that changed
// value would be a regression even with the verdict intact.
func TestEvaluate_TheMemoChangesNoVerdictAndNoEvidence(t *testing.T) {
	e := NewEngine(nil)
	ctx := context.Background()
	cl := StaticClassifier{
		"AAPL":  {Issuer: "APPLE", Sector: "TECH", AssetClass: "EQUITY"},
		"MSFT":  {Issuer: "MSFT", Sector: "TECH", AssetClass: "EQUITY"},
		"XOM":   {Issuer: "EXXON", Sector: "ENERGY", AssetClass: "EQUITY"},
		"SAP":   {Issuer: "SAP", Sector: "TECH", AssetClass: "EQUITY"},
		"SHORT": {Issuer: "APPLE", Sector: "TECH", AssetClass: "EQUITY"},
		"DUST":  {Issuer: "APPLE", Sector: "TECH", AssetClass: "EQUITY"},
	}

	// Mandates chosen so the sweep exercises PASS, BREACH and both refusals
	// (unmarkedHoldings and unresolvedDimension) rather than only the happy path.
	mandates := map[string]*compliancepb.Mandate{
		"five_rule_classified":  benchMandate(),
		"tight_concentration":   mandate(concRule("tight", compliancepb.Dimension_DIMENSION_INSTRUMENT, dec(10, -2))),
		"sector_concentration":  mandate(concRule("sector", compliancepb.Dimension_DIMENSION_SECTOR, dec(10, -2))),
		"deny_tech":             mandate(restrictRule("deny", compliancepb.Dimension_DIMENSION_SECTOR, compliancepb.RestrictionMode_RESTRICTION_MODE_DENY, "TECH")),
		"allow_only_usd":        mandate(restrictRule("allow", compliancepb.Dimension_DIMENSION_CURRENCY, compliancepb.RestrictionMode_RESTRICTION_MODE_ALLOW_ONLY, "USD")),
		"exclude_apple":         mandate(issuerRule("excl", "APPLE")),
		"leverage_tight":        mandate(leverageCap(1, -1)),
		"currency_usd_only":     mandate(currencyRule("ccy", "USD")),
		"unresolvable_sector":   mandate(concRule("dark", compliancepb.Dimension_DIMENSION_SECTOR, dec(99, -2))),
		"every_rule_classified": mandate(append(benchMandate().GetRules(), issuerRule("excl2", "EXXON"))...),
	}

	classifiers := map[string]Classifier{"present": cl, "absent": nil}

	compared := 0
	for bookName, b := range foldSweepBooks() {
		for clName, classifier := range classifiers {
			for mName, m := range mandates {
				t.Run(bookName+"/"+clName+"/"+mName, func(t *testing.T) {
					// Pre-#812: one fresh fold per rule.
					unmemoised := &compliancepb.ComplianceResult{
						Status: compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS,
					}
					for _, rule := range m.GetRules() {
						one := e.Evaluate(ctx, &Candidate{Book: b, Classifier: classifier, AsOf: t0},
							mandate(rule))
						unmemoised.Violations = append(unmemoised.Violations, one.GetViolations()...)
						unmemoised.Status = worst(unmemoised.GetStatus(), one.GetStatus())
					}

					// Post-#812: one Candidate, one fold, every rule.
					shared := &Candidate{Book: b, Classifier: classifier, AsOf: t0}
					memoised := e.Evaluate(ctx, shared, m)

					if memoised.GetStatus() != unmemoised.GetStatus() {
						t.Fatalf("status %v, un-memoised %v", memoised.GetStatus(), unmemoised.GetStatus())
					}
					if len(memoised.GetViolations()) != len(unmemoised.GetViolations()) {
						t.Fatalf("%d violation(s), un-memoised %d",
							len(memoised.GetViolations()), len(unmemoised.GetViolations()))
					}
					for i, got := range memoised.GetViolations() {
						want := unmemoised.GetViolations()[i]
						if !proto.Equal(got, want) {
							t.Fatalf("violation %d differs\n memoised: %v\n un-memoised: %v", i, got, want)
						}
					}
					compared++
				})
			}
		}
	}
	if want := len(foldSweepBooks()) * len(classifiers) * len(mandates); compared != want {
		t.Fatalf("compared %d (book, classifier, mandate) combinations, expected %d", compared, want)
	}
}

// TestFold_TheBookIsFoldedOncePerEvaluation is #812's own "verified when": the
// five-rule mandate that folded the book NINE times must now fold it once. The
// count is the observable, because a latency fix whose only evidence is a
// benchmark on one machine is not a regression guard — a sixth rule, or a helper
// that reaches past the memo, would put the nine back with every test green.
func TestFold_TheBookIsFoldedOncePerEvaluation(t *testing.T) {
	book, cl := benchBook(25)
	c := &Candidate{Book: book, Classifier: cl, AsOf: t0}
	res := NewEngine(nil).Evaluate(context.Background(), c, benchMandate())
	if res.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Fatalf("fixture mandate should pass: %v", res.GetViolations())
	}
	if c.folds != 1 {
		t.Fatalf("a five-rule mandate folded the book %d times; before #812 it was 9 and it must now be 1", c.folds)
	}
	// Non-vacuity: a fold counter that never increments would also read as 1
	// forever, so prove the counter is live by folding a second, different book.
	other, otherCl := benchBook(7)
	c.Book, c.Classifier = other, otherCl
	if got := len(heldPositions(c)); got != 7 {
		t.Fatalf("after replacing the book, heldPositions returned %d position(s), want 7", got)
	}
	if c.folds != 2 {
		t.Fatalf("folds = %d after a second book, want 2 — the counter is not live", c.folds)
	}
}

// TestFold_AReplacedBookIsFoldedAgain pins the memo key. The memo is keyed on the
// *Book it was computed from and not on a "computed" flag, because a stale fold
// on a compliance gate is a rule evaluated against a book that is NOT the one
// under evaluation — a wrong verdict rather than a slow one.
func TestFold_AReplacedBookIsFoldedAgain(t *testing.T) {
	first := &Book{PortfolioID: "p1", BaseCurrency: "USD", NAV: money(1000, 0, "USD"), NAVBasis: NAVBasisEquity,
		Positions: []Position{pos("AAPL", 100, 0, "USD")}}
	second := &Book{PortfolioID: "p1", BaseCurrency: "USD", NAV: money(1000, 0, "USD"), NAVBasis: NAVBasisEquity,
		Positions: []Position{pos("MSFT", 250, 0, "USD"), pos("XOM", 250, 0, "USD")}}

	c := &Candidate{Book: first, AsOf: t0}
	if got := heldPositions(c); len(got) != 1 || got[0].InstrumentID != "AAPL" {
		t.Fatalf("first book: held %v", ids(got))
	}
	if got := totalGross(c); got.Cmp(big.NewRat(100, 1)) != 0 {
		t.Fatalf("first book gross = %s, want 100", got.RatString())
	}

	c.Book = second
	if got := heldPositions(c); len(got) != 2 || got[0].InstrumentID != "MSFT" || got[1].InstrumentID != "XOM" {
		t.Fatalf("after replacing the book the fold is STALE: held %v, want [MSFT XOM]", ids(got))
	}
	if got := totalGross(c); got.Cmp(big.NewRat(500, 1)) != 0 {
		t.Fatalf("after replacing the book gross = %s, want 500 — the memo is stale", got.RatString())
	}
}

// TestTotalGross_HandsBackACopy: the memoised gross is the denominator of every
// concentration weight in the evaluation. A caller that accumulated into the
// returned Rat would corrupt every rule after it, and this file already records
// what a wrong denominator costs — a violation carrying an `observed` weight
// nobody measured.
func TestTotalGross_HandsBackACopy(t *testing.T) {
	b := &Book{PortfolioID: "p1", BaseCurrency: "USD", NAV: money(1000, 0, "USD"), NAVBasis: NAVBasisEquity,
		Positions: []Position{pos("AAPL", 100, 0, "USD"), pos("MSFT", 300, 0, "USD")}}
	c := &Candidate{Book: b, AsOf: t0}

	first := totalGross(c)
	if first.Cmp(big.NewRat(400, 1)) != 0 {
		t.Fatalf("gross = %s, want 400", first.RatString())
	}
	first.Add(first, big.NewRat(1_000_000, 1)) // a caller mutating what it was handed

	if second := totalGross(c); second.Cmp(big.NewRat(400, 1)) != 0 {
		t.Fatalf("a caller mutating the returned gross corrupted the memo: second read = %s, want 400",
			second.RatString())
	}
}

// TestFold_IsSafeUnderConcurrentReaders exercises the lock the memo is written
// under. Candidate is exported with exported fields, and evaluating one book
// against several mandates concurrently is a natural thing for a caller to
// write. NOTE: without -race this proves the answers agree, not that the access
// is race-free — the race detector needs cgo and does not run on the usual dev
// box (AGENTS.md, Constraints), so CI is what proves the second half.
func TestFold_IsSafeUnderConcurrentReaders(t *testing.T) {
	book, cl := benchBook(64)
	c := &Candidate{Book: book, Classifier: cl, AsOf: t0}

	const readers = 16
	var wg sync.WaitGroup
	got := make([]string, readers)
	grosses := make([]string, readers)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i] = strconv.Itoa(len(heldPositions(c)))
			grosses[i] = totalGross(c).RatString()
		}(i)
	}
	wg.Wait()

	for i := 1; i < readers; i++ {
		if got[i] != got[0] {
			t.Fatalf("reader %d saw %s held positions, reader 0 saw %s", i, got[i], got[0])
		}
		if grosses[i] != grosses[0] {
			t.Fatalf("reader %d saw gross %s, reader 0 saw %s", i, grosses[i], grosses[0])
		}
	}
	if c.folds != 1 {
		t.Fatalf("%d concurrent readers folded the book %d times, want 1", readers, c.folds)
	}
}

func ids(ps []Position) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.InstrumentID)
	}
	return out
}

func concRule(id string, dim compliancepb.Dimension, max *commonpb.Decimal) *compliancepb.Rule {
	return &compliancepb.Rule{
		RuleId: id, Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
		Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
			Dimension: dim, MaxWeight: max,
		}},
	}
}

func restrictRule(id string, dim compliancepb.Dimension, mode compliancepb.RestrictionMode, vals ...string) *compliancepb.Rule {
	return &compliancepb.Rule{
		RuleId: id, Type: compliancepb.RuleType_RULE_TYPE_RESTRICTION,
		Params: &compliancepb.Rule_Restriction{Restriction: &compliancepb.RestrictionList{
			Dimension: dim, Mode: mode, Values: vals,
		}},
	}
}

func issuerRule(id string, issuers ...string) *compliancepb.Rule {
	return &compliancepb.Rule{
		RuleId: id, Type: compliancepb.RuleType_RULE_TYPE_ISSUER_EXCLUSION,
		Params: &compliancepb.Rule_IssuerExclusion{IssuerExclusion: &compliancepb.IssuerExclusion{
			IssuerIds: issuers,
		}},
	}
}

func currencyRule(id string, ccys ...string) *compliancepb.Rule {
	return &compliancepb.Rule{
		RuleId: id, Type: compliancepb.RuleType_RULE_TYPE_CURRENCY,
		Params: &compliancepb.Rule_CurrencyRestriction{CurrencyRestriction: &compliancepb.CurrencyRestriction{
			AllowedCurrencies: ccys,
		}},
	}
}
