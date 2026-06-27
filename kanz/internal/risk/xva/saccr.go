package xva

import "math"

// XVA-01d — the Basel SA-CCR standardized exposure (CRE52), the regulatory
// counterparty exposure that feeds REG-01 capital. It replaces the Current
// Exposure Method with a risk-sensitive add-on:
//
//	EAD = α · (RC + PFE),  α = 1.4
//	RC  = max(V − C, 0)                              (replacement cost)
//	PFE = multiplier · Σ_assetclass AddOn            (potential future exposure)
//	multiplier = min(1, Floor + (1−Floor)·exp((V−C) / (2(1−Floor)·ΣAddOn)))
//
// Each trade contributes an effective notional D = δ · d · MF (supervisory delta
// × adjusted notional × maturity factor); trades aggregate within a hedging set,
// hedging sets aggregate within an asset class (a simple sum for IR/FX; a single-
// factor correlation for equity/credit/commodity), and the asset-class add-ons
// sum. This is a faithful unmargined SA-CCR for the common asset classes.

const (
	saccrAlpha = 1.4
	saccrFloor = 0.05
	// businessDayFloor is the 10-business-day maturity-factor floor (~10/250y).
	businessDayFloor = 10.0 / 250.0
)

// AssetClass is the SA-CCR asset class a trade belongs to (CRE52 §38).
type AssetClass int

const (
	IR AssetClass = iota
	FX
	Credit
	Equity
	Commodity
)

// SACCRTrade is one trade's SA-CCR inputs. Callers compute AdjustedNotional per
// asset class (SupervisoryDuration helps for IR/credit; it is the current market
// value of the underlying for equity/FX/commodity) and the supervisory Delta
// (±1 for linear, OptionDelta for options). SupervisoryFactor / Correlation
// default from the asset class via DefaultSupervisoryFactor / DefaultCorrelation
// when left zero.
type SACCRTrade struct {
	AssetClass        AssetClass
	HedgingSet        string  // currency (IR/FX) or reference entity/index (equity/credit/commodity)
	AdjustedNotional  float64 // d
	Delta             float64 // supervisory delta (signed)
	MaturityYears     float64 // remaining maturity M (maturity factor)
	SupervisoryFactor float64 // SF; 0 ⇒ asset-class default
	Correlation       float64 // ρ; 0 ⇒ asset-class default
}

// MaturityFactorUnmargined is √(min(M,1)) with the 10-business-day floor — the
// unmargined SA-CCR maturity factor.
func MaturityFactorUnmargined(m float64) float64 {
	if m < businessDayFloor {
		m = businessDayFloor
	}
	if m > 1 {
		m = 1
	}
	return math.Sqrt(m)
}

// SupervisoryDuration is the IR/credit adjusted-notional duration
// (exp(−0.05·S) − exp(−0.05·E)) / 0.05 for a leg starting at S and ending at E
// (years).
func SupervisoryDuration(start, end float64) float64 {
	return (math.Exp(-0.05*start) - math.Exp(-0.05*end)) / 0.05
}

// effectiveNotional is D = δ · d · MF for one trade.
func (t SACCRTrade) effectiveNotional() float64 {
	return t.Delta * t.AdjustedNotional * MaturityFactorUnmargined(t.MaturityYears)
}

func (t SACCRTrade) sf() float64 {
	if t.SupervisoryFactor > 0 {
		return t.SupervisoryFactor
	}
	return DefaultSupervisoryFactor(t.AssetClass)
}

func (t SACCRTrade) rho() float64 {
	if t.Correlation > 0 {
		return t.Correlation
	}
	return DefaultCorrelation(t.AssetClass)
}

// SACCRNettingSet is the netting-set-level SA-CCR input: the trades plus the
// current net mark-to-market V and the net collateral C held.
type SACCRNettingSet struct {
	Trades     []SACCRTrade
	NetMtM     float64 // V
	Collateral float64 // C
}

