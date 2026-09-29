package volsurface

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/risk/pricing"
)

func TestImpliedVolRejectsInvalidInputs(t *testing.T) {
	for field := range 6 {
		for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 0, -1} {
			if field >= 4 && finite(bad) { // finite negative rates/carry are legitimate
				continue
			}
			x := []float64{10, 100, 100, 1, .02, 0}
			x[field] = bad
			if v, ok := ImpliedVol(pricing.Call, x[0], x[1], x[2], x[3], x[4], x[5]); ok || v != 0 {
				t.Fatalf("field %d input %v accepted: %v %v", field, bad, v, ok)
			}
		}
	}
	if _, ok := ImpliedVol(pricing.OptionType(99), 10, 100, 100, 1, 0, 0); ok {
		t.Fatal("unknown option type accepted")
	}
	for _, x := range [][6]float64{
		{200, 100, 100, 1, 0, 0},
		{10, 100, 100, 1, -1000, 0},
		{10, 100, 100, math.MaxFloat64, 1, -1},
	} {
		if _, err := impliedVol(pricing.Call, x[0], x[1], x[2], x[3], x[4], x[5]); !errors.Is(err, ErrCalibration) {
			t.Fatalf("uncalibratable quote: %v", err)
		}
	}
}

func TestImpliedVolCallPutResidualAndRecovery(t *testing.T) {
	for _, typ := range []pricing.OptionType{pricing.Call, pricing.Put} {
		for _, rate := range []float64{-.03, 0, .08} {
			for _, carry := range []float64{-.02, .04} {
				for _, strike := range []float64{80, 100, 120} {
					for _, want := range []float64{.15, .4, 1.2, 4.9} {
						price := pricing.BlackScholesPrice(typ, 100, strike, 1, rate, carry, want)
						got, ok := ImpliedVol(typ, price, 100, strike, 1, rate, carry)
						if !ok || math.Abs(got-want) > 1e-6 {
							t.Fatalf("round trip: type=%v rate=%v carry=%v strike=%v got=%v want=%v", typ, rate, carry, strike, got, want)
						}
						if residual := math.Abs(pricing.BlackScholesPrice(typ, 100, strike, 1, rate, carry, got) - price); residual > 1e-10*math.Max(1, price) {
							t.Fatalf("uncertified residual %v", residual)
						}
					}
				}
			}
		}
	}
}

func TestSurfaceRejectsInvalidNodesAndQueries(t *testing.T) {
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 0, -1} {
		for field := range 3 {
			p := Point{100, 1, .2}
			switch field {
			case 0:
				p.Strike = bad
			case 1:
				p.Expiry = bad
			case 2:
				p.Vol = bad
			}
			if _, err := Build([]Point{p}); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("invalid node %v: %v", p, err)
			}
		}
		s, err := Build([]Point{{100, 1, .2}})
		if err != nil {
			t.Fatal(err)
		}
		for _, pair := range [][2]float64{{bad, 1}, {100, bad}} {
			if v, ok := s.Vol(pair[0], pair[1]); ok || v != 0 {
				t.Fatalf("invalid query accepted: %v %v", v, ok)
			}
			if v, ok := flatSurface(.04).Vol(pair[0], pair[1]); ok || v != 0 {
				t.Fatalf("invalid SVI query accepted: %v %v", v, ok)
			}
		}
	}
	if _, err := Build([]Point{{100, 1, .2}, {100, 1, .3}}); !errors.Is(err, ErrGrid) {
		t.Fatal(err)
	}
	// This input used to allocate a million cells before recognizing 999,000 holes.
	diagonal := make([]Point, 1000)
	for i := range diagonal {
		diagonal[i] = Point{float64(i + 1), float64(i + 1), .2}
	}
	if _, err := Build(diagonal); !errors.Is(err, ErrGrid) {
		t.Fatal(err)
	}
}

