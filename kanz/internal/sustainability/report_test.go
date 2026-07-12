package sustainability

import (
	"github.com/kanz-eng/kanz/internal/dec"
	"math/big"
	"testing"
	"time"
)

func TestGlidePathTargetAndTracking(t *testing.T) {
	g := GlidePath{BaseYear: 2020, TargetYear: 2050, BaseEmissions: 1000, ReductionFloor: 0.0}
	// Endpoints.
	if g.Target(2020) != 1000 || g.Target(2050) != 0 {
		t.Fatalf("glide-path endpoints: %v / %v", g.Target(2020), g.Target(2050))
	}
	// Midpoint (2035) is halfway: 500.
	if !approx(g.Target(2035), 500, 1e-9) {
		t.Fatalf("glide-path midpoint: want 500 got %v", g.Target(2035))
	}
	// On track: actual below target.
	if ok, gap := g.OnTrack(2035, 400); !ok || gap != -100 {
		t.Fatalf("under-target should be on-track: ok=%v gap=%v", ok, gap)
	}
	// Off track: actual above target.
	if ok, gap := g.OnTrack(2035, 600); ok || gap != 100 {
		t.Fatalf("over-target should be off-track: ok=%v gap=%v", ok, gap)
	}
}

func TestTemperatureAlignmentMonotone(t *testing.T) {
	// On budget ⇒ aligned at the target.
	rise, aligned := TemperatureAlignment(100, 100, 1.5, 3.0)
	if !aligned || !approx(rise, 1.5, 1e-9) {
		t.Fatalf("on-budget should align at target: rise=%v aligned=%v", rise, aligned)
	}
	// Overshoot ⇒ higher implied rise, not aligned.
	riseHi, alignedHi := TemperatureAlignment(200, 100, 1.5, 3.0)
	if alignedHi || riseHi <= rise {
		t.Fatalf("overshoot should raise temp and break alignment: rise=%v aligned=%v", riseHi, alignedHi)
	}
	// Monotone: a bigger overshoot ⇒ a higher implied rise.
	riseHigher, _ := TemperatureAlignment(300, 100, 1.5, 3.0)
	if riseHigher <= riseHi {
		t.Fatal("a larger overshoot should imply a higher temperature rise")
	}
}

func TestBuildReportCompletenessAndSigning(t *testing.T) {
	asOf := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	values := map[string]*big.Rat{
		"TCFD_WACI":               big.NewRat(92, 1),
		"TCFD_FINANCED_EMISSIONS": big.NewRat(115, 1),
		"TCFD_IMPLIED_TEMP_RISE":  big.NewRat(12, 5), // 2.4, exactly
		"TCFD_CLIMATE_VAR":        big.NewRat(50000, 1),
	}
	r, err := BuildReport(TCFD, asOf, values, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.LineItems) != 4 || r.Signature == "" {
		t.Fatalf("report incomplete: items=%d sig=%q", len(r.LineItems), r.Signature)
	}
	if v, ok := r.Lookup("TCFD_WACI"); !ok || v.Cmp(dec.Rat("92")) != 0 {
		t.Fatalf("lookup WACI: %v %v", v, ok)
	}

	// Missing a templated line item ⇒ no signed report (completeness gate).
	delete(values, "TCFD_CLIMATE_VAR")
	if _, err := BuildReport(TCFD, asOf, values, nil); err == nil {
		t.Fatal("missing required line item should error, not sign")
	}
}

func TestReportSignatureDeterministicAndTamperEvident(t *testing.T) {
	asOf := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	values := map[string]*big.Rat{
		"SFDR_GHG_INTENSITY":        dec.Rat("92"),
		"SFDR_CARBON_FOOTPRINT":     dec.Rat("115"),
		"SFDR_FOSSIL_FUEL_EXPOSURE": dec.Rat("0.05"),
	}
	a, _ := BuildReport(SFDR, asOf, values, nil)
	b, _ := BuildReport(SFDR, asOf, values, nil)
	if a.Signature != b.Signature {
		t.Fatal("signature should be deterministic for the same content")
	}
	// Tamper: change a value ⇒ a different signature.
	values["SFDR_CARBON_FOOTPRINT"] = dec.Rat("999")
	c, _ := BuildReport(SFDR, asOf, values, nil)
	if c.Signature == a.Signature {
		t.Fatal("tampered report should produce a different signature")
	}
}

func TestUnknownFramework(t *testing.T) {
	if _, err := BuildReport(Framework("XYZ"), time.Now(), map[string]*big.Rat{}, nil); err == nil {
		t.Fatal("unknown framework should error")
	}
}
