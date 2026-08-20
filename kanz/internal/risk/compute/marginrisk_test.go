package compute

import (
	"context"
	"math/big"
	"testing"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
)

// A LIQUIDATION PROXIMITY COMPUTED OVER MARGIN NOBODY COULD READ MUST NOT BE A
// NUMBER (#408 control 4, #527).
//
// The measure has three answers and only two of them are numbers, and the two
// that look alike are the pair the #408 ruling calls out: an account the
// EXCHANGE says holds no leveraged position (an exact, true zero) and an account
// the platform could not read (no value at all). Every test below that matters
// is one of those two, or the invariant that keeps them apart on the wire.

// --- fixtures -------------------------------------------------------------

type fakeMarginProvider struct {
	accounts []AccountMargin
	ok       bool
	calls    int
}

func (f *fakeMarginProvider) AccountMargins(_ context.Context, _ v1.PortfolioID) ([]AccountMargin, bool) {
	f.calls++
	return f.accounts, f.ok
}

func marginBook() *domain.Portfolio { return domain.NewPortfolio("PORT-MARGIN", "USD") }

// completeAccount is a venue account the exchange answered fully: read, current,
// coverage reported, nothing excluded.
func completeAccount(positions ...LiquidationRef) AccountMargin {
	return AccountMargin{
		Venue: "OKX", Account: "ACCT-1",
		Read: true, CoverageReported: true, ExcludedCount: 0,
		Positions: positions,
	}
}

func levered(id domain.InstrumentID, liquidation, mark *big.Rat) LiquidationRef {
	return LiquidationRef{
		InstrumentID: id, VenueSymbol: string(id) + "-SWAP",
		Liquidation: liquidation, Mark: mark,
	}
}

func rat(n, d int64) *big.Rat { return big.NewRat(n, d) }

// measureFrom registers the seam against provider and evaluates it once,
// returning the measure and every (instrument, reason) the observer saw.
func measureFrom(t *testing.T, provider MarginProvider) (v1.Measure, []string) {
	t.Helper()
	var reasons []string
	r := NewRegistry()
	RegisterMarginRisk(context.Background(), r, provider,
		WithMarginObserver(func(id, reason string) { reasons = append(reasons, id+"/"+reason) }))
	fn, ok := r.funcs[MeasureLiquidationProximity]
	if !ok {
		t.Fatalf("RegisterMarginRisk registered no %s — nothing to evaluate", MeasureLiquidationProximity)
	}
	return fn(marginBook()), reasons
}

// ratValue renders the measure's Decimal as an exact rational so the assertions
// compare NUMBERS rather than a coefficient/exponent pair that a scale change
// would break.
func ratValue(t *testing.T, m v1.Measure) *big.Rat {
	t.Helper()
	if m.Value == nil {
		t.Fatalf("measure has no Value")
	}
	num := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-m.Value.GetExponent())), nil)
	return new(big.Rat).SetFrac(big.NewInt(m.Value.GetCoefficient()), num)
}

func hasReason(reasons []v1.InputExclusion, want string) bool {
	for _, ex := range reasons {
		if ex.Reason == want {
			return true
		}
	}
	return false
}

// --- the arithmetic -------------------------------------------------------

func TestLiquidationProximity_IsTheFractionOfTheMoveAlreadyTravelled(t *testing.T) {
	// Mark 100, the venue liquidates at 80: a 20% adverse move away, so the book
	// is 80% of the way there.
	m, _ := measureFrom(t, &fakeMarginProvider{ok: true, accounts: []AccountMargin{
		completeAccount(levered("BTC", rat(80, 1), rat(100, 1))),
	}})
	if got, want := ratValue(t, m), rat(8, 10); got.Cmp(want) != 0 {
		t.Errorf("proximity = %s, want %s (mark 100, liquidation 80 ⇒ 1 − 0.20)", got, want)
	}
	if m.Coverage.ExcludedCount != 0 {
		t.Errorf("ExcludedCount = %d, want 0 — the exchange answered everything", m.Coverage.ExcludedCount)
	}
	if m.Coverage.Contributed != 1 {
		t.Errorf("Contributed = %d, want 1 (one venue account reached the arithmetic)", m.Coverage.Contributed)
	}
}