// EAD computes the SA-CCR exposure-at-default for the netting set.
func (ns SACCRNettingSet) EAD() float64 {
	addOn := ns.aggregateAddOn()
	vc := ns.NetMtM - ns.Collateral
	mult := 1.0
	if addOn > 0 {
		mult = saccrFloor + (1-saccrFloor)*math.Exp(vc/(2*(1-saccrFloor)*addOn))
		if mult > 1 {
			mult = 1
		}
	}
	pfe := mult * addOn
	rc := math.Max(vc, 0)
	return saccrAlpha * (rc + pfe)
}

// aggregateAddOn sums the per-asset-class add-ons.
func (ns SACCRNettingSet) aggregateAddOn() float64 {
	byClass := make(map[AssetClass][]SACCRTrade)
	for _, t := range ns.Trades {
		byClass[t.AssetClass] = append(byClass[t.AssetClass], t)
	}
	var total float64
	for class, trades := range byClass {
		total += assetClassAddOn(class, trades)
	}
	return total
}

// assetClassAddOn aggregates a single asset class. Trades group into hedging
// sets; each hedging set's add-on is SF·|Σ D|. IR/FX sum the hedging-set add-ons;
// equity/credit/commodity aggregate them with a single-factor correlation ρ
// (CRE52): AddOn = √((Σ ρ·AddOnₕ)² + Σ (1−ρ²)·AddOnₕ²).
func assetClassAddOn(class AssetClass, trades []SACCRTrade) float64 {
	type hs struct {
		d   float64
		sf  float64
		rho float64
	}
	sets := make(map[string]*hs)
	for _, t := range trades {
		s, ok := sets[t.HedgingSet]
		if !ok {
			s = &hs{sf: t.sf(), rho: t.rho()}
			sets[t.HedgingSet] = s
		}
		s.d += t.effectiveNotional()
	}

	simpleSum := class == IR || class == FX
	var sum, systematic, idiosyncratic float64
	for _, s := range sets {
		addOn := s.sf * math.Abs(s.d)
		if simpleSum {
			sum += addOn
			continue
		}
		systematic += s.rho * addOn
		idiosyncratic += (1 - s.rho*s.rho) * addOn * addOn
	}
	if simpleSum {
		return sum
	}
	return math.Sqrt(systematic*systematic + idiosyncratic)
}

// DefaultSupervisoryFactor returns the CRE52 supervisory factor for an asset
// class (the common single-name values; a deployment refines by subtype/rating).
func DefaultSupervisoryFactor(a AssetClass) float64 {
	switch a {
	case IR:
		return 0.005
	case FX:
		return 0.04
	case Credit:
		return 0.0046
	case Equity:
		return 0.32
	case Commodity:
		return 0.18
	default:
		return 0.32
	}
}

// DefaultCorrelation returns the CRE52 single-factor correlation for an asset
// class (single-name default; an index hedging set is 0.80).
func DefaultCorrelation(a AssetClass) float64 {
	switch a {
	case Equity:
		return 0.50
	case Credit:
		return 0.50
	case Commodity:
		return 0.40
	default:
		return 0.50
	}
}

// OptionDelta is the SA-CCR supervisory delta for a bought/sold call/put
// (CRE52 §159): ±Φ(±(ln(P/K) + 0.5σ²T)/(σ√T)). sign is +1 long / −1 short the
// option; call=true for a call. P spot, K strike, sigma supervisory vol, T
// maturity. Provided so an option trade's effective notional is delta-adjusted.
func OptionDelta(long, call bool, p, k, sigma, t float64) float64 {
	if sigma <= 0 || t <= 0 || p <= 0 || k <= 0 {
		// Degenerate ⇒ fall back to the linear ±1.
		return linearSign(long, call)
	}
	d := (math.Log(p/k) + 0.5*sigma*sigma*t) / (sigma * math.Sqrt(t))
	phi := normCDF(d)
	var delta float64
	if call {
		delta = phi
	} else {
		delta = phi - 1 // −Φ(−d)
	}
	if !long {
		delta = -delta
	}
	return delta
}

func linearSign(long, call bool) float64 {
	s := 1.0
	if !call {
		s = -1
	}
	if !long {
		s = -s
	}
	return s
}

// normCDF is the standard-normal CDF (local copy — pricing's is package-private).
func normCDF(x float64) float64 { return 0.5 * math.Erfc(-x/math.Sqrt2) }
