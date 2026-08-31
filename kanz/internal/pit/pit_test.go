package pit

import (
	"testing"
	"time"
)

var base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// RETENTION MUST STOP GROWING (#811). Before the horizon, an intraday
// calibration cadence appended a version per refresh and removed none, so the
// retained set was a function of uptime: at the one-minute cadence the
// risk-engine can be configured with, ~525k curves per currency per year.
func TestPutRetentionStopsGrowing(t *testing.T) {
	const horizon = 24 * time.Hour
	// Ten days of one-minute refreshes: four times past the horizon, so a store
	// that merely grew slower would still fail this.
	const refreshes = 10 * 24 * 60
	var vs []Version[int]
	high := 0
	for i := 0; i < refreshes; i++ {
		vs, _ = Put(vs, base.Add(time.Duration(i)*time.Minute), i, horizon)
		if len(vs) > high {
			high = len(vs)
		}
	}
	// A horizon of 24h at a 1m cadence retains the newest plus the 1440 minutes
	// behind it.
	const want = 24*60 + 1
	if len(vs) != want {
		t.Fatalf("after %d one-minute refreshes under a %s horizon the list holds %d versions, "+
			"want %d — retention is not bounded by the horizon", refreshes, horizon, len(vs), want)
	}
	if high != want {
		t.Fatalf("the list peaked at %d versions, want %d: it grew past the horizon at some point "+
			"and was only trimmed later, so peak memory still tracks uptime", high, want)
	}
}

// A VERSION INSIDE THE HORIZON IS STILL READABLE. The whole point of these
// stores is that a valuation as of T resolves what was live at T; a bound that
// evicted the answer would trade an unbounded store for a wrong risk number.
func TestAtResolvesAVersionInsideTheHorizon(t *testing.T) {
	const horizon = 7 * 24 * time.Hour
	var vs []Version[string]
	for i := 0; i <= 14; i++ { // two weeks of daily closes
		vs, _ = Put(vs, base.AddDate(0, 0, i), day(i), horizon)
	}
	newest := base.AddDate(0, 0, 14)

	// Six days back is inside a seven-day horizon and must still resolve to the
	// version live then, not to a later one.
	if got, ok := At(vs, newest.AddDate(0, 0, -6)); !ok || got != day(8) {
		t.Fatalf("six days inside a %s horizon resolved %q (ok=%v), want %q", horizon, got, ok, day(8))
	}
	// An as-of BETWEEN two versions resolves the earlier one — the point-in-time
	// contract, unchanged by retention.
	if got, ok := At(vs, newest.AddDate(0, 0, -6).Add(13*time.Hour)); !ok || got != day(8) {
		t.Fatalf("between versions resolved %q (ok=%v), want %q — a read must never pick up a "+
			"version from after the state it is pricing", got, ok, day(8))
	}
	// Eight days back is outside it and must report NOT KNOWN rather than the
	// oldest surviving version. Substituting one would price a position against a
	// curve from a week after the state it holds.
	if got, ok := At(vs, newest.AddDate(0, 0, -8)); ok {
		t.Fatalf("outside the horizon At resolved %q, want ok=false", got)
	}
}

// THE NEWEST VERSION IS NEVER EVICTED, so a store whose calibration feed has
// stalled keeps serving its last value. The alternative — a wall-clock horizon
// — turns "the curve is stale" into "there is no curve", and a missing discount
// curve is a DV01 of zero on a book that holds bonds.
func TestPutNeverEvictsTheNewestVersion(t *testing.T) {
	vs, _ := Put(nil, base, "only", time.Nanosecond)
	if len(vs) != 1 {
		t.Fatalf("a %s horizon retained %d versions, want the newest kept", time.Nanosecond, len(vs))
	}
	// And it stays readable arbitrarily far into the future: a stalled feed
	// degrades to stale, never to dark.
	if got, ok := At(vs, base.AddDate(1, 0, 0)); !ok || got != "only" {
		t.Fatalf("a year after the last refresh the store answered %q (ok=%v), want %q — a stalled "+
			"feed must degrade to a stale value, not to no value", got, ok, "only")
	}
}

