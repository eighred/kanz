package pricing

import "math"

// Cox-Ross-Rubinstein binomial tree â€” the American-option pricer (early exercise
// is checked at every node) and the model-agnostic bumped-Greeks engine.

// DefaultBinomialSteps is the existing Go pricing API tree depth. Discretization error
// depends on the contract and inputs; this is not a universal cent-accuracy
// guarantee or evidence of continuous-time American-model convergence.
const DefaultBinomialSteps = 200

// BinomialPrice values an option on a CRR tree. American exercise takes the max
// of continuation and intrinsic at each node; European discounts only the
// terminal payoff back. Finite degenerate inputs return intrinsic value.
// An inadmissible/nonfinite lattice returns NaN: substituting a European model
// would silently remove the American exercise right. Callers must keep this
// unavailable through coverage/refusal, rather than publishing a zero result.
func BinomialPrice(otype OptionType, exercise Exercise, S, K, t, r, q, sigma float64, steps int) float64 {
	if !finiteBinomial(S, K, t, r, q, sigma) {
		return math.NaN()
	}
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
	if !finiteBinomial(dt, u, d, disc, p) || dt <= 0 || u <= d || d <= 0 || disc <= 0 || p < 0 || p > 1 {
		return math.NaN()
	}

	// Terminal payoffs: node j has j up-moves, stepsâˆ’j down-moves.
	values := make([]float64, steps+1)
	for j := 0; j <= steps; j++ {
		st := S * math.Pow(u, float64(j)) * math.Pow(d, float64(steps-j))
		values[j] = intrinsic(otype, st, K)
		if !finiteBinomial(st, values[j]) {
			return math.NaN()
		}
	}
	// Backward induction.
	for i := steps - 1; i >= 0; i-- {
		for j := 0; j <= i; j++ {
			cont := disc * (p*values[j+1] + (1-p)*values[j])
			if exercise == American {
				st := S * math.Pow(u, float64(j)) * math.Pow(d, float64(i-j))
				if !finiteBinomial(st) {
					return math.NaN()
				}
				if ex := intrinsic(otype, st, K); ex > cont {
					cont = ex
				}
			}
			if !finiteBinomial(cont) {
				return math.NaN()
			}
			values[j] = cont
		}
	}
	return values[0]
}

// Bump sizes for finite-difference Greeks â€” relative to spot for Î”/Î“, absolute
// for the rate/vol/time variables, chosen small enough for accuracy yet large
// enough to stay clear of tree-granularity noise.
// bumpSpotRel is 1% of spot â€” large enough to average over the CRR node spacing
// (â‰ˆ SÂ·ÏƒÂ·âˆšdt), so the second difference (Gamma) is not swamped by tree
// quantization noise that a sub-node bump amplifies via 1/hÂ².
const (
	bumpSpotRel = 1e-2
	bumpVol     = 1e-3
	bumpRate    = 1e-4
	bumpTime    = 1e-4
)

// BinomialGreeks computes Greeks by central finite differences of the binomial
// price â€” the model-agnostic path used for American options (no closed form).
// Each Greek is the derivative w.r.t. its own variable, matching the analytic
// convention in pricing.go.
func BinomialGreeks(otype OptionType, exercise Exercise, S, K, t, r, q, sigma float64, steps int) Greeks {
	if !finiteBinomial(S, K, t, r, q, sigma) {
		return unavailableBinomialGreeks()
	}
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
		tt = bumpTime * 2 // keep tâˆ’bump positive near expiry
	}
	volUp, volDown := price(S, t, r, sigma+bumpVol), price(S, t, r, sigma-bumpVol)
	timeUp, timeDown := price(S, tt+bumpTime, r, sigma), price(S, tt-bumpTime, r, sigma)
	rateUp, rateDown := price(S, t, r+bumpRate, sigma), price(S, t, r-bumpRate, sigma)
	// One failed bump makes the whole sensitivity set unavailable. The registered
	// Greek measure selects one field at a time, so partial validity would hide the
	// model failure from whichever measure happened to select a finite field.
	if !finiteBinomial(up, mid, down, volUp, volDown, timeUp, timeDown, rateUp, rateDown) {
		return unavailableBinomialGreeks()
	}
	g := Greeks{
		Delta: (up - down) / (2 * hS),
		Gamma: (up - 2*mid + down) / (hS * hS),
		Vega:  (volUp - volDown) / (2 * bumpVol),
		Theta: (timeUp - timeDown) / (2 * bumpTime),
		Rho:   (rateUp - rateDown) / (2 * bumpRate),
	}
	if !finiteBinomial(g.Delta, g.Gamma, g.Vega, g.Theta, g.Rho) {
		return unavailableBinomialGreeks()
	}
	return g
}

func finiteBinomial(values ...float64) bool {
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
	}
	return true
}

func unavailableBinomialGreeks() Greeks {
	n := math.NaN()
	return Greeks{Delta: n, Gamma: n, Vega: n, Theta: n, Rho: n}
}
