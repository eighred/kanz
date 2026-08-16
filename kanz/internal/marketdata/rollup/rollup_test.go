package rollup

import (
	"errors"
	"math/rand"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/marketdata/store"
)

var (
	// hour0 is a 1h bucket boundary; day0 is the UTC day containing it.
	hour0 = time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	day0  = time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC)
	// learned is a plausible knowledge time for a live-folded minute.
	learned = time.Date(2026, 3, 4, 12, 1, 0, 0, time.UTC)
)

func d(c int64, e int32) *commonpb.Decimal { return &commonpb.Decimal{Coefficient: c, Exponent: e} }

func ptrTo(v int64) *int64 { return &v }

// minute builds a 1m constituent. Every price is passed explicitly: a fixture
// whose OHLC are all the same number cannot show which one the fold picked.
type minuteSpec struct {
	at                     int // minutes past the bucket start
	open, high, low, close int64
	volume                 int64
	trades                 *int64
	knownAt                time.Time
}

func minute(bucket time.Time, s minuteSpec) store.Bar {
	known := s.knownAt
	if known.IsZero() {
		known = learned
	}
	return store.Bar{
		InstrumentID:  "BTC-USDT",
		Venue:         "XBIN",
		Resolution:    store.Resolution1m,
		BucketStart:   bucket.Add(time.Duration(s.at) * time.Minute),
		Open:          d(s.open, -2),
		High:          d(s.high, -2),
		Low:           d(s.low, -2),
		Close:         d(s.close, -2),
		Volume:        d(s.volume, -1),
		TradeCount:    s.trades,
		KnowledgeTime: known,
	}
}

// threeMinutes is the fixture the OHLC assertions rest on, and every number in it
// is chosen so that a WRONG PICK IS VISIBLE:
//
//   - the HIGH (200.00) is in the MIDDLE bar, so taking it from the first or the
//     last constituent gives a different answer;
//   - the LOW (50.00) is in the LAST bar, so taking it from the first does too;
//   - open (100.00) and close (150.00) differ from each other and from both
//     extremes, so no two of the four can be confused.
func threeMinutes() []store.Bar {
	return []store.Bar{
		minute(hour0, minuteSpec{at: 0, open: 10000, high: 12000, low: 9500, close: 11000, volume: 10, trades: ptrTo(3)}),
		minute(hour0, minuteSpec{at: 1, open: 11000, high: 20000, low: 10500, close: 19000, volume: 20, trades: ptrTo(5)}),
		minute(hour0, minuteSpec{at: 2, open: 19000, high: 19500, low: 5000, close: 15000, volume: 30, trades: ptrTo(7)}),
	}
}

func mustFold(t *testing.T, target store.Resolution, start time.Time, mins []store.Bar) store.Bar {
	t.Helper()
	b, err := Fold(target, start, mins)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	return b
}

func wantDecimal(t *testing.T, field string, got *commonpb.Decimal, coeff int64, exp int32, why string) {
	t.Helper()
	if got.GetCoefficient() != coeff || got.GetExponent() != exp {
		t.Errorf("%s = {%d, %d}, want {%d, %d}. %s",
			field, got.GetCoefficient(), got.GetExponent(), coeff, exp, why)
	}
}

// OPEN AND CLOSE COME FROM THE EARLIEST AND LATEST MINUTE, AND HIGH AND LOW FROM
// THE WHOLE BUCKET.
//
// The fixture puts the high in the middle bar and the low in the last, so a fold
// that read the extremes off the first or last constituent returns a different
// number rather than the same one by luck. These four prices are what every
// return, every indicator and every ADV is computed from downstream, and a wrong
// close does not look wrong — it looks like the market moved.
func TestTheFoldTakesOpenAndCloseFromTheEdgeMinutesAndTheExtremesFromAllOfThem(t *testing.T) {
	got := mustFold(t, store.Resolution1h, hour0, threeMinutes())

	wantDecimal(t, "open", got.Open, 10000, -2, "the open is the FIRST minute's open.")
	wantDecimal(t, "close", got.Close, 15000, -2, "the close is the LAST minute's close.")
	wantDecimal(t, "high", got.High, 20000, -2, "the high is in the MIDDLE minute; reading it off an "+
		"edge bar returns 12000 or 19500.")
	wantDecimal(t, "low", got.Low, 5000, -2, "the low is in the LAST minute; reading it off the first "+
		"bar returns 9500.")
	// 1.0 + 2.0 + 3.0, summed exactly in the decimal domain — dec.Add keeps the
	// operands' own exponent rather than rescaling to a fixed one.
	wantDecimal(t, "volume", got.Volume, 60, -1, "volume is the exact sum of the constituents.")

	if got.Resolution != store.Resolution1h {
		t.Errorf("resolution = %q, want 1h", got.Resolution)
	}
	if !got.BucketStart.Equal(hour0) {
		t.Errorf("bucket_start = %s, want %s", got.BucketStart, hour0)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("the folded bar does not survive store validation: %v", err)
	}
}