// AN IN-HORIZON BACKFILL STILL INSERTS IN PLACE. The nightly-close job can land
// behind an intraday refresh, and that ordering is the reason Put sorts at all.
func TestPutAcceptsAnInHorizonBackfill(t *testing.T) {
	const horizon = 7 * 24 * time.Hour
	vs, _ := Put(nil, base.Add(48*time.Hour), "late", horizon)
	vs, dropped := Put(vs, base, "backfill", horizon)
	if dropped != 0 {
		t.Fatalf("an in-horizon backfill was dropped (%d) — the nightly close landing behind an "+
			"intraday refresh is the ordinary case", dropped)
	}
	if len(vs) != 2 || vs[0].V != "backfill" {
		t.Fatalf("backfill did not insert at index 0: %+v", vs)
	}
	if got, ok := At(vs, base.Add(time.Hour)); !ok || got != "backfill" {
		t.Fatalf("resolved %q (ok=%v) at an as-of the backfill covers, want %q", got, ok, "backfill")
	}
}

// A BACKFILL OLDER THAN THE HORIZON IS DROPPED, AND THE CALLER IS TOLD. Silence
// here would be data loss that reported success.
func TestPutReportsAnOutOfHorizonBackfill(t *testing.T) {
	const horizon = 24 * time.Hour
	vs, _ := Put(nil, base, "newest", horizon)
	vs, dropped := Put(vs, base.Add(-48*time.Hour), "ancient", horizon)
	if dropped != 1 {
		t.Fatalf("dropped=%d for a backfill two days outside a %s horizon, want 1 — a Put that "+
			"retains nothing must say so", dropped, horizon)
	}
	if len(vs) != 1 || vs[0].V != "newest" {
		t.Fatalf("the out-of-horizon backfill was retained: %+v", vs)
	}
}

// A SECOND PUT AT THE SAME AS-OF REPLACES — the recalibration contract every
// store documents, and it must survive the retention branch.
func TestPutReplacesAtTheSameAsOf(t *testing.T) {
	const horizon = 24 * time.Hour
	vs, _ := Put(nil, base, "first", horizon)
	vs, dropped := Put(vs, base, "second", horizon)
	if dropped != 0 || len(vs) != 1 || vs[0].V != "second" {
		t.Fatalf("same-as-of Put gave %+v dropped=%d, want one version holding %q", vs, dropped, "second")
	}
}

// AN EMPTY LIST HAS NO VERSION, NOT A ZERO ONE. A caller that read a zero curve
// out of a missing one would price CVA at zero and DV01 at zero, both of which
// make a limit pass.
func TestAtOnAnEmptyListIsNotKnown(t *testing.T) {
	if got, ok := At[string](nil, base); ok {
		t.Fatalf("At on an empty list returned %v, want ok=false", got)
	}
}

// THE HORIZON HELPER IS WHERE "UNSET" STOPS MEANING "FOREVER". Every store
// funnels its option through it, so a store that forgets gets the documented
// default rather than the #811 behaviour.
func TestHorizonSubstitutesTheDefault(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Hour} {
		if got := Horizon(d); got != DefaultHorizon {
			t.Fatalf("Horizon(%s) = %s, want DefaultHorizon %s", d, got, DefaultHorizon)
		}
	}
	if got := Horizon(time.Hour); got != time.Hour {
		t.Fatalf("Horizon(1h) = %s, want it left alone", got)
	}
}

// THE DEFAULT IS SPOTSOURCE'S MARK MAX-AGE, DELIBERATELY. This asserts the
// number rather than the identifier because internal/risk/spotsource cannot be
// imported here without a cycle through pricing; if either constant moves, one
// of the two valuation inputs becomes admissible while the other has been
// dropped, and this is the line that says so.
func TestDefaultHorizonMatchesTheMarkMaxAge(t *testing.T) {
	if DefaultHorizon != 7*24*time.Hour {
		t.Fatalf("DefaultHorizon is %s, want 7 days — spotsource.DefaultMaxAge refuses a mark "+
			"older than a week at the valuation as-of, so a shorter curve horizon drops the curve "+
			"for a mark the system still accepts, and a longer one retains curves no admissible "+
			"mark can pair with", DefaultHorizon)
	}
}

func day(i int) string { return time.Duration(i).String() }
