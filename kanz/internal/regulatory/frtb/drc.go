package frtb

// Default risk charge, non-securitization (PARITY-03g, the MAR22 aggregation),
// and the residual risk add-on (MAR23). Both consume CRIF-style inputs — JTD
// amounts and notionals the deployment computes upstream (LGD scaling and any
// seniority-eligible offsetting are applied when the JTD is constructed).

// JTDPosition is one net jump-to-default exposure: signed Amount (+ long risk,
// − short risk), bucketed per MAR22 (corporates / sovereigns / local
// governments) and rated for the default risk weight.
type JTDPosition struct {
	Obligor string
	Bucket  string
	Rating  string
	Amount  float64
}

// DRCParams carries the rating → default risk weight table (MAR22.24).
type DRCParams struct {
	RiskWeight map[string]float64
	// UnratedWeight applies when a rating is missing from the table.
	UnratedWeight float64
}

// DefaultDRCParams is the published MAR22.24 default-risk-weight table.
func DefaultDRCParams() DRCParams {
	return DRCParams{
		RiskWeight: map[string]float64{
			"AAA":       0.005,
			"AA":        0.02,
			"A":         0.03,
			"BBB":       0.06,
			"BB":        0.15,
			"B":         0.30,
			"CCC":       0.50,
			"DEFAULTED": 1.00,
		},
		UnratedWeight: 0.15,
	}
}

// DRC is the non-securitization default risk charge: per bucket, net the JTD
// per obligor, compute the hedge-benefit ratio HBR = ΣnetLong/(ΣnetLong +
// Σ|netShort|), and charge max(0, Σ RW·netLong − HBR·Σ RW·|netShort|); buckets
// sum (no cross-bucket netting).
func DRC(positions []JTDPosition, p DRCParams) float64 {
	type ok struct{ bucket, obligor, rating string }
	netted := map[ok]float64{}
	for _, j := range positions {
		netted[ok{j.Bucket, j.Obligor, j.Rating}] += j.Amount
	}

	type bucketAccum struct {
		long, short   float64 // Σ net long, Σ |net short|
		wLong, wShort float64 // risk-weighted versions
	}
	buckets := map[string]*bucketAccum{}
	for k, amt := range netted {
		b := buckets[k.bucket]
		if b == nil {
			b = &bucketAccum{}
			buckets[k.bucket] = b
		}
		rw, found := p.RiskWeight[k.rating]
		if !found {
			rw = p.UnratedWeight
		}
		if amt >= 0 {
			b.long += amt
			b.wLong += rw * amt
		} else {
			b.short += -amt
			b.wShort += rw * -amt
		}
	}

	var total float64
	for _, b := range buckets {
		hbr := 0.0
		if b.long+b.short > 0 {
			hbr = b.long / (b.long + b.short)
		}
		if c := b.wLong - hbr*b.wShort; c > 0 {
			total += c
		}
	}
	return total
}

// RRAOPosition is one instrument's residual-risk notional: Exotic underlyings
// (longevity, weather, natural disasters…) carry the higher weight.
type RRAOPosition struct {
	Notional float64
	Exotic   bool
}

// RRAO weights per MAR23.8: 1.0% of notional for exotic underlyings, 0.1% for
// other residual risks.
const (
	rraoExoticWeight = 0.010
	rraoOtherWeight  = 0.001
)

// RRAO is the residual risk add-on: a plain weighted notional sum — no
// netting, no diversification (that is the point of the add-on).
func RRAO(positions []RRAOPosition) float64 {
	var total float64
	for _, p := range positions {
		n := p.Notional
		if n < 0 {
			n = -n
		}
		if p.Exotic {
			total += rraoExoticWeight * n
		} else {
			total += rraoOtherWeight * n
		}
	}
	return total
}
