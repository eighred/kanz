package frtb

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// --- curvature ---------------------------------------------------------------

// One bucket, both directions hand-computed: two factors with CVR⁺ {4, 3}
// (both losses) and CVR⁻ {−1, −2} (both gains). Up: √(16+9+2ρ²·12); down is
// all-negative so diagonal drops and ψ zeroes the cross term ⇒ 0.
func TestCurvatureCharge_HandComputed(t *testing.T) {
	params := Params{"Equity": {IntraCorr: 0.5, InterCorr: 0.5}}
	sens := []CurvatureSensitivity{
		{RiskClass: "Equity", Bucket: "1", Factor: "A", Up: 4, Down: -1},
		{RiskClass: "Equity", Bucket: "1", Factor: "B", Up: 3, Down: -2},
	}
	// Medium scenario: ρ² = 0.25 ⇒ up = √(25 + 2·0.25·12) = √31.
	medium := curvatureScenario(sens, params["Equity"], Medium)
	if want := math.Sqrt(31); math.Abs(medium-want) > 1e-12 {
		t.Errorf("medium scenario: got %v want %v", medium, want)
	}
	// The worst-of-three can only be ≥ medium.
	if got := mustCurvature(t, sens, params); got < medium {
		t.Errorf("worst scenario %v must be ≥ medium %v", got, medium)
	}
}

// Two all-gain factors must not create capital, and must not offset a lossy
// bucket beyond ψ's guard.
func TestCurvatureCharge_GainsNeverCreateCapital(t *testing.T) {
	params := Params{"Equity": {IntraCorr: 0.5, InterCorr: 0.5}}
	gains := []CurvatureSensitivity{
		{RiskClass: "Equity", Bucket: "1", Factor: "A", Up: -5, Down: -5},
		{RiskClass: "Equity", Bucket: "1", Factor: "B", Up: -3, Down: -3},
	}
	if got := mustCurvature(t, gains, params); got != 0 {
		t.Errorf("all-gain book must carry zero curvature charge, got %v", got)
	}
}

// --- DRC / RRAO ----------------------------------------------------------------

// The MAR22 hedge-benefit example: long 100 BBB (RW 6%), short 50 BB (RW 15%)
// in one bucket. HBR = 100/150; charge = 6 − (2/3)·7.5 = 1.
func TestDRC_HedgeBenefitRatio(t *testing.T) {
	got, err := DRC([]JTDPosition{
		{Obligor: "X", Bucket: "corporates", Rating: "BBB", Amount: 100},
		{Obligor: "Y", Bucket: "corporates", Rating: "BB", Amount: -50},
	}, DefaultDRCParams())
	if err != nil {
		t.Fatalf("DRC: %v", err)
	}
	if math.Abs(got-1.0) > 1e-9 {
		t.Errorf("DRC: got %v want 1.0", got)
	}
}

func TestDRC_NettingAndBuckets(t *testing.T) {
	p := DefaultDRCParams()
	// Same obligor long 100 / short 40 nets to long 60 before weighting.
	netted, err := DRC([]JTDPosition{
		{Obligor: "X", Bucket: "corporates", Rating: "BBB", Amount: 100},
		{Obligor: "X", Bucket: "corporates", Rating: "BBB", Amount: -40},
	}, p)
	if err != nil {
		t.Fatalf("netting: %v", err)
	}
	if want := 0.06 * 60; math.Abs(netted-want) > 1e-9 {
		t.Errorf("obligor netting: got %v want %v", netted, want)
	}
	// A short in ANOTHER bucket must not hedge: no cross-bucket netting.
	cross, err := DRC([]JTDPosition{
		{Obligor: "X", Bucket: "corporates", Rating: "BBB", Amount: 100},
		{Obligor: "S", Bucket: "sovereigns", Rating: "BBB", Amount: -100},
	}, p)
	if err != nil {
		t.Fatalf("cross-bucket: %v", err)
	}
	if want := 0.06 * 100; math.Abs(cross-want) > 1e-9 {
		t.Errorf("cross-bucket: got %v want %v (short must not offset)", cross, want)
	}
	// Unrated falls back to the unrated weight.
	unrated, err := DRC([]JTDPosition{{Obligor: "U", Bucket: "corporates", Rating: "NR?", Amount: 100}}, p)
	if err != nil {
		t.Fatalf("unrated fallback must still apply when UnratedWeight is non-zero: %v", err)
	}
	if want := p.UnratedWeight * 100; math.Abs(unrated-want) > 1e-9 {
		t.Errorf("unrated: got %v want %v", unrated, want)
	}
}

