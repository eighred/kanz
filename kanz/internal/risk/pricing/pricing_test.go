package pricing

import (
	"math"
	"testing"
)

const tol = 1e-6

func approx(t *testing.T, name string, got, want, eps float64) {
	t.Helper()
	if math.Abs(got-want) > eps {
		t.Fatalf("%s: got %.8f want %.8f (Δ %.2e > %.2e)", name, got, want, math.Abs(got-want), eps)
	}
}

// Textbook reference: S=100, K=100, t=1, r=5%, q=0, σ=20% ⇒ call≈10.4506,
// put≈5.5735 (Hull, Options Futures and Other Derivatives).
func TestBlackScholes_ReferenceValues(t *testing.T) {
	call := BlackScholesPrice(Call, 100, 100, 1, 0.05, 0, 0.20)
	put := BlackScholesPrice(Put, 100, 100, 1, 0.05, 0, 0.20)
	approx(t, "call", call, 10.4506, 1e-3)
	approx(t, "put", put, 5.5735, 1e-3)
}

func TestPutCallParity(t *testing.T) {
	S, K, ttm, r, q, sigma := 120.0, 100.0, 0.75, 0.03, 0.01, 0.35
	call := BlackScholesPrice(Call, S, K, ttm, r, q, sigma)
	put := BlackScholesPrice(Put, S, K, ttm, r, q, sigma)
	// C − P = S·e^{−qt} − K·e^{−rt}
	lhs := call - put
	rhs := S*math.Exp(-q*ttm) - K*math.Exp(-r*ttm)
	approx(t, "put-call parity", lhs, rhs, 1e-9)
}

func TestBlackScholesGreeks_VsFiniteDifference(t *testing.T) {
	S, K, ttm, r, q, sigma := 100.0, 105.0, 0.5, 0.04, 0.0, 0.25
	for _, ot := range []OptionType{Call, Put} {
		g := BlackScholesGreeks(ot, S, K, ttm, r, q, sigma)
		price := func(s, tt, rr, sig float64) float64 { return BlackScholesPrice(ot, s, K, tt, rr, q, sig) }

		h := 1e-4 * S
		fdDelta := (price(S+h, ttm, r, sigma) - price(S-h, ttm, r, sigma)) / (2 * h)
		fdGamma := (price(S+h, ttm, r, sigma) - 2*price(S, ttm, r, sigma) + price(S-h, ttm, r, sigma)) / (h * h)
		hv := 1e-5
		fdVega := (price(S, ttm, r, sigma+hv) - price(S, ttm, r, sigma-hv)) / (2 * hv)
		ht := 1e-5
		fdTheta := (price(S, ttm+ht, r, sigma) - price(S, ttm-ht, r, sigma)) / (2 * ht)
		hr := 1e-6
		fdRho := (price(S, ttm, r+hr, sigma) - price(S, ttm, r-hr, sigma)) / (2 * hr)

		approx(t, string(rune('C'))+" delta", g.Delta, fdDelta, 1e-5)
		approx(t, "gamma", g.Gamma, fdGamma, 1e-4)
		approx(t, "vega", g.Vega, fdVega, 1e-3)
		approx(t, "theta", g.Theta, fdTheta, 1e-3)
		approx(t, "rho", g.Rho, fdRho, 1e-3)
	}
}

// Binomial European converges to Black-Scholes; American put ≥ European put
// (early exercise has non-negative value).
func TestBinomial_ConvergenceAndAmericanPremium(t *testing.T) {
	S, K, ttm, r, q, sigma := 100.0, 100.0, 1.0, 0.05, 0.0, 0.20

	bsCall := BlackScholesPrice(Call, S, K, ttm, r, q, sigma)
	binEuroCall := BinomialPrice(Call, European, S, K, ttm, r, q, sigma, 500)
	approx(t, "binomial≈BS call", binEuroCall, bsCall, 0.02)

	euroPut := BinomialPrice(Put, European, S, K, ttm, r, q, sigma, 500)
	amerPut := BinomialPrice(Put, American, S, K, ttm, r, q, sigma, 500)
	if amerPut < euroPut-tol {
		t.Fatalf("American put %.6f < European put %.6f", amerPut, euroPut)
	}
	// A deep ITM American put on a positive-rate underlying should carry a
	// strictly positive early-exercise premium.
	deepEuro := BinomialPrice(Put, European, 60, 100, ttm, r, q, sigma, 500)
	deepAmer := BinomialPrice(Put, American, 60, 100, ttm, r, q, sigma, 500)
	if deepAmer <= deepEuro {
		t.Fatalf("expected early-exercise premium: American %.4f ≤ European %.4f", deepAmer, deepEuro)
	}
}

func TestBinomialGreeks_AgreeWithAnalytic(t *testing.T) {
	// For a European option the binomial bumped Greeks should track the BS
	// analytic Greeks closely.
	S, K, ttm, r, q, sigma := 100.0, 100.0, 1.0, 0.05, 0.0, 0.20
	a := BlackScholesGreeks(Call, S, K, ttm, r, q, sigma)
	b := BinomialGreeks(Call, European, S, K, ttm, r, q, sigma, 500)
	approx(t, "delta", b.Delta, a.Delta, 5e-3)
	approx(t, "gamma", b.Gamma, a.Gamma, 2e-2)
	approx(t, "vega", b.Vega, a.Vega, 5e-2)
}

func TestDegenerateInputs(t *testing.T) {
	// Expired ⇒ intrinsic; zero greeks.
	approx(t, "expired call", BlackScholesPrice(Call, 110, 100, 0, 0.05, 0, 0.2), 10, tol)
	if g := (BlackScholesGreeks(Call, 110, 100, 0, 0.05, 0, 0.2)); g != (Greeks{}) {
		t.Fatalf("expired greeks should be zero, got %+v", g)
	}
}

func TestDeltaGammaVaR(t *testing.T) {
	// Long a call: positive delta, positive gamma. The adverse move is down;
	// gamma cushions it, so VaR < a delta-only estimate.
	dollarDelta, dollarGamma, spot, sigma, z := 5000.0, 200.0, 100.0, 0.02, 2.326
	v := DeltaGammaVaR(dollarDelta, dollarGamma, spot, sigma, z)
	move := z * sigma * spot
	deltaOnly := dollarDelta * move
	if v <= 0 {
		t.Fatalf("expected positive VaR, got %.4f", v)
	}
	if v >= deltaOnly {
		t.Fatalf("long gamma should reduce downside VaR below delta-only: VaR %.2f, delta-only %.2f", v, deltaOnly)
	}
	// Hand check: loss on the down move = δ·(−dS) + ½γ·dS².
	wantLoss := -DeltaGammaPnL(dollarDelta, dollarGamma, -move)
	approx(t, "delta-gamma VaR", v, wantLoss, 1e-6)
}