// THE INPUT ORDER MUST NOT REACH THE OUTPUT.
//
// "First" and "last" are by BucketStart, not by slice position: the store returns
// ascending today, but a caller that built the slice from a map, an append loop or
// a merge of two sources would hand over any order at all. If the fold read the
// edges positionally, that ordering accident would become the hour's open and
// close — and nothing downstream could tell.
func TestAShuffledBucketFoldsToTheSameBar(t *testing.T) {
	ordered := mustFold(t, store.Resolution1h, hour0, threeMinutes())

	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20; i++ {
		mins := threeMinutes()
		rng.Shuffle(len(mins), func(a, b int) { mins[a], mins[b] = mins[b], mins[a] })
		got := mustFold(t, store.Resolution1h, hour0, mins)
		if !store.SameCandle(ordered, got) || !got.KnowledgeTime.Equal(ordered.KnowledgeTime) {
			t.Fatalf("shuffle %d folded differently: open %v close %v vs open %v close %v — the "+
				"slice order reached the result", i, got.Open, got.Close, ordered.Open, ordered.Close)
		}
	}
}

// THE CALLER'S SLICE IS NOT REORDERED. Fold is documented as pure; sorting in
// place would mutate an argument and the caller's next read of the same slice
// would silently see a different order.
func TestFoldDoesNotReorderTheCallersSlice(t *testing.T) {
	mins := threeMinutes()
	mins[0], mins[2] = mins[2], mins[0]
	first := mins[0].BucketStart

	mustFold(t, store.Resolution1h, hour0, mins)

	if !mins[0].BucketStart.Equal(first) {
		t.Errorf("the caller's slice was reordered: element 0 is now %s, was %s",
			mins[0].BucketStart, first)
	}
}

// A DUPLICATE MINUTE IS REFUSED, NOT RESOLVED. Two bars for the same minute make
// "first" and "last" ambiguous, and picking either would make the open and close
// depend on which copy happened to sort first.
func TestTheFoldRefusesTheSameMinuteTwice(t *testing.T) {
	mins := threeMinutes()
	mins = append(mins, mins[0])

	_, err := Fold(store.Resolution1h, hour0, mins)
	if !errors.Is(err, ErrCannotFold) {
		t.Fatalf("err = %v, want ErrCannotFold — a duplicated minute must not be silently collapsed", err)
	}
}

// THE TRADE COUNT OF AN HOUR WITH ONE UNREPORTED MINUTE IS NIL, NOT A PARTIAL SUM
// (#432).
//
// This is the interesting case: the sum over the reported minutes (3+7=10) is a
// number, it is smaller than the truth by an unknowable amount, and it is
// INDISTINGUISHABLE from a correct count once stored. store.Bar.TradeCount is a
// pointer precisely so absence has a representation that cannot be averaged by
// accident.
func TestAnHourWithAnUnreportedMinuteHasNoTradeCountAtAll(t *testing.T) {
	mins := threeMinutes()
	mins[1].TradeCount = nil // the venue did not tell us for this minute

	got := mustFold(t, store.Resolution1h, hour0, mins)

	if got.TradeCount != nil {
		t.Fatalf("trade_count = %d, want nil. A partial sum invents the claim that the unreported "+
			"minute saw no trades — the exact conflation the nullable column removed.", *got.TradeCount)
	}
}

// A ZERO IS A REAL OBSERVATION AND STILL SUMS. The rule is about REPORTED versus
// not, never about the value: a minute in which nothing traded is data, and
// treating it as absent would be the same conflation running the other way.
func TestAQuietMinuteStillCountsAsReported(t *testing.T) {
	mins := threeMinutes()
	mins[1].TradeCount = ptrTo(0)

	got := mustFold(t, store.Resolution1h, hour0, mins)

	if got.TradeCount == nil {
		t.Fatal("trade_count = nil, want 10. A reported zero is an observation, not a gap.")
	}
	if *got.TradeCount != 10 {
		t.Errorf("trade_count = %d, want 10 (3+0+7)", *got.TradeCount)
	}
}

// THE DERIVED BAR IS STAMPED WITH THE LATEST OF ITS CONSTITUENTS' KNOWLEDGE TIMES.
//
// Anything EARLIER would claim the hour was known before the minute that changed
// it, and a point-in-time read as of that instant would return a bar built from
// data that had not arrived. Anything LATER — the run time, the bucket end — makes
// the stamp depend on when the job ran, so a re-run writes a fresh version of
// every bucket it touches and the whole idempotency argument collapses.
func TestTheDerivedKnowledgeTimeIsTheLatestConstituents(t *testing.T) {
	late := learned.Add(72 * time.Hour) // a restatement that arrived three days later
	mins := threeMinutes()
	mins[1].KnowledgeTime = late

	got := mustFold(t, store.Resolution1h, hour0, mins)

	if !got.KnowledgeTime.Equal(late) {
		t.Fatalf("knowledge_time = %s, want %s — the latest constituent's. An earlier stamp claims "+
			"the hour was known before the minute it was built from.", got.KnowledgeTime, late)
	}
}

