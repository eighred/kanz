package collateral

import "sort"

// COLL-01c — collateral optimization: the cheapest-to-deliver allocation of a
// pool of collateral assets across margin requirements, under each agreement's
// eligibility schedule and haircuts, minimizing the total opportunity cost of
// posting. It is an OPT-01b-style allocation, but where OPT-01 solves a
// quadratic portfolio, this is a linear transportation problem with a separable
// per-asset cost — so the cheapest-first greedy is cost-OPTIMAL for the
// fractional (divisible-collateral) case, no QP needed (the "small, dependency-
// free" stance).

// Asset is a postable collateral asset and its opportunity cost.
type Asset struct {
	// ID is the canonical asset id (a cash currency, a bond, an equity).
	ID string
	// Available is the market value of the asset available to post.
	Available float64
	// Cost is the opportunity cost per unit of MARKET value posted (e.g. the
	// asset's funding spread or convenience yield) — cheaper assets are delivered
	// first.
	Cost float64
}

// Eligibility is one asset's terms under one agreement.
type Eligibility struct {
	Eligible bool
	// Haircut is the valuation haircut; post-value = market-value × (1 − haircut).
	Haircut float64
}

// Requirement is one agreement's collateral need plus its eligibility schedule.
type Requirement struct {
	AgreementID string
	// Amount is the required POST-value (haircut-adjusted) to satisfy the call.
	Amount float64
	// Schedule maps asset id → its eligibility/haircut under this agreement.
	Schedule map[string]Eligibility
}

// Allocation is one posting decision: UsedValue of the asset's market value is
// posted to the agreement, contributing PostedValue of haircut-adjusted value.
type Allocation struct {
	AgreementID string
	AssetID     string
	UsedValue   float64 // market value consumed
	PostedValue float64 // haircut-adjusted value delivered
	Cost        float64 // opportunity cost incurred (UsedValue × asset cost)
}

// option is one (requirement, asset) candidate posting, with its effective cost
// per unit of post-value (the cheapest-to-deliver ranking key).
type option struct {
	reqIdx     int
	asset      *Asset
	haircut    float64
	effective  float64 // cost per unit of post-value = Cost / (1 − haircut)
	postPerVal float64 // post-value per unit of market value = (1 − haircut)
}

// Optimize allocates the asset pool across the requirements cheapest-to-deliver.
// It returns the allocations and ok=true when every requirement is fully met;
// ok=false (with the partial allocations) when the eligible pool is insufficient.
// Cost is minimized: every (agreement, asset) option is ranked by its cost per
// unit of delivered post-value and filled in that order, subject to per-asset
// availability and per-requirement need.
func Optimize(assets []Asset, requirements []Requirement) ([]Allocation, bool) {
	// Mutable copies of the availabilities and remaining needs.
	pool := make([]Asset, len(assets))
	copy(pool, assets)
	byID := make(map[string]*Asset, len(pool))
	for i := range pool {
		byID[pool[i].ID] = &pool[i]
	}
	remaining := make([]float64, len(requirements))
	for i, r := range requirements {
		remaining[i] = r.Amount
	}

	// Build the eligible options, cheapest effective cost first.
	var opts []option
	for ri, r := range requirements {
		for id, e := range r.Schedule {
			if !e.Eligible || e.Haircut >= 1 {
				continue
			}
			a, ok := byID[id]
			if !ok {
				continue
			}
			postPerVal := 1 - e.Haircut
			opts = append(opts, option{
				reqIdx:     ri,
				asset:      a,
				haircut:    e.Haircut,
				effective:  a.Cost / postPerVal,
				postPerVal: postPerVal,
			})
		}
	}
	sort.SliceStable(opts, func(i, j int) bool {
		if opts[i].effective != opts[j].effective {
			return opts[i].effective < opts[j].effective
		}
		return opts[i].asset.ID < opts[j].asset.ID // deterministic tie-break
	})

	var allocations []Allocation
	for _, o := range opts {
		need := remaining[o.reqIdx]
		if need <= 0 || o.asset.Available <= 0 {
			continue
		}
		// Post-value the asset can still deliver vs what the requirement needs.
		maxPost := o.asset.Available * o.postPerVal
		post := need
		if maxPost < post {
			post = maxPost
		}
		used := post / o.postPerVal
		o.asset.Available -= used
		remaining[o.reqIdx] -= post
		allocations = append(allocations, Allocation{
			AgreementID: requirements[o.reqIdx].AgreementID,
			AssetID:     o.asset.ID,
			UsedValue:   used,
			PostedValue: post,
			Cost:        used * o.asset.Cost,
		})
	}

	ok := true
	for _, rem := range remaining {
		if rem > 1e-9 {
			ok = false
			break
		}
	}
	return allocations, ok
}

// TotalCost sums the opportunity cost of an allocation set — the objective
// Optimize minimizes.
func TotalCost(allocations []Allocation) float64 {
	var c float64
	for _, a := range allocations {
		c += a.Cost
	}
	return c
}