func TestLiquidationProximity_AtTheLiquidationPriceIsOne(t *testing.T) {
	m, _ := measureFrom(t, &fakeMarginProvider{ok: true, accounts: []AccountMargin{
		completeAccount(levered("BTC", rat(100, 1), rat(100, 1))),
	}})
	if got := ratValue(t, m); got.Cmp(rat(1, 1)) != 0 {
		t.Errorf("proximity = %s, want 1 — the venue is liquidating at the current price", got)
	}
}

func TestLiquidationProximity_TakesTheWorstPositionAcrossEveryAccount(t *testing.T) {
	// A comfortable account and a dangerous one. An average would report 0.45 and
	// hide the position that is about to be sold.
	safe := completeAccount(levered("ETH", rat(10, 1), rat(100, 1))) // 0.10
	danger := AccountMargin{
		Venue: "OKX", Account: "ACCT-2", Read: true, CoverageReported: true,
		Positions: []LiquidationRef{levered("BTC", rat(92, 1), rat(100, 1))}, // 0.92
	}
	m, _ := measureFrom(t, &fakeMarginProvider{ok: true, accounts: []AccountMargin{safe, danger}})
	if got, want := ratValue(t, m), rat(92, 100); got.Cmp(want) != 0 {
		t.Errorf("proximity = %s, want %s — the MAXIMUM, not an average (an average would be 0.51)", got, want)
	}
	if m.Coverage.Contributed != 2 {
		t.Errorf("Contributed = %d, want 2 (two accounts answered)", m.Coverage.Contributed)
	}
}

func TestLiquidationProximity_ClampsAtZeroRatherThanReportingNegative(t *testing.T) {
	// A short whose liquidation boundary is 2.5× the mark: the move required is
	// larger than the price itself.
	m, _ := measureFrom(t, &fakeMarginProvider{ok: true, accounts: []AccountMargin{
		completeAccount(levered("BTC", rat(250, 1), rat(100, 1))),
	}})
	got := ratValue(t, m)
	if got.Sign() != 0 {
		t.Errorf("proximity = %s, want exact 0 — a negative proximity would be compared against a limit", got)
	}
}

// --- the pair the ruling calls out ---------------------------------------

func TestLiquidationProximity_AnExchangeReportedUnleveredAccountIsAnExactZero(t *testing.T) {
	// Read, current, coverage reported, nothing excluded, and NO open position:
	// the exchange itself saying this account holds no leveraged position.
	m, reasons := measureFrom(t, &fakeMarginProvider{ok: true, accounts: []AccountMargin{completeAccount()}})
	if m.Value == nil {
		t.Fatalf("an account the EXCHANGE says is unlevered must produce a value, not a refusal")
	}
	if got := ratValue(t, m); got.Sign() != 0 {
		t.Errorf("proximity = %s, want exact 0 — nothing to be near", got)
	}
	if m.Coverage.ExcludedCount != 0 {
		t.Errorf("ExcludedCount = %d, want 0", m.Coverage.ExcludedCount)
	}
	if m.Coverage.Contributed == 0 {
		t.Fatalf("Contributed = 0 on a complete observation — publish.toProtoInputCoverage renders " +
			"a coverage with no contributions and no exclusions as ABSENT, which tells every " +
			"consumer this measure does not report coverage, on the one answer whose credibility " +
			"rests on it")
	}
	if len(reasons) != 0 {
		t.Errorf("observer fired %v on a complete, unlevered account — nothing was skipped", reasons)
	}
}