// A NIL PRICE MUST NOT BE COMPARED. dec.Cmp reads a nil Decimal as ZERO, so a
// constituent with no high SILENTLY LOSES the maximum comparison and contributes
// nothing — the hour's high is then the highest of the bars that happened to be
// well-formed, and the folded bar validates cleanly because every field it does
// have is consistent.
//
// The high is the interesting direction precisely because the folded bar survives
// it. A nil LOW would propagate into the output and be caught one layer later by
// store.Bar.Validate; a nil HIGH is invisible without this check.
func TestTheFoldRefusesAConstituentWithNoPrice(t *testing.T) {
	mins := threeMinutes()
	mins[1].High = nil // the minute that holds the bucket's true high

	_, err := Fold(store.Resolution1h, hour0, mins)
	if !errors.Is(err, ErrCannotFold) {
		t.Fatalf("err = %v, want ErrCannotFold — a nil high compares as zero, loses the maximum "+
			"silently, and the resulting bar is internally consistent", err)
	}
}

// BARS ARE PER VENUE AND THERE IS NO COMPOSITE SERIES. Merging two venues'
// minutes would produce a candle no order can execute against, and the divergence
// it causes reads as "the strategy stopped working" rather than as a data defect.
func TestTheFoldRefusesToMergeTwoVenues(t *testing.T) {
	mins := threeMinutes()
	mins[1].Venue = "XOKX"

	_, err := Fold(store.Resolution1h, hour0, mins)
	if !errors.Is(err, ErrCannotFold) {
		t.Fatalf("err = %v, want ErrCannotFold — two venues must not fold into one candle", err)
	}
}

// A CONSTITUENT FROM ANOTHER BUCKET IS REFUSED. It is the one mistake a driver
// can make that produces a plausible bar: an off-by-one grouping would fold 61
// minutes and the hour's volume would simply be a little high.
func TestTheFoldRefusesAMinuteFromOutsideTheBucket(t *testing.T) {
	mins := threeMinutes()
	mins = append(mins, minute(hour0, minuteSpec{at: 60, open: 1, high: 1, low: 1, close: 1, volume: 1, trades: ptrTo(1)}))

	_, err := Fold(store.Resolution1h, hour0, mins)
	if !errors.Is(err, ErrCannotFold) {
		t.Fatalf("err = %v, want ErrCannotFold — the 61st minute belongs to the next hour", err)
	}
}

// A ROLLUP OF A ROLLUP IS REFUSED. Folding 1h bars into a day would double-count
// any minute present in both series, and the day would still validate.
func TestTheFoldRefusesACoarseConstituent(t *testing.T) {
	mins := threeMinutes()
	mins[0].Resolution = store.Resolution1h

	_, err := Fold(store.Resolution1d, day0, mins)
	if !errors.Is(err, ErrCannotFold) {
		t.Fatalf("err = %v, want ErrCannotFold — only the 1m base series may be folded", err)
	}
}

// THE TARGET MUST BE COARSER THAN THE BASE. Deriving 1m here would give one
// candle two sources — the thing marketdata.TranslateBar refuses at the ingest
// seam, refused again at the only other door.
//
// The fixture is ONE minute sitting exactly on the requested bucket, so nothing
// else in the fold has grounds to refuse it: without the coarser-than-base check
// this would succeed and produce a 1m bar derived from a 1m bar.
func TestTheFoldRefusesToProduceTheBaseResolution(t *testing.T) {
	_, err := Fold(store.Resolution1m, hour0, threeMinutes()[:1])
	if !errors.Is(err, ErrCannotFold) {
		t.Fatalf("err = %v, want ErrCannotFold — 1m is ingested, not derived", err)
	}
}

// AN UNALIGNED BUCKET START IS REFUSED. A bar covering 12:30–13:30 in a series of
// bars covering 12:00–13:00 is a candle that overlaps two others and belongs to
// neither.
//
// The constituents are placed INSIDE the offset window, so the range check has no
// complaint and this is the only guard standing between the caller and a candle
// on a grid of its own.
func TestTheFoldRefusesAnUnalignedBucketStart(t *testing.T) {
	offset := hour0.Add(30 * time.Minute)
	mins := []store.Bar{
		minute(offset, minuteSpec{at: 0, open: 10000, high: 12000, low: 9500, close: 11000, volume: 10, trades: ptrTo(3)}),
		minute(offset, minuteSpec{at: 1, open: 11000, high: 20000, low: 10500, close: 19000, volume: 20, trades: ptrTo(5)}),
	}

	_, err := Fold(store.Resolution1h, offset, mins)
	if !errors.Is(err, ErrCannotFold) {
		t.Fatalf("err = %v, want ErrCannotFold — a bucket must start on a boundary of its own "+
			"resolution", err)
	}
}

// AN EMPTY BUCKET HAS NO BAR. There is no price to open at, so the alternative to
// refusing is inventing one.
func TestTheFoldRefusesAnEmptyBucket(t *testing.T) {
	_, err := Fold(store.Resolution1h, hour0, nil)
	if !errors.Is(err, ErrCannotFold) {
		t.Fatalf("err = %v, want ErrCannotFold — an OHLC bar with no prices can only be invented", err)
	}
}
