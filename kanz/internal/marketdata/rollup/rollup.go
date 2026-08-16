// Package rollup derives the coarse OHLCV series (1h, 1d) from the stored
// 1-minute base series.
//
// # Why it exists
//
// marketdata.TranslateBar refuses any candle coarser than 1m from a venue —
// "only 1m is INGESTED, and 1h/1d are derived from it by rollup" — and the store
// accepts all three precisely so that a rollup has somewhere to write. Nothing
// wrote them until this package: Resolution1h had zero producers, so every
// consumer that wanted an hourly or daily series read minutes instead. Over a
// 28-day window that is ~40,320 rows per instrument per call, and
// internal/risk/compute walks the book twice per risk request. The same window at
// 1d is 28 rows.
//
// # Two halves, deliberately separated
//
// Fold is pure: constituents in, one coarse bar out, no clock and no store. It is
// where every aggregation decision lives and it is testable without either.
// Roller is the driver: it decides WHICH buckets are eligible, reads, folds and
// writes. The split exists because the hard questions divide the same way — what
// a coarse bar's numbers ARE is arithmetic; whether a bucket may be rolled up at
// all is a question about time and coverage that no pure function can answer.
//
// # A derived bar is stamped with when its INPUTS were known, not when it was run
//
// This is the decision the rest of the package hangs on, and it is stated once
// here: KnowledgeTime is the MAXIMUM KnowledgeTime of the constituents. See the
// block on Fold for what each of the two obvious alternatives costs. The property
// that makes everything else work is that the derived bar is then a PURE FUNCTION
// of its inputs — same minutes in, byte-identical bar out, forever — so a re-run
// collides with the row it wrote last time and PutBars is a genuine no-op.
package rollup

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/marketdata/store"
)

// ErrCannotFold rejects a set of constituents that does not describe one whole
// coarse bucket of one series.
//
// EVERY REFUSAL HERE IS A REFUSAL TO GUESS. The alternative to each one is a bar
// that is well-formed, indistinguishable from a correct bar, and wrong — and a
// coarse bar is cached forever, because the store is append-only by (bucket,
// knowledge_time) and nothing revisits it unless a human re-runs the rollup.
var ErrCannotFold = errors.New("rollup: cannot fold")

