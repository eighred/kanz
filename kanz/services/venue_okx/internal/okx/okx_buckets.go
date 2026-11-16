package okx

import "time"

// okxBuckets is one rate-limit bucket per OKX RATE-LIMIT FAMILY (#933).
//
// # The modelling error this replaces
//
// execution.WeightBucket is, in its own words, "a Binance-weight token bucket":
// Binance meters every REST call by a per-endpoint WEIGHT against ONE per-minute
// budget, so a single bucket with per-call weights is a faithful model of it. OKX
// does not work that way — its limits are PER ENDPOINT, and two endpoints are two
// independent remote budgets that refill independently and cannot starve each
// other. The OKX connector borrowed Binance's shape, and a single bucket sized
// from /trade/order's 60-per-2s limit then metered every other endpoint too.
//
// That was harmless while every call cost 1. #923 made it load-bearing by adding
// GET /api/v5/trade/fills-history to the ORDER PATH, correctly priced at 6 to
// reflect that endpoint's real 10-per-2s limit against a bucket sized for 60. The
// arithmetic per order that actually trades went from place(1) + query(1) = 2 to
// place(1) + query(1) + fills(6) = 8 against a budget of 60 — so the adapter
// self-throttled at ~7.5 traded orders per 2s where OKX itself would have
// permitted 60 placements AND 10 fills-history calls in the same window,
// concurrently, out of separate remote budgets.
//
// The 6 was never the defect and lowering it would have been the wrong fix: it is
// a faithful model of the remote limit, and undercharging would let the recovery
// path spend a budget it does not have and collect 50011s on the placement path —
// the one path that must never be starved. The defect is that two independent
// remote budgets were drawn from one local bucket.
//
// # Why only fills-history is split out, and why that is not half a fix
//
// THE ONLY NUMBERS HERE ARE ONES THIS REPOSITORY ALREADY HAD. /trade/order's
// 60-per-2s and fills-history's 10-per-2s are both stated in okx_rest.go, the
// second as the basis for the weight of 6. Every other endpoint's real OKX limit
// is NOT established anywhere in this tree, and #933 says so explicitly:
// confirming which endpoints share a limit group is read-only API documentation
// work that has not been done.
//
// So the remaining endpoints keep sharing one bucket, at exactly the budget and
// window they have today. That is deliberate and it is the conservative
// direction: giving each of them its own bucket would RAISE the aggregate this
// connector is willing to send from 60-per-2s to N times that, against remote
// limits nobody has verified — which is how a connector starts collecting 50011
// rate-limit refusals on the capital path. Under-using a budget costs throughput;
// over-using one costs orders. An unverified limit is treated as unknown, and an
// unknown is not rounded up.
//
// Adding a verified family is one entry in okxRateFamily plus one line in
// bucketFor. The guard in okx_buckets_test.go fails if an endpoint is added to
// the REST client without naming a family.
type okxBuckets struct {
	// shared meters every family whose real OKX limit is unverified. It is sized
	// exactly as the single bucket was before this change, so those endpoints'
	// behaviour is bit-for-bit what it was.
	shared *WeightBucket
	// fillsHistory meters GET /api/v5/trade/fills-history against ITS OWN
	// documented 10-per-2s limit. Calls against it cost 1, not 6: the 6 existed
	// only to express a 10/2s endpoint in units of a 60/2s bucket, and with a
	// bucket that is actually 10/2s the translation is the identity.
	fillsHistory *WeightBucket
}

// okxRateFamily names an OKX rate-limit budget. Every REST call declares one.
type okxRateFamily int

const (
	// familyUnverified is every endpoint whose real per-endpoint limit this
	// repository has not established. They share one bucket at /trade/order's
	// budget, which is what they shared before #933 — see the type comment for
	// why splitting them on unverified numbers would be the unsafe direction.
	familyUnverified okxRateFamily = iota
	// familyFillsHistory is GET /api/v5/trade/fills-history: 10 requests per 2
	// seconds, stated in okx_rest.go, and the reason a fills-history call used to cost 6.
	familyFillsHistory
)

// okxFillsHistoryPerWindow and okxFillsHistoryWindow are OKX's documented limit
// for GET /api/v5/trade/fills-history.
//
// THIS IS THE ONE NEW NUMBER IN THIS FILE and it is not new information: it is
// the limit okx_rest.go already named when it justified charging that endpoint 6
// units of a 60-unit bucket ("fills-history's own limit is 10 per 2 seconds — six
// times tighter"). Expressing it as a bucket rather than as a ratio is the whole
// change.
const (
	okxFillsHistoryPerWindow = 10
	okxFillsHistoryWindow    = 2 * time.Second
)

// newOKXBuckets builds the per-family buckets. shared is the bucket the
// composition root already sized from VenueSettings.WeightBudget, passed in
// rather than rebuilt so the configured budget still means what it meant.
//
// now is the injectable clock, threaded so a test can MEASURE a sustained rate
// rather than sleep through one — #933 asks for a measured ceiling, and a
// measurement that depends on wall-clock timing is a flake waiting to happen.
func newOKXBuckets(shared *WeightBucket, now func() time.Time) *okxBuckets {
	if shared == nil {
		// A nil shared bucket would make Allow panic on the first call, on the
		// order path, at runtime. The connector is unusable without a budget, so
		// this fails at construction with a real one rather than later with a
		// nil dereference.
		shared = NewWeightBucket(okxDefaultWeightBudget, okxDefaultWeightWindow, now)
	}
	return &okxBuckets{
		shared:       shared,
		fillsHistory: NewWeightBucket(okxFillsHistoryPerWindow, okxFillsHistoryWindow, now),
	}
}

// bucketFor returns the bucket a family draws from.
//
// AN UNKNOWN FAMILY FALLS TO shared, NOT TO A FRESH BUCKET. A fresh bucket would
// hand an unrecognised endpoint a brand-new full budget, which is the generous
// direction on a value nobody declared — the opposite of what an unknown should
// do. Falling to shared makes an unnamed endpoint behave exactly as it did before
// any of this existed.
func (b *okxBuckets) bucketFor(f okxRateFamily) *WeightBucket {
	if f == familyFillsHistory {
		return b.fillsHistory
	}
	return b.shared
}

// allow consumes weight from the family's bucket, reporting whether the call may
// proceed. It is the single seam every REST method meters through.
func (b *okxBuckets) allow(f okxRateFamily, weight int) bool {
	return b.bucketFor(f).Allow(weight)
}
