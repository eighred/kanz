package xva

// Credit-stress scenarios (XVA-01e) — the credit-axis counterpart of the
// price/curve/liquidity/factor stresses. A credit crisis does not move the trade
// prices the GICS scenarios shock; it widens credit spreads (raising hazard
// rates ⇒ higher CVA) or realizes a counterparty default. A CreditStress is a
// transform on the credit curve, the liquidity.Stress pattern one axis over: the
// exposure profile is unchanged, the default model is stressed.
type CreditStress struct {
	// HazardMult scales every hazard rate (≥1 widens spreads). ≤0 ⇒ 1 (no change).
	HazardMult float64
	// JumpToDefault collapses survival to ~0 (the counterparty defaults now), so
	// the loss crystallizes against the near-term exposure.
	JumpToDefault bool
}

// Apply returns the stressed credit curve.
func (s CreditStress) Apply(c CreditCurve) CreditCurve {
	if s.JumpToDefault {
		return CreditCurve{Tenors: []float64{100}, Hazards: []float64{1e6}, Recovery: c.Recovery}
	}
	m := s.HazardMult
	if m <= 0 {
		m = 1
	}
	h := make([]float64, len(c.Hazards))
	for i, v := range c.Hazards {
		h[i] = v * m
	}
	return CreditCurve{Tenors: append([]float64(nil), c.Tenors...), Hazards: h, Recovery: c.Recovery}
}

// ApplyTo returns a copy of the adjustment inputs with the counterparty curve
// stressed — so CVA/FVA recompute under the stress with no other change.
func (s CreditStress) ApplyTo(a Adjustments) Adjustments {
	a.Counterparty = s.Apply(a.Counterparty)
	return a
}
