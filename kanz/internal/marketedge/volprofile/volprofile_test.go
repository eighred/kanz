package volprofile

import (
	"errors"
	"math/big"
	"reflect"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/marketedge/bars"
	"github.com/eighred/kanz/internal/marketedge/trades"
)

var (
	btcBinance = Series{InstrumentID: "BTC-USDT", Venue: "XBIN"}
	btcOKX     = Series{InstrumentID: "BTC-USDT", Venue: "XOKX"}
	day0       = time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC) // a Monday
)

// store builds a Store with the given minimum and the shipped defaults, failing
// the test rather than returning an error nobody would read.
func store(t *testing.T, minSessions int) *Store {
	t.Helper()
	s, err := New(Config{MinSessions: minSessions})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// trade is one print of `size` at `at`. Price is irrelevant to a volume profile
// and is set only so the fixture reads like a real trade.
func trade(at time.Time, size int64) trades.Trade {
	return trades.Trade{
		Price:     big.NewRat(30000, 1),
		Size:      big.NewRat(size, 1),
		TakerSide: trades.SideBuy,
		EventTime: at,
	}
}

// feedFlat prints `size` in every bucket of session `day`, so the session's shape
// is uniform and its total is size*bucketsPerSession.
func feedFlat(s *Store, ser Series, day time.Time, size int64) {
	for i := 0; i < BucketsPerSession(DefaultBucket); i++ {
		s.Observe(ser, trade(day.Add(time.Duration(i)*DefaultBucket+time.Minute), size))
	}
}

// A PROFILE THAT DOES NOT EXIST REPORTS UNKNOWN, NOT A FLAT SHAPE.
//
// This is the property the whole package exists for. A flat profile IS TWAP, so
// an implementation that answered a never-observed series with equal shares would
// turn a VWAP order into a TWAP order wearing the wrong name, and the execution
// attribution would name the algorithm that did not run.
func TestAnAbsentProfileIsUnknownAndCarriesNoShape(t *testing.T) {
	s := store(t, 5)
	a, err := s.Profile(btcBinance, day0)
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if a.Known() {
		t.Fatal("a series nothing has ever been observed for reported a KNOWN profile — a " +
			"scheduler would slice against a shape nobody measured")
	}
	if a.Verdict != VerdictAbsent {
		t.Fatalf("Verdict = %v, want %v", a.Verdict, VerdictAbsent)
	}
	if a.Shares != nil {
		t.Fatalf("an unknown answer carries %d share(s) — a caller that reads Shares without "+
			"checking the verdict gets a shape, which is exactly the substitution this package "+
			"refuses", len(a.Shares))
	}
}

// THE ZERO ANSWER IS UNKNOWN.
//
// A struct nobody filled in must not read as a known shape: the zero value is the
// one a forgotten assignment, a dropped error branch or a partially built fixture
// produces, and it has to fail closed like every other critical unknown here.
func TestTheZeroAnswerIsUnknown(t *testing.T) {
	var a Answer
	if a.Known() {
		t.Fatal("the zero Answer reports Known — an answer nobody computed would be read as a " +
			"measured shape")
	}
	if a.Verdict != VerdictAbsent {
		t.Fatalf("the zero Verdict is %v; it must be %v so a forgotten assignment fails closed",
			a.Verdict, VerdictAbsent)
	}
	if _, ok := a.Share(day0); ok {
		t.Fatal("the zero Answer resolved a bucket share")
	}
}

// A PROFILE OLDER THAN ITS HORIZON IS UNKNOWN, NOT SERVED STALE.
func TestAProfileOlderThanTheHorizonIsUnknown(t *testing.T) {
	s := store(t, 5)
	for d := 0; d < 6; d++ {
		feedFlat(s, btcBinance, day0.Add(time.Duration(d)*Session), 10)
	}
	s.Advance(day0.Add(6 * Session))

	fresh, err := s.Profile(btcBinance, day0.Add(6*Session))
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if !fresh.Known() {
		t.Fatalf("the profile is not known the day after it was built: %v", fresh.Verdict)
	}

	// One instant past the horizon measured from the newest completed session's
	// end. Everything retained is older than the window the horizon describes.
	late := day0.Add(6*Session + DefaultHorizon + time.Nanosecond)
	a, err := s.Profile(btcBinance, late)
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if a.Known() {
		t.Fatal("a profile whose newest session is past the horizon reported KNOWN — the " +
			"scheduler would slice today against a shape from a month ago")
	}
	if a.Verdict != VerdictStale {
		t.Fatalf("Verdict = %v, want %v", a.Verdict, VerdictStale)
	}
	if a.Shares != nil {
		t.Fatal("a stale answer carries a shape")
	}
	if a.Newest.IsZero() {
		t.Fatal("a stale answer does not say how old it is — an operator cannot tell a feed " +
			"that died yesterday from one that never started")
	}
}

// TOO LITTLE HISTORY IS UNKNOWN, AND IT IS A DISTINCT ANSWER FROM ABSENT.
func TestTooFewSessionsIsUnknownAndDistinctFromAbsent(t *testing.T) {
	s := store(t, 5)
	for d := 0; d < 3; d++ {
		feedFlat(s, btcBinance, day0.Add(time.Duration(d)*Session), 10)
	}
	s.Advance(day0.Add(3 * Session))

	a, err := s.Profile(btcBinance, day0.Add(3*Session))
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if a.Known() {
		t.Fatal("three sessions satisfied a five-session minimum")
	}
	if a.Verdict != VerdictTooFewSessions {
		t.Fatalf("Verdict = %v, want %v", a.Verdict, VerdictTooFewSessions)
	}
	if a.Sessions != 3 {
		t.Fatalf("Sessions = %d, want 3 — the answer must say how much history it has, or an "+
			"operator cannot tell 'nearly ready' from 'nothing here'", a.Sessions)
	}
	if a.Shares != nil {
		t.Fatal("an answer with too little history carries a shape")
	}
}

// THE SHAPE IS THE MEASURED ONE, AND THE SHARES SUM TO EXACTLY ONE.
//
// The fixture is the issue's own evidence: a session that prints most of its
// volume in the final stretch. A flat answer here is the defect.
func TestSharesFollowTheObservedShape(t *testing.T) {
	s := store(t, 2)
	n := BucketsPerSession(DefaultBucket)
	for d := 0; d < 3; d++ {
		day := day0.Add(time.Duration(d) * Session)
		for i := 0; i < n; i++ {
			size := int64(1)
			if i >= n-2 { // the final hour
				size = 10
			}
			s.Observe(btcBinance, trade(day.Add(time.Duration(i)*DefaultBucket+time.Minute), size))
		}
	}
	s.Advance(day0.Add(3 * Session))

	a, err := s.Profile(btcBinance, day0.Add(3*Session))
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if !a.Known() {
		t.Fatalf("verdict = %v, want a known profile", a.Verdict)
	}
	if len(a.Shares) != n {
		t.Fatalf("len(Shares) = %d, want %d", len(a.Shares), n)
	}

	// Each session prints 1 in each of the first n-2 buckets and 10 in each of the
	// last two: total = (n-2) + 20.
	total := int64(n-2) + 20
	wantEarly := big.NewRat(1, total)
	wantLate := big.NewRat(10, total)
	if a.Shares[0].Cmp(wantEarly) != 0 {
		t.Fatalf("Shares[0] = %s, want %s", a.Shares[0], wantEarly)
	}
	if a.Shares[n-1].Cmp(wantLate) != 0 {
		t.Fatalf("Shares[%d] = %s, want %s — the final-hour concentration this profile exists "+
			"to expose was flattened away", n-1, a.Shares[n-1], wantLate)
	}
	if a.Shares[n-1].Cmp(a.Shares[0]) <= 0 {
		t.Fatal("the closing buckets do not carry more of the session than the opening ones — " +
			"the profile is flat, which IS TWAP")
	}

	sum := new(big.Rat)
	for _, sh := range a.Shares {
		sum.Add(sum, sh)
	}
	if sum.Cmp(big.NewRat(1, 1)) != 0 {
		t.Fatalf("the shares sum to %s, not exactly 1 — a schedule built on them would place "+
			"more or less than the order", sum)
	}
}

// EVERY SESSION WEIGHS THE SAME.
//
// The shape is the MEAN OF EACH SESSION'S SHARES, not the pooled distribution. A
// pooled sum is dominated by whichever day traded most, so one liquidation
// cascade would set the schedule for the following month. This fixture is built
// so the two answers differ: one enormous flat session against two small skewed
// ones.
func TestEachSessionWeighsTheSame(t *testing.T) {
	s := store(t, 3)
	n := BucketsPerSession(DefaultBucket)

	// Session 0: flat, and 1000x the size of the others.
	feedFlat(s, btcBinance, day0, 1000)
	// Sessions 1 and 2: everything in the last bucket, tiny.
	for d := 1; d < 3; d++ {
		day := day0.Add(time.Duration(d) * Session)
		s.Observe(btcBinance, trade(day.Add(time.Duration(n-1)*DefaultBucket+time.Minute), 1))
	}
	s.Advance(day0.Add(3 * Session))

	a, err := s.Profile(btcBinance, day0.Add(3*Session))
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if !a.Known() {
		t.Fatalf("verdict = %v", a.Verdict)
	}

	// Mean of shares: bucket n-1 gets (1/n + 1 + 1)/3.
	want := new(big.Rat).Quo(
		new(big.Rat).Add(big.NewRat(1, int64(n)), big.NewRat(2, 1)),
		big.NewRat(3, 1),
	)
	if a.Shares[n-1].Cmp(want) != 0 {
		t.Fatalf("Shares[%d] = %s, want %s — the big flat session is setting the shape, so the "+
			"profile is a pooled distribution rather than a per-session mean and one outlier day "+
			"decides the next month's schedule", n-1, a.Shares[n-1], want)
	}
}

// TWO VENUES ON ONE INSTRUMENT ARE TWO PROFILES.
//
// A profile keyed only by instrument is a wrong number rather than a
// simplification: the venues have different intraday shapes, and a schedule
// derived from the wrong one participates against liquidity that is not there.
func TestVenuesAreProfiledSeparately(t *testing.T) {
	s := store(t, 2)
	n := BucketsPerSession(DefaultBucket)

	for d := 0; d < 3; d++ {
		day := day0.Add(time.Duration(d) * Session)
		// Binance: everything in the FIRST bucket.
		s.Observe(btcBinance, trade(day.Add(time.Minute), 100))
		// OKX: everything in the LAST bucket.
		s.Observe(btcOKX, trade(day.Add(time.Duration(n-1)*DefaultBucket+time.Minute), 100))
	}
	s.Advance(day0.Add(3 * Session))

	bin, err := s.Profile(btcBinance, day0.Add(3*Session))
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	okx, err := s.Profile(btcOKX, day0.Add(3*Session))
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if !bin.Known() || !okx.Known() {
		t.Fatalf("verdicts: binance=%v okx=%v", bin.Verdict, okx.Verdict)
	}
	one := big.NewRat(1, 1)
	if bin.Shares[0].Cmp(one) != 0 || bin.Shares[n-1].Sign() != 0 {
		t.Fatalf("the Binance shape is not front-loaded: first=%s last=%s",
			bin.Shares[0], bin.Shares[n-1])
	}
	if okx.Shares[n-1].Cmp(one) != 0 || okx.Shares[0].Sign() != 0 {
		t.Fatalf("the OKX shape is not back-loaded: first=%s last=%s — the two venues' prints "+
			"were merged into one shape, and a schedule built from it matches neither book",
			okx.Shares[0], okx.Shares[n-1])
	}
}

// AN UNOBSERVED BUCKET IS UNKNOWN AND IS NEVER QUIET.
//
// This package holds no ingestion-coverage attestation, so it cannot tell a
// bucket in which nothing traded from one in which the feed was down. Counting
// the first as a measured zero would be a claim nobody made.
func TestAnUnobservedBucketIsUnknownAndNeverQuiet(t *testing.T) {
	s := store(t, 2)
	n := BucketsPerSession(DefaultBucket)

	// Each session prints in exactly two buckets; the other n-2 are unobserved.
	for d := 0; d < 3; d++ {
		day := day0.Add(time.Duration(d) * Session)
		s.Observe(btcBinance, trade(day.Add(time.Minute), 5))
		s.Observe(btcBinance, trade(day.Add(time.Duration(n-1)*DefaultBucket+time.Minute), 5))
	}
	s.Advance(day0.Add(3 * Session))

	a, err := s.Profile(btcBinance, day0.Add(3*Session))
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if got, want := a.Window.Buckets, 3*n; got != want {
		t.Fatalf("Window.Buckets = %d, want %d", got, want)
	}
	if got, want := a.Window.Observed, 6; got != want {
		t.Fatalf("Window.Observed = %d, want %d", got, want)
	}
	if got, want := a.Window.Unknown(), 3*n-6; got != want {
		t.Fatalf("Window.Unknown() = %d, want %d — an interval nobody saw is being reported as "+
			"a measured zero", got, want)
	}
	if a.Window.Sound() {
		t.Fatal("a window with unobserved buckets reported Sound — a caller gating on it would " +
			"treat a shape full of holes as fully measured")
	}
}

// COVERAGE IS REPORTED, NOT A GATE.
//
// The same asymmetry the bar view already settled: readings over what IS there
// are provable, and refusing every window with a hole would blank the profile on
// every instrument thin enough to skip a bucket, which is most of them. The
// counter is what a scheduler decides on.
func TestAnUnsoundWindowStillYieldsAKnownProfile(t *testing.T) {
	s := store(t, 2)
	n := BucketsPerSession(DefaultBucket)
	for d := 0; d < 3; d++ {
		day := day0.Add(time.Duration(d) * Session)
		s.Observe(btcBinance, trade(day.Add(time.Duration(n-1)*DefaultBucket+time.Minute), 5))
	}
	s.Advance(day0.Add(3 * Session))

	a, err := s.Profile(btcBinance, day0.Add(3*Session))
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if a.Window.Sound() {
		t.Fatal("the fixture is not exercising an unsound window")
	}
	if !a.Known() {
		t.Fatalf("verdict = %v — an unsound window was turned into a refusal, which blanks the "+
			"profile on every thin instrument", a.Verdict)
	}
}

// THE SESSION IN PROGRESS DOES NOT CONTRIBUTE.
//
// A partial session is the hours that have already elapsed, and folding it in
// tilts the shape toward the morning every morning.
func TestTheSessionInProgressDoesNotContribute(t *testing.T) {
	s := store(t, 2)
	n := BucketsPerSession(DefaultBucket)
	for d := 0; d < 3; d++ {
		feedFlat(s, btcBinance, day0.Add(time.Duration(d)*Session), 10)
	}
	// A fourth session, only its first bucket, and NOT advanced past.
	s.Observe(btcBinance, trade(day0.Add(3*Session+time.Minute), 1_000_000))

	a, err := s.Profile(btcBinance, day0.Add(3*Session+time.Hour))
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if a.Sessions != 3 {
		t.Fatalf("Sessions = %d, want 3 — the session in progress was counted", a.Sessions)
	}
	want := big.NewRat(1, int64(n))
	if a.Shares[0].Cmp(want) != 0 {
		t.Fatalf("Shares[0] = %s, want %s — the in-progress session's opening print tilted the "+
			"shape, which happens every morning and always in the same direction",
			a.Shares[0], want)
	}
}

// RETENTION IS BOUNDED BY THE HORIZON, MEASURED FROM THE NEWEST SESSION RETAINED.
func TestRetentionDropsSessionsPastTheHorizon(t *testing.T) {
	s, err := New(Config{MinSessions: 2, Horizon: 3 * Session})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for d := 0; d < 10; d++ {
		feedFlat(s, btcBinance, day0.Add(time.Duration(d)*Session), 10)
	}
	s.Advance(day0.Add(10 * Session))

	a, err := s.Profile(btcBinance, day0.Add(10*Session))
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	// A 3-session horizon measured back from the newest retained session START
	// keeps that session and the three before it: sessions 6..9.
	if a.Sessions != 4 {
		t.Fatalf("Sessions = %d, want 4 — the horizon is not bounding the retained set", a.Sessions)
	}
	if !a.Oldest.Equal(day0.Add(6 * Session)) {
		t.Fatalf("Oldest = %s, want %s", a.Oldest, day0.Add(6*Session))
	}
}

// THE HORIZON IS ANCHORED ON THE NEWEST RETAINED SESSION, NOT ON WALL CLOCK.
//
// A wall-clock prune would empty the store the moment a feed stalled, turning
// "this profile is stale" into "there is no profile" — and the two are different
// answers to a scheduler. The staleness verdict is where the age is reported.
func TestAStalledFeedKeepsItsSessionsRatherThanGoingEmpty(t *testing.T) {
	s, err := New(Config{MinSessions: 2, Horizon: 3 * Session})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for d := 0; d < 4; d++ {
		feedFlat(s, btcBinance, day0.Add(time.Duration(d)*Session), 10)
	}
	s.Advance(day0.Add(4 * Session))

	// A year later. Nothing has been observed since.
	a, err := s.Profile(btcBinance, day0.Add(365*Session))
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if a.Verdict != VerdictStale {
		t.Fatalf("Verdict = %v, want %v", a.Verdict, VerdictStale)
	}
	if a.Sessions == 0 {
		t.Fatal("a stalled feed emptied the store — a stale profile and an absent one became " +
			"the same answer, and the operator loses the ability to tell them apart")
	}
}

// THE PROFILE IS COMPUTED ONCE AND READ MANY TIMES.
//
// A shape recomputed per order is a per-session division over every retained
// bucket on the order path. The cache is invalidated by a session completing and
// by nothing else.
func TestTheProfileIsComputedOnceAndReadManyTimes(t *testing.T) {
	s := store(t, 2)
	for d := 0; d < 4; d++ {
		feedFlat(s, btcBinance, day0.Add(time.Duration(d)*Session), 10)
	}
	s.Advance(day0.Add(4 * Session))

	before := s.Recomputes()
	for i := 0; i < 50; i++ {
		if _, err := s.Profile(btcBinance, day0.Add(4*Session)); err != nil {
			t.Fatalf("Profile: %v", err)
		}
	}
	after := s.Recomputes()
	if got := after - before; got != 1 {
		t.Fatalf("50 reads caused %d recomputation(s), want exactly 1 — the shape is being "+
			"rebuilt per query, which on the order path is a division per retained bucket per "+
			"order", got)
	}

	// A new completed session must invalidate it, or the profile silently stops
	// following the market.
	feedFlat(s, btcBinance, day0.Add(4*Session), 10)
	s.Advance(day0.Add(5 * Session))
	if _, err := s.Profile(btcBinance, day0.Add(5*Session)); err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if got := s.Recomputes() - after; got != 1 {
		t.Fatalf("a newly completed session caused %d recomputation(s), want 1 — the cached "+
			"shape outlived the data it was built from", got)
	}
}

// A MUTATION OF THE RETURNED SHARES MUST NOT REACH THE CACHE.
func TestTheReturnedSharesAreTheCallersOwn(t *testing.T) {
	s := store(t, 2)
	for d := 0; d < 3; d++ {
		feedFlat(s, btcBinance, day0.Add(time.Duration(d)*Session), 10)
	}
	s.Advance(day0.Add(3 * Session))

	a, err := s.Profile(btcBinance, day0.Add(3*Session))
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	a.Shares[0].SetInt64(999)

	b, err := s.Profile(btcBinance, day0.Add(3*Session))
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if b.Shares[0].Cmp(big.NewRat(999, 1)) == 0 {
		t.Fatal("a caller's mutation of the returned shares reached the cached shape — every " +
			"later reader gets the corrupted profile and nothing says so")
	}
}

// Share RESOLVES THE BUCKET CONTAINING AN INSTANT.
func TestShareResolvesTheBucketContainingAnInstant(t *testing.T) {
	s := store(t, 2)
	n := BucketsPerSession(DefaultBucket)
	for d := 0; d < 3; d++ {
		day := day0.Add(time.Duration(d) * Session)
		s.Observe(btcBinance, trade(day.Add(time.Duration(n-1)*DefaultBucket+time.Minute), 7))
	}
	s.Advance(day0.Add(3 * Session))

	a, err := s.Profile(btcBinance, day0.Add(3*Session))
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	// 23:45 on any day falls in the final 30-minute bucket.
	got, ok := a.Share(day0.Add(9 * Session).Add(23*time.Hour + 45*time.Minute))
	if !ok {
		t.Fatal("Share reported no answer for a known profile")
	}
	if got.Cmp(big.NewRat(1, 1)) != 0 {
		t.Fatalf("Share = %s, want 1", got)
	}
	// 00:15 falls in the first bucket, which never traded.
	got, ok = a.Share(day0.Add(9 * Session).Add(15 * time.Minute))
	if !ok {
		t.Fatal("Share reported no answer for the opening bucket")
	}
	if got.Sign() != 0 {
		t.Fatalf("Share = %s, want 0", got)
	}
}

// AN OUT-OF-ORDER PRINT FOR A CLOSED SESSION IS DROPPED AND COUNTED.
//
// Restating a session already folded into the shape would change a profile
// readers have already scheduled against. The count is the only evidence the
// shape is short.
func TestALatePrintIsDroppedAndCounted(t *testing.T) {
	s := store(t, 2)
	feedFlat(s, btcBinance, day0, 10)
	feedFlat(s, btcBinance, day0.Add(Session), 10)
	feedFlat(s, btcBinance, day0.Add(2*Session), 10)
	s.Advance(day0.Add(3 * Session))

	before := s.Late()
	s.Observe(btcBinance, trade(day0.Add(time.Minute), 1_000_000))
	if got := s.Late() - before; got != 1 {
		t.Fatalf("Late() moved by %d, want 1", got)
	}

	a, err := s.Profile(btcBinance, day0.Add(3*Session))
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	want := big.NewRat(1, int64(BucketsPerSession(DefaultBucket)))
	if a.Shares[0].Cmp(want) != 0 {
		t.Fatalf("Shares[0] = %s, want %s — a late print restated a session already in the "+
			"shape", a.Shares[0], want)
	}
}

// THERE IS NO MINIMUM-SESSION DEFAULT.
//
// How much history a desk demands before it will schedule against a shape is an
// execution-policy decision, not a market-data fact. A number invented here would
// either refuse a healthy profile or let a two-day sample set the schedule, and
// "nothing configured" would look exactly like "checked, and fine".
func TestMinSessionsHasNoDefault(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New accepted a Config with no MinSessions — an unconfigured store would " +
			"answer from whatever history it happened to have")
	}
	if _, err := New(Config{MinSessions: -1}); err == nil {
		t.Fatal("New accepted a negative MinSessions")
	}
}

