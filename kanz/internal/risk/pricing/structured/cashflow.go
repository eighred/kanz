// Package structured is the STRUCT-01 securitized & structured-products engine —
// the deterministic waterfall cashflow projection, the prepayment/default model,
// and the OAS / effective-duration risk for MBS / ABS / CLO and amortizing
// structures. It turns the ASSET_CLASS tag into real price and risk (ROI #34).
//
// It lives under kanz/internal/risk/pricing (inside the risk module, no RISK-02
// edge), the structured sibling of the FI-01 bond pricer — and it reuses the
// FI-01 curve.Curve directly: the discount basis for pricing, and the rate the
// behavioral prepayment model reads its refinancing incentive off (STRUCT-01c),
// which is what gives the OAS/effective-duration its option-adjusted (negatively
// convex) character.
//
// # Working shape
//
// The engine works in float64 (cashflows are derived projections, like the bond
// analytics) with its own Deal/Pool/Tranche structs — the float mirror of the
// reference.v1.StructuredTerms wire schema (the FI-01 BondSpec precedent).
package structured

// Pool is the amortizing collateral backing a deal.
type Pool struct {
	// Balance is the original pool principal.
	Balance float64
	// GrossCoupon is the annualized weighted-average coupon the collateral pays.
	GrossCoupon float64
	// ServicingFee is the annualized fee stripped before the waterfall.
	ServicingFee float64
	// TermMonths is the fully-amortizing term (WAM).
	TermMonths int
}

// netCoupon is the coupon available to the structure after servicing.
func (p Pool) netCoupon() float64 { return p.GrossCoupon - p.ServicingFee }

// Tranche is one note in the capital structure. Tranches in a Deal are ordered
// senior → junior; principal pays sequentially down the order, losses allocate
// up from the bottom.
type Tranche struct {
	Name    string
	Balance float64
	Coupon  float64 // annualized
}

// Deal is a securitization: the collateral pool and its capital structure.
type Deal struct {
	Pool     Pool
	Tranches []Tranche
}

// PeriodCashflow is the pool's cashflow for one month.
type PeriodCashflow struct {
	Month        int
	BeginBalance float64
	Interest     float64 // net interest collected (after servicing)
	Principal    float64 // scheduled + prepay + recovery
	Loss         float64 // principal written off (default × severity)
}

// TrancheCashflow is one tranche's projected cashflow stream.
type TrancheCashflow struct {
	Name      string
	Interest  []float64
	Principal []float64
	Balance   []float64 // end-of-month balance
	Writedown []float64 // principal written down by loss allocation
}

// Projection is the full deterministic waterfall result.
type Projection struct {
	Pool     []PeriodCashflow
	Tranches []TrancheCashflow
	// Residual is the excess cash (interest above tranche coupons, principal after
	// all tranches retire) paid to the equity/residual holder each month. Tracking
	// it makes the cash-conservation invariant exact: pool cash == Σ tranche
	// (interest+principal) + residual.
	Residual []float64
}

// Project runs the deterministic monthly waterfall over the deal under a prepay
// model and the prevailing rate curve (which the behavioral model reads its
// incentive from). It returns the pool and per-tranche cashflow streams. The
// waterfall is sequential-pay: interest senior→junior at each tranche's coupon,
// principal senior→junior, losses allocated junior→senior.
func (d Deal) Project(pm PrepayModel, env RateEnv) Projection {
	n := d.Pool.TermMonths
	r := d.Pool.GrossCoupon / 12
	pmt := levelPayment(d.Pool.Balance, r, n)

	proj := Projection{Residual: make([]float64, 0, n)}
	tcf := make([]TrancheCashflow, len(d.Tranches))
	for i, t := range d.Tranches {
		tcf[i].Name = t.Name
	}
	balances := make([]float64, len(d.Tranches))
	for i, t := range d.Tranches {
		balances[i] = t.Balance
	}

	bal := d.Pool.Balance
	for m := 1; m <= n && bal > 1e-6; m++ {
		smm := pm.SMM(m, d.Pool.GrossCoupon, env)
		mdr := pm.MDR(m)
		sev := pm.Severity()

		grossInterest := bal * r
		schedPrin := pmt - grossInterest
		if schedPrin > bal {
			schedPrin = bal
		}
		if schedPrin < 0 {
			schedPrin = 0
		}
		defaulted := mdr * bal
		survived := bal - schedPrin - defaulted
		if survived < 0 {
			survived = 0
		}
		prepay := smm * survived
		recovery := defaulted * (1 - sev)
		loss := defaulted * sev

		netInterest := bal * d.Pool.netCoupon() / 12
		principalCollected := schedPrin + prepay + recovery

		// --- Interest waterfall: senior → junior at each tranche's coupon. ---
		availInt := netInterest
		for i := range d.Tranches {
			due := balances[i] * d.Tranches[i].Coupon / 12
			pay := due
			if pay > availInt {
				pay = availInt
			}
			availInt -= pay
			tcf[i].Interest = append(tcf[i].Interest, pay)
		}
		residual := availInt // excess interest to the equity holder

		// --- Loss allocation: junior → senior write-down. ---
		remainingLoss := loss
		for i := len(d.Tranches) - 1; i >= 0; i-- {
			wd := remainingLoss
			if wd > balances[i] {
				wd = balances[i]
			}
			balances[i] -= wd
			remainingLoss -= wd
			tcf[i].Writedown = append(tcf[i].Writedown, wd)
		}

		// --- Principal waterfall: senior → junior, sequential. ---
		availPrin := principalCollected
		for i := range d.Tranches {
			pay := balances[i]
			if pay > availPrin {
				pay = availPrin
			}
			balances[i] -= pay
			availPrin -= pay
			tcf[i].Principal = append(tcf[i].Principal, pay)
			tcf[i].Balance = append(tcf[i].Balance, balances[i])
		}
		residual += availPrin // principal after every tranche retired

		proj.Pool = append(proj.Pool, PeriodCashflow{
			Month: m, BeginBalance: bal,
			Interest: netInterest, Principal: principalCollected, Loss: loss,
		})
		proj.Residual = append(proj.Residual, residual)

		bal = bal - schedPrin - prepay - defaulted
	}
	proj.Tranches = tcf
	return proj
}

// WAL is the weighted-average life (years) of a tranche's principal stream.
func (t TrancheCashflow) WAL() float64 {
	var weighted, total float64
	for m, p := range t.Principal {
		years := float64(m+1) / 12
		weighted += years * p
		total += p
	}
	if total == 0 {
		return 0
	}
	return weighted / total
}

// levelPayment is the constant monthly payment fully amortizing balance over n
// months at monthly rate r (the mortgage formula). r==0 ⇒ straight-line.
func levelPayment(balance, r float64, n int) float64 {
	if n <= 0 {
		return balance
	}
	if r == 0 {
		return balance / float64(n)
	}
	f := pow(1+r, n)
	return balance * r * f / (f - 1)
}

func pow(x float64, n int) float64 {
	out := 1.0
	for i := 0; i < n; i++ {
		out *= x
	}
	return out
}
