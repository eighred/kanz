package execution

import (
	"math"
	"sort"
	"sync"
	"time"
)

// VENUE RANKING, ON MEASURED COST (#437 option B).
//
// #437's option A made an untargeted order's destination a NAMED choice instead
// of an array index. This is option B: choosing it by what each venue has
// actually cost, which the issue deliberately sequenced after #436 —
//
//	B should wait for #436 so the ranking is measured rather than assumed — a
//	router that ranks on a model nobody has validated is a more confident
//	version of the same problem.
//
// # It is a PREFERENCE, never a permission
//
// Ranking reorders venues that have ALREADY passed every correctness check —
// the MIC match, the exchange-account match that prevents cross-collateralization,
// and the order-type support gate. It cannot admit a venue those refuse, and it
// cannot refuse one they allow. The worst it can do is prefer the wrong one of
// two acceptable venues, which is the same class of wrong the configured default
// already is, and strictly better informed.
//
// # And it fails back to the stated choice, never to an array index
//
// No data, too little data, stale data, or a tie ⇒ the ranker abstains and the
// DECLARED default stands. That ordering matters: #437 exists because a
// destination chosen by slice position looked like a decision. An empty ranker
// must degrade to the operator's choice, not to venues[0].
//
// # Why the fold is in-process rather than over the bus
//
// costwatch computes these measurements already, per fill, inside the OMS. It
// hands them here directly. The alternative — publishing order.cost.recorded and
// subscribing to it in the same service — would put a broker round trip inside a
// feedback loop and make the OMS a consumer of its own FACT, which is the
// coupling #475's publish-only grant deliberately avoids.
//
// THE HONEST COST OF THAT CHOICE: with several OMS replicas each ranks on the
// fills it happened to see. For a preference between two already-acceptable
// venues that is acceptable and it is stated rather than hidden — a replica with
// thin evidence abstains via the minimum-sample rule below rather than acting on
// three fills.

// venueStat is one venue's running cost, in basis points of arrival notional.
type venueStat struct {
	// sum is Σ(shortfall bps × notional) and weight is Σ notional, so the mean
	// is NOTIONAL-WEIGHTED (#483).
	//
	// AN UNWEIGHTED MEAN WAS WRONG IN TWO WAYS AT ONCE. A 0.001 BTC fill counted
	// as much as a 100 BTC one, so a venue's "cost" was dominated by however many
	// small fills it happened to produce rather than by the money that went
	// through it. And once #435 let one order be worked in fifty slices, fifty
	// small fills arrived where one large fill used to — which under an
	// unweighted mean is fifty votes for one decision.
	//
	// Weighting fixes both with one change: a parent split into fifty slices
	// contributes EXACTLY what the same order sent whole would have contributed,
	// because the notional is the same either way.
	sum    float64
	weight float64

	// n counts fills, for the summary only. It is deliberately NOT the evidence
	// floor — see decisions.
	n int

	// decisions is the set of distinct DECISIONS this venue has been measured on,
	// where a decision is a parent order if the fill belonged to a slice and the
	// order itself otherwise.
	//
	// IT IS THE EVIDENCE FLOOR, because fifty slices of one order are one piece
	// of evidence about a venue, not fifty. Counting fills let a venue worked
	// with an algorithm cross the floor fifty times faster — and the ranker then
	// feeds back into where the next order is sent, so the bias compounds.
	//
	// BOUNDED BY minSamples, which is what makes holding a set affordable: once
	// a venue has that many distinct decisions the floor is crossed and no
	// further id can change the answer, so nothing more is stored. The whole stat
	// resets when it ages out of the window.
	decisions map[string]struct{}

	last  time.Time
	first time.Time
}

// VenueCosts folds per-fill cost measurements into a per-venue preference.
//
// Goroutine-safe: costwatch folds from a consumer goroutine while the Router
// reads from the admission path.
type VenueCosts struct {
	mu    sync.RWMutex
	stats map[string]*venueStat

	now        func() time.Time
	maxAge     time.Duration
	minSamples int
}