func TestCalibrationDoesNotSilentlyDropCorruptQuotes(t *testing.T) {
	quotes := sviQuotes(t, SVIParams{A: .04}, 1, 100, 0, 0, []float64{80, 90, 100, 110, 120})
	for _, bad := range []OptionQuote{
		{Type: pricing.Call, Strike: 100, Expiry: 2, Price: math.NaN()},
		{Type: pricing.Call, Strike: 100, Expiry: 2, Price: 1000},
		{Type: pricing.OptionType(99), Strike: 100, Expiry: 2, Price: 10},
	} {
		batch := append(append([]OptionQuote(nil), quotes...), bad)
		if _, err := FromQuotes(batch, 100, 0, 0); err == nil {
			t.Fatal("grid dropped corrupt quote")
		}
		if _, err := FitSVI(batch, 100, pricing.FlatCurve(0), 0); !errors.Is(err, ErrSVIFit) {
			t.Fatalf("SVI dropped corrupt quote: %v", err)
		}
	}
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := FitSVI(quotes, 100, pricing.FlatCurve(bad), 0); err == nil {
			t.Fatal("bad curve accepted")
		}
		if _, err := FitSVI(quotes, bad, pricing.FlatCurve(0), 0); err == nil {
			t.Fatal("bad spot accepted")
		}
		if _, err := FitSVI(quotes, 100, pricing.FlatCurve(0), bad); err == nil {
			t.Fatal("bad carry accepted")
		}
		for field := range 5 {
			p := SVIParams{A: .04, B: .1, Sigma: .2}
			switch field {
			case 0:
				p.A = bad
			case 1:
				p.B = bad
			case 2:
				p.Rho = bad
			case 3:
				p.M = bad
			case 4:
				p.Sigma = bad
			}
			if p.ButterflyFree(-1, 1) {
				t.Fatal("non-finite SVI certified")
			}
		}
		if (SVIParams{A: .04}).ButterflyFree(bad, 1) {
			t.Fatal("non-finite domain certified")
		}
	}
	flat := SVIParams{A: .04}
	if !flat.ButterflyFree(-1, 1) || flat.dw(0) != 0 || flat.d2w(0) != 0 {
		t.Fatal("flat slice must have finite zero derivatives")
	}
}

func TestInvalidRefreshPreservesValidPITSurfaceConcurrently(t *testing.T) {
	ctx := context.Background()
	quotes := sviQuotes(t, SVIParams{A: .04}, 1, 100, 0, 0, []float64{80, 90, 100, 110, 120})
	asOf := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st := NewStore()
	cal := Calibrator{Source: staticVolQuotes{quotes: quotes, spot: 100}, Store: st, Disc: pricing.FlatCurve(0)}
	if _, err := cal.Refresh(ctx, "X", asOf); err != nil {
		t.Fatal(err)
	}
	want, ok := st.Vol(ctx, "X", 100, 1, asOf)
	if !ok {
		t.Fatal("valid surface missing")
	}
	quotes = append(append([]OptionQuote(nil), quotes...), OptionQuote{Type: pricing.Call, Strike: 100, Expiry: 2, Price: math.NaN()})
	cal.Source = staticVolQuotes{quotes: quotes, spot: 100}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				if _, err := cal.Refresh(ctx, "X", asOf.Add(time.Hour)); !errors.Is(err, ErrInvalidInput) {
					t.Errorf("bad refresh accepted: %v", err)
				}
				if got, ok := st.Vol(ctx, "X", 100, 1, asOf.Add(2*time.Hour)); !ok || got != want {
					t.Errorf("valid version corrupted: %v %v", got, ok)
				}
			}
		})
	}
	wg.Wait()
	if _, ok := st.Vol(ctx, "X", 100, 1, asOf.Add(-time.Second)); ok {
		t.Fatal("future surface leaked")
	}
}

func FuzzImpliedVolCertificate(f *testing.F) {
	f.Add(uint8(0), 10.0, 100.0, 100.0, 1.0, .02, 0.0)
	f.Add(uint8(1), math.NaN(), 100.0, 100.0, 1.0, 0.0, 0.0)
	f.Fuzz(func(t *testing.T, typ uint8, price, spot, strike, expiry, rate, carry float64) {
		v, ok := ImpliedVol(pricing.OptionType(typ), price, spot, strike, expiry, rate, carry)
		if !ok {
			if v != 0 {
				t.Fatal("failed solve returned a usable value")
			}
			return
		}
		if !positive(v) || v < 1e-6 || v > 5 || !validQuote(OptionQuote{Type: pricing.OptionType(typ), Price: price, Strike: strike, Expiry: expiry}) || !positive(spot) || !finite(rate) || !finite(carry) {
			t.Fatal("invalid success")
		}
		residual := math.Abs(pricing.BlackScholesPrice(pricing.OptionType(typ), spot, strike, expiry, rate, carry, v) - price)
		if !finite(residual) || residual > 1e-10*math.Max(1, price) {
			t.Fatalf("invalid residual %v", residual)
		}
	})
}

func FuzzSurfaceQuery(f *testing.F) {
	f.Add(100.0, 1.0, .2, 100.0, 1.0)
	f.Add(math.NaN(), 1.0, .2, 100.0, 1.0)
	f.Fuzz(func(t *testing.T, k, e, v, qk, qe float64) {
		if got, ok := flatSurface(.04).Vol(qk, qe); ok && (!positive(qk) || !positive(qe) || !positive(got)) {
			t.Fatal("invalid SVI query success")
		}
		s, err := Build([]Point{{k, e, v}})
		if err != nil {
			return
		}
		if !positive(k) || !positive(e) || !positive(v) {
			t.Fatal("invalid grid accepted")
		}
		got, ok := s.Vol(qk, qe)
		if ok && (!positive(qk) || !positive(qe) || !positive(got) || got != v) {
			t.Fatal("invalid query success")
		}
	})
}