// Fold aggregates the 1-minute constituents of ONE coarse bucket into one coarse
// bar.
//
// # The constituents are ordered by BucketStart, never by slice order
//
// Open is the FIRST constituent's open and Close is the LAST one's close, where
// first and last mean earliest and latest BucketStart. Taking them from the slice
// as given would make the result depend on the order the store, a map iteration
// or a caller's append loop happened to produce — and the two prices that would
// be wrong are the two every derived number is computed from: a return is
// close/close, an ADV is volume against a close, and every indicator in
// internal/marketdata/indicator reads closes. A wrong close does not look wrong;
// it looks like the market moved.
//
// A DUPLICATE BucketStart IS REFUSED rather than resolved. Two bars for the same
// minute mean the caller already collapsed the knowledge axis incorrectly (the
// store's own read collapses each bucket to one version), and picking either one
// would make the fold's output depend on which — the exact non-determinism the
// sort exists to remove.
//
// # Every constituent is re-validated, and that is load-bearing
//
// dec.Cmp treats a nil Decimal as ZERO. A constituent with a nil Low would
// therefore win the Low comparison against any positive price, and the hour would
// print a low of zero — a bar that then fails nothing downstream because it is
// internally consistent. Validate refuses the nil before it can be compared.
//
// # KnowledgeTime is max(constituents), and both alternatives are worse
//
//   - THE RUN TIME (what backfill correctly uses for venue history) would claim
//     the platform learned this hour later than it learned every minute in it.
//     Two costs: a point-in-time read as of an instant when all the inputs were
//     already known would not see the coarse bar, so the 1m and 1h series would
//     disagree about the same market at the same horizon; and the stamp would
//     change on every run, so a re-run would write a fresh version of every
//     bucket it touched — a table that doubles per invocation and a restatement
//     history full of entries restating nothing.
//   - THE BUCKET'S OWN END would assert the platform knew the hour as it closed,
//     which is exactly the claim #427 refused on the ingest path. Backfilled
//     minutes are learned months after they happened; a derived bar stamped with
//     the bucket end would leak them backwards into every backtest as of a date
//     before the backfill ran.
//
// max(constituents) is the earliest instant at which THIS EXACT BAR was
// derivable. It cannot claim to be known before its inputs, and it cannot hide a
// correction: a restated minute carries a later KnowledgeTime, so the bar derived
// from it carries one too and lands beside the old version rather than over it.
//
// # What Fold does NOT know
//
// It does not know whether it was given a WHOLE bucket. Nothing in a slice of
// bars can say whether the missing 12:37 was a quiet minute or a gap in
// ingestion, so completeness is the driver's decision — see Roller.Run. Fold
// checks only that everything it was given belongs to the bucket it was told.
func Fold(target store.Resolution, bucketStart time.Time, constituents []store.Bar) (store.Bar, error) {
	interval, ok := target.Interval()
	if !ok {
		return store.Bar{}, fmt.Errorf("%w: %q is not a resolution this platform stores",
			ErrCannotFold, target)
	}
	base, _ := store.Resolution1m.Interval()
	if interval <= base {
		return store.Bar{}, fmt.Errorf("%w: %q is not COARSER than the 1m base series — one minute "+
			"is what is ingested, and a rollup that produced it would give one candle two sources",
			ErrCannotFold, target)
	}
	if bucketStart.IsZero() {
		return store.Bar{}, fmt.Errorf("%w: bucket_start is required", ErrCannotFold)
	}
	// UTC-ALIGNED BOUNDARIES, AND 1d MEANS A UTC DAY. A venue whose session day
	// starts elsewhere would need a different bucketer; inventing one silently here
	// would shift every daily return by the offset and the series would still look
	// perfectly well-formed.
	start := bucketStart.UTC()
	if !start.Equal(start.Truncate(interval)) {
		return store.Bar{}, fmt.Errorf("%w: bucket_start %s is not aligned to a %s boundary, so the "+
			"bar would cover an interval no other bar in the series covers", ErrCannotFold, start, target)
	}
	end := start.Add(interval)
	if len(constituents) == 0 {
		return store.Bar{}, fmt.Errorf("%w: bucket %s has no constituents — an OHLC bar with no "+
			"prices in it cannot be constructed, only invented", ErrCannotFold, start)
	}

	// COPIED BEFORE SORTING. Reordering a caller's slice in place is a side effect
	// on a function documented as pure, and the caller's next read of the same
	// slice would silently see a different order.
	ordered := make([]store.Bar, len(constituents))
	copy(ordered, constituents)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].BucketStart.Before(ordered[j].BucketStart)
	})

	first, last := ordered[0], ordered[len(ordered)-1]
	out := store.Bar{
		InstrumentID: first.InstrumentID,
		Venue:        first.Venue,
		Resolution:   target,
		BucketStart:  start,
		Open:         first.Open,
		High:         first.High,
		Low:          first.Low,
		Close:        last.Close,
	}

	volume := &commonpb.Decimal{}
	var trades int64
	everyMinuteReported := true
	var known, prevStart time.Time

	for i, m := range ordered {
		if err := m.Validate(); err != nil {
			return store.Bar{}, fmt.Errorf("%w: constituent %s: %w", ErrCannotFold, m.BucketStart, err)
		}
		if m.Resolution != store.Resolution1m {
			return store.Bar{}, fmt.Errorf("%w: constituent %s is a %s bar — the coarse series is "+
				"derived from the 1m base only, and folding a rollup of a rollup would double-count "+
				"any minute present in both", ErrCannotFold, m.BucketStart, m.Resolution)
		}
		// VENUE AND INSTRUMENT ARE PART OF A BAR'S IDENTITY, so a mixed slice is
		// refused rather than merged. store.Bar's own doc states the cost: a model
		// trained on a composite candle and executed against one venue's book shows
		// a divergence that reads as "the strategy stopped working" rather than as a
		// data defect. This platform has no composite series and this is not the
		// place to grow one.
		if m.InstrumentID != first.InstrumentID || m.Venue != first.Venue {
			return store.Bar{}, fmt.Errorf("%w: constituents span %s/%s and %s/%s — bars are per "+
				"instrument per venue, and merging them would build a composite candle no order can "+
				"execute against", ErrCannotFold,
				first.InstrumentID, first.Venue, m.InstrumentID, m.Venue)
		}
		if m.BucketStart.Before(start) || !m.BucketStart.Before(end) {
			return store.Bar{}, fmt.Errorf("%w: constituent %s is outside bucket [%s, %s)",
				ErrCannotFold, m.BucketStart, start, end)
		}
		if i > 0 && m.BucketStart.Equal(prevStart) {
			return store.Bar{}, fmt.Errorf("%w: minute %s appears twice — first and last are then "+
				"ambiguous, and the fold's open and close would depend on which copy sorted first",
				ErrCannotFold, m.BucketStart)
		}
		prevStart = m.BucketStart

		if dec.Cmp(m.High, out.High) > 0 {
			out.High = m.High
		}
		if dec.Cmp(m.Low, out.Low) < 0 {
			out.Low = m.Low
		}
		// SUMMED IN THE DECIMAL DOMAIN, exactly. dec.Add computes in math/big and
		// refuses rather than wrapping, which is the whole reason it exists — a
		// volume folded through a float or a raw int64 coefficient is a number the
		// ADV and the participation-rate check are then computed from.
		v, ok := dec.Add(volume, m.Volume)
		if !ok {
			return store.Bar{}, fmt.Errorf("%w: volume sum at %s is not representable",
				ErrCannotFold, m.BucketStart)
		}
		volume = v

		if m.TradeCount == nil {
			everyMinuteReported = false
		} else {
			if trades > math.MaxInt64-*m.TradeCount {
				return store.Bar{}, fmt.Errorf("%w: trade count sum overflows int64 at %s",
					ErrCannotFold, m.BucketStart)
			}
			trades += *m.TradeCount
		}

		if m.KnowledgeTime.After(known) {
			known = m.KnowledgeTime
		}
	}

	out.Volume = volume
	out.KnowledgeTime = known.UTC()

	// THE TRADE COUNT IS ALL-OR-NOTHING (#432).
	//
	// The question is what the trade count of an hour is when some of its minutes
	// carry one and some are nil. Both plausible answers lose something and only
	// one of them loses it visibly:
	//
	//   - SUMMING, TREATING nil AS 0, invents the claim that the unreported minutes
	//     saw no trades. store.Bar.TradeCount exists precisely because that
	//     conflation was shipped once: every OKX candle was stored as a dead minute
	//     because the venue's row has no count field at all. The result is a number
	//     that is smaller than the truth by an unknown amount and is
	//     INDISTINGUISHABLE from a correct one — it will be averaged, differenced
	//     and used as a liquidity proxy, and nothing downstream can tell.
	//
	//   - NIL IF ANY MINUTE IS UNREPORTED discards real information: an hour with
	//     fifty-nine counted minutes and one unreported one reports no count at all.
	//
	// This takes the second, because a nil cannot be averaged by accident and a
	// wrong number can. WHAT IT COSTS, stated so it is not discovered: a single
	// backfilled minute inside an otherwise live day (the seam where the two
	// sources meet) nils out the whole day's count. In practice a venue either
	// reports counts or does not, so the mixed case is the JOIN between a live fold
	// and a backfill rather than a routine occurrence — which is also exactly the
	// case where a partial sum would be most misleading, since the missing minutes
	// are contiguous rather than scattered.
	if everyMinuteReported {
		count := trades
		out.TradeCount = &count
	}

	// The fold's own arithmetic cannot produce an inconsistent candle from
	// consistent inputs, so this catches the input-shaped failures instead: a
	// volume sum that left the representable decimal domain, or a constituent set
	// whose identity fields were empty.
	if err := out.Validate(); err != nil {
		return store.Bar{}, fmt.Errorf("%w: folded bar for %s: %w", ErrCannotFold, start, err)
	}
	return out, nil
}