func TestLiquidationProximity_ASilentVenueIsNotAnUnleveredOne(t *testing.T) {
	// Binance's adapter is spot-only and publishes nothing on
	// accounting.margin.observed (#601), so its account is UNKNOWN — never
	// observed. That must NOT come out the same as the exchange-reported
	// unlevered account above, or a blind book and an unlevered one are
	// indistinguishable.
	silent := AccountMargin{Venue: "BINANCE", Account: "ACCT-B", Read: false}
	m, reasons := measureFrom(t, &fakeMarginProvider{ok: true, accounts: []AccountMargin{silent}})

	if m.Value != nil {
		t.Fatalf("a venue that has never reported margin produced a value (%v) — silence was read as safety",
			ratValue(t, m))
	}
	if m.Coverage.ExcludedCount == 0 {
		t.Fatalf("ExcludedCount = 0 on an unreadable account: riskview folds any measure whose " +
			"coverage excludes nothing, so this would reach RiskLimitRule as a limit-passing zero")
	}
	if !hasReason(m.Coverage.Exclusions, SkipMarginUnknown) {
		t.Errorf("exclusions %v carry no %q — an operator cannot tell which of the unknowns to fix",
			m.Coverage.Exclusions, SkipMarginUnknown)
	}
	if len(reasons) == 0 {
		t.Errorf("observer never fired — the counter is a metric, not a substitute, but it must still fire")
	}

	// And the two answers must differ in the way the ruling requires.
	unlevered, _ := measureFrom(t, &fakeMarginProvider{ok: true, accounts: []AccountMargin{completeAccount()}})
	if (unlevered.Value == nil) == (m.Value == nil) {
		t.Fatalf("an unlevered book and a blind one produced the same shape of answer")
	}
}

// --- every way to be unreadable ------------------------------------------

func TestLiquidationProximity_RefusesOnEveryUnreadableShapeAndSaysWhich(t *testing.T) {
	priced := levered("BTC", rat(80, 1), rat(100, 1))
	cases := []struct {
		name     string
		provider *fakeMarginProvider
		reason   string
	}{
		{
			name:     "the platform cannot say which accounts back the portfolio",
			provider: &fakeMarginProvider{ok: false},
			reason:   SkipNoMarginAccounts,
		},
		{
			name:     "an empty account list is not an unlevered book",
			provider: &fakeMarginProvider{ok: true, accounts: []AccountMargin{}},
			reason:   SkipNoMarginAccounts,
		},
		{
			name: "never observed, or no longer current",
			provider: &fakeMarginProvider{ok: true, accounts: []AccountMargin{
				{Venue: "OKX", Account: "A", Read: false},
			}},
			reason: SkipMarginUnknown,
		},
		{
			name: "the observation reports no coverage at all",
			provider: &fakeMarginProvider{ok: true, accounts: []AccountMargin{
				{Venue: "OKX", Account: "A", Read: true, CoverageReported: false, Positions: []LiquidationRef{priced}},
			}},
			reason: SkipMarginUncovered,
		},
		{
			name: "the exchange did not answer part of it",
			provider: &fakeMarginProvider{ok: true, accounts: []AccountMargin{
				{Venue: "OKX", Account: "A", Read: true, CoverageReported: true, ExcludedCount: 1,
					Positions: []LiquidationRef{priced}},
			}},
			reason: SkipMarginIncomplete,
		},
		{
			name: "an open position with no liquidation price",
			provider: &fakeMarginProvider{ok: true, accounts: []AccountMargin{
				completeAccount(levered("BTC", nil, rat(100, 1))),
			}},
			reason: SkipNoLiquidationPrice,
		},
		{
			name: "an open position with no reference price to measure against",
			provider: &fakeMarginProvider{ok: true, accounts: []AccountMargin{
				completeAccount(levered("BTC", rat(80, 1), nil)),
			}},
			reason: SkipNoMark,
		},
		{
			name: "a reference price of zero has no denominator",
			provider: &fakeMarginProvider{ok: true, accounts: []AccountMargin{
				completeAccount(levered("BTC", rat(80, 1), new(big.Rat))),
			}},
			reason: SkipMarkNotPositive,
		},
		{
			name: "a negative liquidation price is refused, not clamped",
			provider: &fakeMarginProvider{ok: true, accounts: []AccountMargin{
				completeAccount(levered("BTC", rat(-5, 1), rat(100, 1))),
			}},
			reason: SkipLiquidationNegative,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, reasons := measureFrom(t, tc.provider)
			if m.Value != nil {
				t.Fatalf("%s produced a value (%s) — a proximity the platform could not read is not a number",
					tc.name, ratValue(t, m))
			}
			if m.Coverage.ExcludedCount == 0 {
				t.Fatalf("%s produced no exclusion: an absent Value with clean coverage is folded by "+
					"riskview and dec.FromProto(nil) is ZERO, so it would reach the gate as a "+
					"limit-passing confident zero", tc.name)
			}
			if !hasReason(m.Coverage.Exclusions, tc.reason) {
				t.Errorf("exclusions %v carry no %q", m.Coverage.Exclusions, tc.reason)
			}
			if len(reasons) == 0 {
				t.Errorf("observer never fired for %s", tc.name)
			}
		})
	}
}