func TestRRAO_WeightedNotionals(t *testing.T) {
	got := RRAO([]RRAOPosition{
		{Notional: 1_000_000, Exotic: true},   // 1.0% = 10,000
		{Notional: -2_000_000, Exotic: false}, // 0.1% of |notional| = 2,000
	})
	if math.Abs(got-12_000) > 1e-9 {
		t.Errorf("RRAO: got %v want 12000", got)
	}
}

// --- CRIF ---------------------------------------------------------------------

const sampleCRIF = "RiskType\tQualifier\tBucket\tLabel1\tLabel2\tAmount\n" +
	"Risk_IRCurve\tUSD\t1\t5y\tOIS\t1500.5\n" +
	"Risk_Equity\tAAPL\t3\t\t\t-2000\n" +
	"Risk_EquityVol\tAAPL\t3\t1y\t\t300\n" +
	"Risk_XCcyBasis\tUSD\t\t\t\t99\n" // margin-only row: parsed, not mapped

func TestParseCRIF_AndMappers(t *testing.T) {
	recs, err := ParseCRIF(strings.NewReader(sampleCRIF))
	if err != nil {
		t.Fatalf("ParseCRIF: %v", err)
	}
	if len(recs) != 4 {
		t.Fatalf("records: got %d want 4", len(recs))
	}

	delta := DeltaSensitivities(recs)
	if len(delta) != 2 {
		t.Fatalf("delta rows: got %d want 2 (vol + basis rows excluded)", len(delta))
	}
	if delta[0].RiskClass != "GIRR" || delta[0].Factor != "USD/5y/OIS" || delta[0].Amount != 1500.5 {
		t.Errorf("GIRR row mapped wrong: %+v", delta[0])
	}
	if delta[1].RiskClass != "Equity" || delta[1].Bucket != "3" {
		t.Errorf("equity row mapped wrong: %+v", delta[1])
	}

	vega := VegaSensitivities(recs)
	if len(vega) != 1 || vega[0].RiskClass != "Equity" || vega[0].Amount != 300 {
		t.Errorf("vega mapping wrong: %+v", vega)
	}

	// End-to-end: a live CRIF drives the delta charge under the default params.
	if got := mustCharge(t, delta, DefaultParams()); got <= 0 {
		t.Errorf("CRIF-driven SBM charge must be positive, got %v", got)
	}
}

func TestParseCRIF_Errors(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"missing RiskType": "Qualifier\tAmount\nUSD\t1\n",
		"missing Amount":   "RiskType\tQualifier\nRisk_FX\tUSD\n",
		"non-numeric":      "RiskType\tAmount\nRisk_FX\tabc\n",
		"short row":        "RiskType\tQualifier\tAmount\nRisk_FX\n",
	}
	for name, in := range cases {
		if _, err := ParseCRIF(strings.NewReader(in)); !errors.Is(err, ErrCRIF) {
			t.Errorf("%s: want ErrCRIF, got %v", name, err)
		}
	}
}

// --- params -------------------------------------------------------------------

func TestDefaultParams_Valid(t *testing.T) {
	if err := ValidateParams(DefaultParams()); err != nil {
		t.Fatalf("DefaultParams must validate: %v", err)
	}
}

