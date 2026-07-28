package structured

import (
	"math"

	"github.com/eighred/kanz/internal/risk/pricing/curve"
)

// STRUCT-01c — the prepayment / default model. Prepayment is quoted as an annual
// CPR (conditional prepayment rate) and defaults as a CDR (conditional default
// rate); the monthly engine needs the single-monthly-mortality equivalents:
//
//	SMM = 1 − (1 − CPR)^(1/12)        MDR = 1 − (1 − CDR)^(1/12)
//
// Three models ship: a flat ConstantCPR, the PSA ramp, and a BEHAVIORAL model
// whose prepayment responds to the FI-01 rate curve — refinancing accelerates as
// rates fall below the pool coupon. That rate-responsiveness is what gives the
// STRUCT-01d effective duration its negative convexity (a mortgage shortens
// exactly when you don't want it to).

// RateEnv is the rate environment a projection runs in — the FI-01 curve plus the
// tenor the behavioral model reads its refinancing rate off.
type RateEnv struct {
	Curve    *curve.Curve
	RefTenor float64 // years; the point on the curve the refi rate is read at
}

// refRate is the current refinancing rate (the curve rate at RefTenor). A nil
// curve ⇒ 0 (a rate-insensitive environment).
func (e RateEnv) refRate() float64 {
	if e.Curve == nil {
		return 0
	}
	tenor := e.RefTenor
	if tenor <= 0 {
		tenor = 10
	}
	return e.Curve.Rate(tenor)
}

// PrepayModel supplies the monthly prepay (SMM) and default (MDR) rates and the
// loss severity. SMM may depend on the month, the pool coupon, and the rate
// environment (the behavioral model); MDR/Severity are scenario constants here.
type PrepayModel interface {
	SMM(month int, grossCoupon float64, env RateEnv) float64
	MDR(month int) float64
	Severity() float64
}

// cprToSMM converts an annual CPR to a single monthly mortality.
func cprToSMM(cpr float64) float64 {
	if cpr <= 0 {
		return 0
	}
	if cpr >= 1 {
		return 1
	}
	return 1 - math.Pow(1-cpr, 1.0/12)
}

// ConstantCPR is a flat-speed model — a fixed CPR/CDR regardless of rates.
type ConstantCPR struct {
	CPR float64
	CDR float64
	Sev float64
}

func (m ConstantCPR) SMM(int, float64, RateEnv) float64 { return cprToSMM(m.CPR) }
func (m ConstantCPR) MDR(int) float64                   { return cprToSMM(m.CDR) }
func (m ConstantCPR) Severity() float64                 { return m.Sev }

// PSA is the standard PSA ramp: CPR ramps linearly from 0 to 6% over the first 30
// months then holds, scaled by Multiple (1.0 == 100 PSA).
type PSA struct {
	Multiple float64
	CDR      float64
	Sev      float64
}

func (m PSA) SMM(month int, _ float64, _ RateEnv) float64 {
	cpr := 0.06 * math.Min(float64(month)/30, 1) * m.Multiple
	return cprToSMM(cpr)
}
func (m PSA) MDR(int) float64   { return cprToSMM(m.CDR) }
func (m PSA) Severity() float64 { return m.Sev }

// Behavioral is the rate-driven refi model: CPR rises on an S-curve with the
// refinancing incentive (pool coupon − current refi rate). When rates fall, the
// incentive rises and prepayment accelerates — the source of the negative
// convexity STRUCT-01d measures.
type Behavioral struct {
	Base      float64 // baseline CPR at zero incentive (turnover)
	Max       float64 // saturated CPR at deep in-the-money
	Steepness float64 // S-curve slope on the incentive
	CDR       float64
	Sev       float64
}

func (m Behavioral) SMM(_ int, grossCoupon float64, env RateEnv) float64 {
	incentive := grossCoupon - env.refRate()
	// Logistic S-curve from Base (incentive≤0) toward Max (incentive≫0).
	cpr := m.Base + (m.Max-m.Base)/(1+math.Exp(-m.Steepness*incentive))
	// At zero incentive the logistic is at the midpoint; recentre so no incentive
	// ⇒ Base (turnover only), rising toward Max as the loan goes in-the-money.
	zero := m.Base + (m.Max-m.Base)/2
	cpr = m.Base + (cpr - zero)
	if cpr < 0 {
		cpr = 0
	}
	return cprToSMM(cpr)
}
func (m Behavioral) MDR(int) float64   { return cprToSMM(m.CDR) }
func (m Behavioral) Severity() float64 { return m.Sev }