// A BUCKET THAT DOES NOT DIVIDE THE SESSION IS REFUSED.
func TestABucketMustDivideTheSession(t *testing.T) {
	if _, err := New(Config{MinSessions: 2, Bucket: 7 * time.Minute}); err == nil {
		t.Fatal("New accepted a 7-minute bucket — 24h is not a whole number of them, so the " +
			"last bucket of every session would be short and its share understated")
	}
	if _, err := New(Config{MinSessions: 2, Bucket: 90 * time.Second}); err == nil {
		t.Fatal("New accepted a bucket that is not a whole number of one-minute bars")
	}
	if _, err := New(Config{MinSessions: 2, Bucket: 15 * time.Minute}); err != nil {
		t.Fatalf("New refused a 15-minute bucket, which divides both the session and the bar "+
			"resolution: %v", err)
	}
}

// AN AS-OF IS REQUIRED AND DOES NOT DEFAULT TO NOW.
//
// The same refusal the bar view makes: a default to wall clock is correct for a
// live scheduler and total look-ahead for a backtest, and it arrives wearing the
// shape of a convenience.
func TestAnAsOfIsRequired(t *testing.T) {
	s := store(t, 2)
	if _, err := s.Profile(btcBinance, time.Time{}); err == nil {
		t.Fatal("Profile accepted a zero as-of — a caller that forgot one would be answered " +
			"from wall clock, which in a backtest is look-ahead")
	}
}

