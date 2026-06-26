package pricing

import "math"

// Cox-Ross-Rubinstein binomial tree — the American-option pricer (early exercise
// is checked at every node) and the model-agnostic bumped-Greeks engine.

// DefaultBinomialSteps is the tree depth. 200 steps converges CRR to within a
// cent of Black-Scholes for a European option (the DERIV-01f convergence check)
// while staying fast enough for per-position aggregation.
const DefaultBinomialSteps = 200

// BinomialPrice values an option on a CRR tree. American exercise takes the max
// of continuation and intrinsic at each node; European discounts only the
// terminal payoff back. Degenerate inputs return intrinsic value.
func BinomialPrice(otype OptionType, exercise Exercise, S, K, t, r, q, sigma float64, steps int) float64 {
	if t <= 0 || sigma <= 0 || S <= 0 || K <= 0 {
		return intrinsic(otype, S, K)
	}
	if steps < 1 {
		steps = DefaultBinomialSteps
	}
	dt := t / float64(steps)
	u := math.Exp(sigma * math.Sqrt(dt))
	d := 1 / u
	disc := math.Exp(-r * dt)
	// Risk-neutral up-probability under dividend yield q.
	p := (math.Exp((r-q)*dt) - d) / (u - d)
	if p < 0 || p > 1 {
		// Numerically unstable (extreme rate/vol/dt); fall back to BS European
		// value rather than emit a garbage tree price.
		return BlackScholesPrice(otype, S, K, t, r, q, sigma)
	}

	// Terminal payoffs: node j has j up-moves, steps−j down-moves.
	values := make([]float64, steps+1)
	for j := 0; j <= steps; j++ {
		st := S * math.Pow(u, float64(j)) * math.Pow(d, float64(steps-j))
		values[j] = intrinsic(otype, st, K)
	}
	// Backward induction.
	for i := steps - 1; i >= 0; i-- {
		for j := 0; j <= i; j++ {
			cont := disc * (p*values[j+1] + (1-p)*values[j])
			if exercise == American {
				st := S * math.Pow(u, float64(j)) * math.Pow(d, float64(i-j))
				if ex := intrinsic(otype, st, K); ex > cont {
					cont = ex
				}
			}
			values[j] = cont
		}
	}
	return values[0]
}

// Bump sizes for finite-difference Greeks — relative to spot for Δ/Γ, absolute
// for the rate/vol/time variables, chosen small enough for accuracy yet large
// enough to stay clear of tree-granularity noise.
// bumpSpotRel is 1% of spot — large enough to average over the CRR node spacing
// (≈ S·σ·√dt), so the second difference (Gamma) is not swamped by tree
// quantization noise that a sub-node bump amplifies via 1/h².
const (
	bumpSpotRel = 1e-2
	bumpVol     = 1e-3
	bumpRate    = 1e-4
	bumpTime    = 1e-4
)

// BinomialGreeks computes Greeks by central finite differences of the binomial
// price — the model-agnostic path used for American options (no closed form).
// Each Greek is the derivative w.r.t. its own variable, matching the analytic
// convention in pricing.go.
func BinomialGreeks(otype OptionType, exercise Exercise, S, K, t, r, q, sigma float64, steps int) Greeks {
	if t <= 0 || sigma <= 0 || S <= 0 || K <= 0 {
		return Greeks{}
	}
	price := func(s, tt, rr, sig float64) float64 {
		return BinomialPrice(otype, exercise, s, K, tt, rr, q, sig, steps)
	}
	hS := S * bumpSpotRel
	up, mid, down := price(S+hS, t, r, sigma), price(S, t, r, sigma), price(S-hS, t, r, sigma)

	tt := t
	if tt-bumpTime <= 0 {
		tt = bumpTime * 2 // keep t−bump positive near expiry
	}
	return Greeks{
		Delta: (up - down) / (2 * hS),
		Gamma: (up - 2*mid + down) / (hS * hS),
		Vega:  (price(S, t, r, sigma+bumpVol) - price(S, t, r, sigma-bumpVol)) / (2 * bumpVol),
		Theta: (price(S, tt+bumpTime, r, sigma) - price(S, tt-bumpTime, r, sigma)) / (2 * bumpTime),
		Rho:   (price(S, t, r+bumpRate, sigma) - price(S, t, r-bumpRate, sigma)) / (2 * bumpRate),
	}
}