func TestLiquidationProximity_ALeveredBookNothingCouldPriceIsNotZero(t *testing.T) {
	// THE CONFIDENT ZERO'S LAST DOOR. The account is read, current and complete —
	// every account-level check passes — and the exchange has listed two open
	// leveraged positions. Neither can be placed against a reference price. If
	// the measure fell through to the unlevered zero here it would report "no
	// leveraged position" for a book the exchange has just said is full of them.
	m, _ := measureFrom(t, &fakeMarginProvider{ok: true, accounts: []AccountMargin{
		completeAccount(
			levered("BTC", rat(80, 1), nil),
			levered("ETH", rat(40, 1), nil),
		),
	}})
	if m.Value != nil {
		t.Fatalf("proximity = %s on a levered book where nothing could be priced — the exchange "+
			"reported open positions and this answer says there are none", ratValue(t, m))
	}
	if m.Coverage.ExcludedCount != 2 {
		t.Errorf("ExcludedCount = %d, want 2 — one per position the measure could not place",
			m.Coverage.ExcludedCount)
	}
	if m.Coverage.Contributed != 1 {
		t.Errorf("Contributed = %d, want 1 — the ACCOUNT was answered completely; it is the "+
			"positions inside it that could not be placed", m.Coverage.Contributed)
	}
}

func TestLiquidationProximity_APartiallyPricedBookStillAnswersAndSaysWhatItMissed(t *testing.T) {
	m, _ := measureFrom(t, &fakeMarginProvider{ok: true, accounts: []AccountMargin{
		completeAccount(
			levered("BTC", rat(70, 1), rat(100, 1)), // 0.70
			levered("ETH", rat(40, 1), nil),         // unpriceable
		),
	}})
	if got, want := ratValue(t, m), rat(7, 10); got.Cmp(want) != 0 {
		t.Errorf("proximity = %s, want %s", got, want)
	}
	if m.Coverage.ExcludedCount != 1 {
		t.Errorf("ExcludedCount = %d, want 1 — the answer is partial and must say so", m.Coverage.ExcludedCount)
	}
	if len(m.Coverage.Exclusions) == 0 || m.Coverage.Exclusions[0].InstrumentID != "ETH" {
		t.Errorf("exclusions %v do not name the position that was left out", m.Coverage.Exclusions)
	}
}

// --- the invariant that keeps the wire honest -----------------------------