// THE SERIES KEY IS THE ONE THE REST OF THE EDGE USES.
//
// bars.Series and coverage.Series are the same two fields, and this is a third
// copy rather than an import because taking either would make the profile depend
// on a bus publisher it never uses. The agreement is asserted rather than left to
// a comment, exactly as coverage asserts its resolution against the bar fold's.
func TestSeriesMatchesTheBarSeries(t *testing.T) {
	got := reflect.TypeOf(Series{})
	want := reflect.TypeOf(bars.Series{})
	if got.NumField() != want.NumField() {
		t.Fatalf("volprofile.Series has %d field(s), bars.Series has %d",
			got.NumField(), want.NumField())
	}
	for i := 0; i < want.NumField(); i++ {
		w, g := want.Field(i), got.Field(i)
		if w.Name != g.Name || w.Type != g.Type {
			t.Fatalf("field %d: volprofile.Series has %s %s, bars.Series has %s %s — the edge's "+
				"one key for 'this instrument on this venue' has drifted, and a profile keyed "+
				"differently from the series it is built from cannot be joined to it",
				i, g.Name, g.Type, w.Name, w.Type)
		}
	}
}

// A ZERO-SIZE OR UNTIMED PRINT IS NOISE, NOT AN EXECUTION.
func TestNoiseIsNotFolded(t *testing.T) {
	s := store(t, 1)
	s.Observe(btcBinance, trades.Trade{Size: big.NewRat(0, 1), EventTime: day0})
	s.Observe(btcBinance, trades.Trade{Size: nil, EventTime: day0})
	s.Observe(btcBinance, trades.Trade{Size: big.NewRat(1, 1)}) // zero event time
	s.Advance(day0.Add(2 * Session))

	a, err := s.Profile(btcBinance, day0.Add(2*Session))
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if a.Verdict != VerdictAbsent {
		t.Fatalf("Verdict = %v, want %v — noise opened a session", a.Verdict, VerdictAbsent)
	}
}

// AN AS-OF BEFORE THE NEWEST COMPLETED SESSION IS REFUSED, NOT ANSWERED.
//
// The store only ever moves forward, so answering such a read would hand back a
// shape containing sessions the caller could not have seen. It is the leak that
// flatters most and shows least: the shares are real, from real sessions, and
// describe a future the order did not have.
func TestAnAsOfBeforeTheNewestSessionIsRefused(t *testing.T) {
	s := store(t, 2)
	for d := 0; d < 4; d++ {
		feedFlat(s, btcBinance, day0.Add(time.Duration(d)*Session), 10)
	}
	s.Advance(day0.Add(4 * Session))

	// The newest completed session ends at day0+4S; a read as of the day before
	// would be answered from it.
	if _, err := s.Profile(btcBinance, day0.Add(2*Session)); !errors.Is(err, ErrLookahead) {
		t.Fatalf("Profile as of an earlier session returned %v, want ErrLookahead — a backtest "+
			"would have been handed the shape of days it had not reached", err)
	}
	if _, err := s.Profile(btcBinance, day0.Add(4*Session)); err != nil {
		t.Fatalf("Profile at the newest session's close was refused: %v", err)
	}
}