// VenueCostsOption customizes a VenueCosts.
type VenueCostsOption func(*VenueCosts)

// WithCostClock injects the clock (tests).
func WithCostClock(now func() time.Time) VenueCostsOption {
	return func(v *VenueCosts) {
		if now != nil {
			v.now = now
		}
	}
}

// WithCostWindow bounds how old a venue's most recent measurement may be and
// still count.
func WithCostWindow(d time.Duration) VenueCostsOption {
	return func(v *VenueCosts) { v.maxAge = d }
}

// WithMinSamples sets how many distinct DECISIONS a venue needs before it may be
// preferred (#483) — not how many fills.
func WithMinSamples(n int) VenueCostsOption {
	return func(v *VenueCosts) { v.minSamples = n }
}

// Defaults for the two rules that decide whether the ranker may speak at all.
const (
	// DefaultCostWindow bounds staleness. A venue's cost is a property of the
	// market and of that venue's queue TODAY; a week-old average would keep
	// preferring a venue that has since degraded, and would do it confidently.
	DefaultCostWindow = 6 * time.Hour

	// DefaultMinSamples is how much evidence a venue needs before the ranker
	// will act on it. THIS IS THE RULE THAT STOPS THE RANKER BEING WORSE THAN
	// THE CONFIG LINE. Execution cost is noisy; three fills can rank a good
	// venue below a bad one purely on which happened to catch a spread. Below
	// this the ranker abstains and the operator's declared default stands.
	//
	// IT COUNTS DISTINCT DECISIONS, NOT FILLS (#483). One order worked in fifty
	// slices is one observation of this venue under one set of market
	// conditions; counting it as fifty would clear this floor from a single
	// order, which is the opposite of what the floor is for.
	DefaultMinSamples = 20
)