func TestValidateParams_Rejects(t *testing.T) {
	cases := map[string]Params{
		"empty":         {},
		"no weights":    {"GIRR": {IntraCorr: 0.5}},
		"zero weight":   {"GIRR": {RiskWeight: map[string]float64{"1": 0}, IntraCorr: 0.5}},
		"corr ≥ 1":      {"GIRR": {RiskWeight: map[string]float64{"1": 0.01}, IntraCorr: 1.0}},
		"negative corr": {"GIRR": {RiskWeight: map[string]float64{"1": 0.01}, InterCorr: -0.1}},
	}
	for name, p := range cases {
		if err := ValidateParams(p); !errors.Is(err, ErrParams) {
			t.Errorf("%s: want ErrParams, got %v", name, err)
		}
	}
}

// AN EMPTY DRCParams IS REFUSED, NOT WEIGHTED TO ZERO (#617).
//
// DRCParams{} has a nil RiskWeight and a zero UnratedWeight, so every
// jump-to-default position weighted to nothing: DRC contributed 0 to
// FRTB_TOTAL and the filing was SIGNED. handleFRTB decodes DRCParams straight
// from the request body, so `"drc_params": {}` reached the computation from
// outside the process.
//
// It is the same defect #565 fixed for Delta, Vega and Curvature — DRC was the
// one component of Total still computed without an error return.
func TestDRC_RefusesAnEmptyParamTable(t *testing.T) {
	pos := []JTDPosition{{Obligor: "X", Bucket: "corporates", Rating: "BBB", Amount: 1_000_000}}

	got, err := DRC(pos, DRCParams{})
	if err == nil {
		t.Fatalf("DRC with an empty DRCParams returned %v and no error.\n\n"+
			"A million of jump-to-default exposure contributed %v to FRTB_TOTAL, and the filing "+
			"assembled on it would be signed — indistinguishable from a book with no default risk.",
			got, got)
	}
	if !errors.Is(err, ErrUncovered) {
		t.Errorf("DRC error = %v, want it to wrap ErrUncovered so callers can tell an uncovered "+
			"table from any other failure", err)
	}
	if got != 0 {
		t.Errorf("DRC returned %v alongside its error — a refused computation must produce no "+
			"number a caller could file", got)
	}
}

// A RATING MISSING FROM A TABLE THAT OTHERWISE COVERS THE BOOK IS THE QUIETER
// HALF, and the one a "params is empty" check alone would miss: the aggregate
// is computed correctly for every other rating, and this one position vanishes
// inside it. Same shape as coversBuckets in sbm.go.
func TestDRC_RefusesOneUncoveredRatingInsideAGoodTable(t *testing.T) {
	p := DRCParams{RiskWeight: map[string]float64{"BBB": 0.06}} // UnratedWeight zero
	_, err := DRC([]JTDPosition{
		{Obligor: "X", Bucket: "corporates", Rating: "BBB", Amount: 100},
		{Obligor: "Y", Bucket: "corporates", Rating: "CCC", Amount: 5_000_000},
	}, p)
	if err == nil {
		t.Fatal("DRC accepted a rating the table does not cover. The BBB leg would be weighted " +
			"correctly and the five-million CCC leg would contribute nothing, which is a filing " +
			"that looks computed and is short by the whole uncovered exposure.")
	}
	if !strings.Contains(err.Error(), "CCC") {
		t.Errorf("DRC error = %v, want it to name the uncovered rating — a responder needs to know "+
			"WHICH rating is missing to fix the table", err)
	}
}

// AND A DELIBERATE UNRATED WEIGHT STILL APPLIES. Weighting unrated exposure at
// a stated rate is a supervisory choice; the refusal above must not break it,
// or the fix would make a correct configuration unusable.
func TestDRC_NonZeroUnratedWeightIsStillAFallback(t *testing.T) {
	p := DRCParams{RiskWeight: map[string]float64{"BBB": 0.06}, UnratedWeight: 0.15}
	got, err := DRC([]JTDPosition{{Obligor: "U", Bucket: "corporates", Rating: "NR?", Amount: 100}}, p)
	if err != nil {
		t.Fatalf("a non-zero UnratedWeight must still cover an unrated position: %v", err)
	}
	if want := 0.15 * 100; math.Abs(got-want) > 1e-9 {
		t.Errorf("unrated fallback: got %v want %v", got, want)
	}
}