func TestLiquidationProximity_NoValueAlwaysCarriesAnExclusion(t *testing.T) {
	// riskview folds any measure whose coverage excludes nothing, and
	// dec.FromProto(nil) is zero. So a measure that withholds its value and
	// reports clean coverage arrives at RiskLimitRule as a confident zero by the
	// one route the coverage discipline does not cover. This sweeps every shape
	// the seam can produce and pins the implication in both directions.
	shapes := []*fakeMarginProvider{
		{ok: false},
		{ok: true, accounts: nil},
		{ok: true, accounts: []AccountMargin{{Read: false}}},
		{ok: true, accounts: []AccountMargin{{Read: true}}},
		{ok: true, accounts: []AccountMargin{{Read: true, CoverageReported: true, ExcludedCount: 3}}},
		{ok: true, accounts: []AccountMargin{completeAccount()}},
		{ok: true, accounts: []AccountMargin{completeAccount(levered("BTC", nil, nil))}},
		{ok: true, accounts: []AccountMargin{completeAccount(levered("BTC", rat(80, 1), rat(100, 1)))}},
		{ok: true, accounts: []AccountMargin{
			completeAccount(levered("BTC", rat(80, 1), rat(100, 1)), levered("ETH", nil, nil)),
		}},
	}
	for i, provider := range shapes {
		m, _ := measureFrom(t, provider)
		if m.Value == nil && m.Coverage.ExcludedCount == 0 {
			t.Errorf("shape %d withheld its value with clean coverage — riskview would fold it and "+
				"the gate would read it as zero", i)
		}
		if m.Value != nil && m.Coverage.Contributed == 0 {
			t.Errorf("shape %d produced a value with no contributions — the wire mapping renders "+
				"that coverage as ABSENT, which claims the measure does not report coverage", i)
		}
	}
}

// --- the seam -------------------------------------------------------------

func TestRegisterMarginRisk_NilProviderRegistersNothingAndSaysSo(t *testing.T) {
	var reasons []string
	r := NewRegistry()
	RegisterMarginRisk(context.Background(), r, nil,
		WithMarginObserver(func(_, reason string) { reasons = append(reasons, reason) }))

	if _, ok := r.funcs[MeasureLiquidationProximity]; ok {
		t.Errorf("a nil provider registered %s — a measure that always refuses reads as LIVE to "+
			"compute.Dark, and the margin family's posture gauge stops saying nothing is wired",
			MeasureLiquidationProximity)
	}
	if len(reasons) != 1 || reasons[0] != SkipNoMarginProvider {
		t.Errorf("observer saw %v, want exactly [%s] — an ABSENT measure is otherwise silent, "+
			"because engine.filterMeasures drops unknown names", reasons, SkipNoMarginProvider)
	}
}

func TestLiquidationProximity_DoesNotMutateTheProvidersRationals(t *testing.T) {
	// big.Rat's operations write into their receiver, and these pointers are the
	// venue's own figures held by the view every other caller reads. A Sub or an
	// Abs applied in place here would rewrite the exchange's liquidation price
	// for everyone.
	liq, mark := rat(80, 1), rat(100, 1)
	wantLiq, wantMark := rat(80, 1), rat(100, 1)
	measureFrom(t, &fakeMarginProvider{ok: true, accounts: []AccountMargin{
		completeAccount(levered("BTC", liq, mark)),
	}})
	if liq.Cmp(wantLiq) != 0 {
		t.Errorf("the venue's liquidation price was rewritten in place: %s, want %s", liq, wantLiq)
	}
	if mark.Cmp(wantMark) != 0 {
		t.Errorf("the reference price was rewritten in place: %s, want %s", mark, wantMark)
	}
}

func TestLiquidationProximityIsCataloguedUnderTheMarginFamily(t *testing.T) {
	for _, cm := range Catalogue() {
		if cm.Name == MeasureLiquidationProximity {
			if cm.Family != FamilyMargin {
				t.Errorf("family = %q, want %q", cm.Family, FamilyMargin)
			}
			return
		}
	}
	t.Fatalf("%s is absent from the catalogue — MeasurePosture subtracts the live registry from "+
		"it, so a measure it does not name cannot be counted as dark", MeasureLiquidationProximity)
}