// NewVenueCosts returns an empty fold.
func NewVenueCosts(opts ...VenueCostsOption) *VenueCosts {
	v := &VenueCosts{
		stats:      map[string]*venueStat{},
		now:        time.Now,
		maxAge:     DefaultCostWindow,
		minSamples: DefaultMinSamples,
	}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

// Observe records one fill's realized shortfall, in basis points including fees,
// weighted by the notional it was measured over and attributed to the DECISION
// it belonged to.
//
// bps and notional are float64 and that is deliberate: this is a STATISTIC used
// to order two venues, not money. The measurement itself is exact (*big.Rat, in
// internal/execution/tca) and stays exact on the FACT and in the ledger. Nothing
// here is added to a balance.
//
// decision is the parent order id when this fill belonged to a slice of a worked
// order, and the order's own id otherwise (#435, #483). It decides how much
// EVIDENCE this venue has, which is a different question from how much its
// measurements weigh — see venueStat.
//
// A NON-POSITIVE NOTIONAL IS DROPPED, NOT COUNTED AS ZERO WEIGHT. A fill nobody
// could size cannot be weighed against one that could, and letting it through
// with zero weight would add it to the evidence floor while contributing nothing
// to the mean — evidence for a number it had no part in.
func (v *VenueCosts) Observe(venue string, bps, notional float64, decision string) {
	if venue == "" || math.IsNaN(bps) || math.IsInf(bps, 0) {
		return
	}
	if notional <= 0 || math.IsNaN(notional) || math.IsInf(notional, 0) {
		return
	}
	now := v.now().UTC()
	v.mu.Lock()
	defer v.mu.Unlock()
	s := v.stats[venue]
	if s == nil {
		s = &venueStat{first: now, decisions: map[string]struct{}{}}
		v.stats[venue] = s
	}
	s.sum += bps * notional
	s.weight += notional
	s.n++
	// BOUNDED: once the floor is crossed no further id can change the verdict, so
	// nothing more is stored. An empty decision id is not counted as evidence —
	// it is an unattributable measurement, and treating "" as one decision would
	// make every such fill collapse into a single permanent sample.
	if decision != "" && len(s.decisions) < v.minSamples {
		s.decisions[decision] = struct{}{}
	}
	s.last = now
}

// mean is the notional-weighted cost, and ok=false when nothing weighable has
// been seen. Callers hold the lock.
func (s *venueStat) mean() (float64, bool) {
	if s.weight <= 0 {
		return 0, false
	}
	return s.sum / s.weight, true
}

// Preferred returns the cheapest venue among the candidates, and whether the
// ranker had enough current evidence to answer at all.
//
// ok=false is the ABSTAIN, and it is the common case early on. It means "the
// caller should use its declared default", not "no venue is acceptable".
func (v *VenueCosts) Preferred(candidates []string) (string, bool) {
	if len(candidates) < 2 {
		// Nothing to choose between. Answering here would let a single-venue
		// deployment look ranked when it is not.
		return "", false
	}
	now := v.now().UTC()
	v.mu.RLock()
	defer v.mu.RUnlock()

	type scored struct {
		mic  string
		mean float64
	}
	var eligible []scored
	for _, mic := range candidates {
		s := v.stats[mic]
		// THE FLOOR COUNTS DECISIONS, NOT FILLS (#483). Fifty slices of one order
		// are one piece of evidence about a venue; counting them as fifty let a
		// venue worked with an algorithm start being preferred fifty times sooner
		// than one worked whole, and the preference then decides where the next
		// order goes.
		if s == nil || len(s.decisions) < v.minSamples {
			continue
		}
		if v.maxAge > 0 && now.Sub(s.last) > v.maxAge {
			continue
		}
		m, ok := s.mean()
		if !ok {
			continue
		}
		eligible = append(eligible, scored{mic: mic, mean: m})
	}
	// EVERY CANDIDATE MUST BE MEASURED, OR NONE IS PREFERRED. Ranking a venue
	// with 200 fills against one with none does not compare them — it prefers
	// whichever the platform happens to have used, which is how a default
	// entrenches itself and stops being questioned.
	if len(eligible) != len(candidates) {
		return "", false
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].mean != eligible[j].mean {
			return eligible[i].mean < eligible[j].mean // lower cost is better
		}
		return eligible[i].mic < eligible[j].mic // deterministic on a tie
	})
	// A TIE IS AN ABSTENTION. Two venues with the same measured cost give the
	// ranker no reason to override the operator, and picking alphabetically
	// would be an array index by another name.
	if len(eligible) > 1 && eligible[0].mean == eligible[1].mean {
		return "", false
	}
	return eligible[0].mic, true
}

// Snapshot returns each venue's mean cost and sample count, for the startup log
// and the posture metric. Sorted for a stable rendering.
func (v *VenueCosts) Snapshot() []VenueCostSummary {
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := make([]VenueCostSummary, 0, len(v.stats))
	for mic, s := range v.stats {
		m, ok := s.mean()
		if !ok {
			continue
		}
		out = append(out, VenueCostSummary{
			Venue: mic, MeanBps: m, Samples: s.n, Decisions: len(s.decisions),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Venue < out[j].Venue })
	return out
}

// VenueCostSummary is one venue's measured cost.
type VenueCostSummary struct {
	Venue   string
	MeanBps float64
	// Samples is fills; Decisions is the distinct orders those fills belonged to,
	// counting a worked parent once however many slices it took.
	//
	// BOTH ARE REPORTED BECAUSE THE GAP BETWEEN THEM IS THE INTERESTING NUMBER:
	// Samples far above Decisions means this venue's evidence comes from a few
	// orders worked in many slices, which is exactly when a fill-counted floor
	// would have been fooled. Decisions is the one the ranker acts on, and it
	// stops counting at the minimum-sample floor.
	Samples   int
	Decisions int
}
