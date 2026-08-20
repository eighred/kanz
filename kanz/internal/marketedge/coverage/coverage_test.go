package coverage_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/marketedge/bars"
	"github.com/eighred/kanz/internal/marketedge/coverage"
	"github.com/eighred/kanz/pkg/bus"
)

// recordingPub captures what would go on the bus. It does NOT validate the
// envelope — publish_test.go runs a real producer for that.
type recordingPub struct {
	mu   sync.Mutex
	sent []*marketpb.IngestionCoverage
	err  error
}

func (p *recordingPub) Publish(_ context.Context, e bus.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	raw, err := proto.Marshal(e.Payload)
	if err != nil {
		return err
	}
	var cov marketpb.IngestionCoverage
	if err := proto.Unmarshal(raw, &cov); err != nil {
		return err
	}
	p.sent = append(p.sent, &cov)
	return nil
}

func (p *recordingPub) records() []*marketpb.IngestionCoverage {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*marketpb.IngestionCoverage, len(p.sent))
	copy(out, p.sent)
	return out
}

var (
	series = coverage.Series{InstrumentID: "BTC-USDT", Venue: "XBIN"}
	base   = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
)

func newRec(t *testing.T, pub coverage.Publisher, maxSilence time.Duration) *coverage.Recorder {
	t.Helper()
	rec, err := coverage.NewRecorder(coverage.Config{
		Publisher: pub, Tenant: "acme", MaxSilence: maxSilence,
	})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	return rec
}

// THE RESOLUTION MUST MATCH THE BAR SERIES, and this is the assertion rather
// than a comment.
//
// The record's only job is to explain an absence in the 1-minute bar series. If
// the two ever disagree, every coverage lookup silently answers about a
// different interval than the one the reader is missing a bar for — and the
// answer would still look like a coverage record. The package deliberately does
// not import bars (coverage must not depend on the series it vouches for), so
// this test is the only place the equality is checked.
func TestResolutionMatchesBars(t *testing.T) {
	if coverage.Resolution != bars.Resolution {
		t.Fatalf("coverage.Resolution = %s but bars.Resolution = %s — a coverage bucket that does "+
			"not line up with a bar bucket answers a question nobody asked",
			coverage.Resolution, bars.Resolution)
	}
}

// A SILENCE TOLERANCE IS REQUIRED, AND ONE THAT SPANS A BUCKET IS REFUSED.
//
// Both directions matter. Zero would credit nothing and report a healthy feed as
// unobserved; a tolerance at or beyond one bucket would credit a whole SILENT
// bucket off one observation on either side of it, which is exactly the claim
// this record exists to refuse.
func TestRecorderRefusesAnUnusableSilenceTolerance(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    time.Duration
	}{
		{"zero", 0},
		{"negative", -time.Second},
		{"one whole bucket", coverage.Resolution},
		{"longer than a bucket", 5 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := coverage.NewRecorder(coverage.Config{
				Publisher: &recordingPub{}, MaxSilence: tc.d,
			})
			if !errors.Is(err, coverage.ErrSilenceTolerance) {
				t.Fatalf("NewRecorder(MaxSilence=%s) err = %v, want ErrSilenceTolerance", tc.d, err)
			}
		})
	}
}

// A SERIES NOBODY EVER HEARD FROM PRODUCES NO RECORD AT ALL.
//
// This is the whole deliverable in one assertion: "no coverage record" and
// "covered, and the market was quiet" must never look the same. Registering a
// subscription is not attesting to it — a feed that never connected must leave
// the interval UNKNOWN, which downstream is the absence of a row, not a row
// saying zero.
func TestRegisteringASubscriptionAttestsNothing(t *testing.T) {
	pub := &recordingPub{}
	rec := newRec(t, pub, 45*time.Second)
	rec.Observe(series, "binance:trades:BTCUSDT")

	rec.Flush(context.Background(), base.Add(10*time.Minute))

	if got := pub.records(); len(got) != 0 {
		t.Fatalf("a subscription that was never heard from published %d attestation(s); it must "+
			"publish none, so the interval reads as UNKNOWN rather than as observed-and-quiet", len(got))
	}
}

// A QUIET MINUTE WITH A LIVE HEARTBEAT IS FULLY COVERED.
//
// This is the case the bar series cannot express and the reason the whole record
// exists: no trade in the minute, and yet the platform can say it was looking
// for all sixty seconds.
func TestHeartbeatsAloneCoverAQuietMinute(t *testing.T) {
	pub := &recordingPub{}
	rec := newRec(t, pub, 45*time.Second)
	obs := rec.Observe(series, "binance:trades:BTCUSDT")

	// First contact one bucket EARLY, so the bucket under test is entered with a
	// live subscription rather than opened by its first observation.
	for at := base.Add(-time.Minute); !at.After(base.Add(time.Minute)); at = at.Add(20 * time.Second) {
		obs.Live(at)
	}

	var target *marketpb.IngestionCoverage
	for _, r := range pub.records() {
		if r.GetBucketStart().AsTime().Equal(base) {
			target = r
		}
	}
	if target == nil {
		t.Fatal("no attestation for the quiet minute — with heartbeats landing throughout it, the " +
			"platform demonstrably WAS observing and must be able to say so")
	}
	if got := target.GetObserved().AsDuration(); got != time.Minute {
		t.Fatalf("observed = %s, want 1m: heartbeats spanned the whole bucket, so every second of "+
			"it was proven and a missing bar inside it is a QUIET market, not an unknown one", got)
	}
	if got := target.GetBreaks(); got != 0 {
		t.Errorf("breaks = %d, want 0", got)
	}
	if got := target.GetAttestor(); got != "binance:trades:BTCUSDT" {
		t.Errorf("attestor = %q — a claim nothing can be traced to cannot be audited", got)
	}
}

// COVERAGE STARTS AT FIRST CONTACT, NOT AT THE TOP OF THE BUCKET.
//
// The seconds before the first observation are seconds nobody was watching.
// Crediting them would be the first lie in the record, and it would be the
// easiest one to write.
func TestFirstContactDoesNotCreditTheSecondsBeforeIt(t *testing.T) {
	pub := &recordingPub{}
	rec := newRec(t, pub, 45*time.Second)
	obs := rec.Observe(series, "binance:trades:BTCUSDT")

	// First heartbeat 30s into the bucket, then one every 20s across the boundary.
	obs.Live(base.Add(30 * time.Second))
	obs.Live(base.Add(50 * time.Second))
	obs.Live(base.Add(70 * time.Second))

	got := pub.records()
	if len(got) != 1 {
		t.Fatalf("published %d attestations, want 1", len(got))
	}
	if want := 30 * time.Second; got[0].GetObserved().AsDuration() != want {
		t.Fatalf("observed = %s, want %s — the first 30s of the bucket had no subscription "+
			"reporting, and a record that claimed them would vouch for time nobody watched",
			got[0].GetObserved().AsDuration(), want)
	}
}

// SILENCE BEYOND TOLERANCE IS NOT CREDITED, AND IS NOT AN OUTAGE EITHER.
//
// A gap between observations is the ABSENCE of evidence, not evidence of
// absence. It is left uncredited — so the window refuses to support a claim —
// and it is not counted as a break, because nothing was actually observed to
// fail.
func TestSilenceBeyondToleranceIsNotCredited(t *testing.T) {
	pub := &recordingPub{}
	rec := newRec(t, pub, 45*time.Second)
	obs := rec.Observe(series, "binance:trades:BTCUSDT")

	obs.Live(base.Add(-10 * time.Second)) // opens the previous bucket
	obs.Live(base.Add(10 * time.Second))  // 20s gap: within tolerance
	obs.Live(base.Add(58 * time.Second))  // 48s gap: BEYOND tolerance
	obs.Live(base.Add(70 * time.Second))  // closes the bucket under test

	var target *marketpb.IngestionCoverage
	for _, r := range pub.records() {
		if r.GetBucketStart().AsTime().Equal(base) {
			target = r
		}
	}
	if target == nil {
		t.Fatal("no attestation for the bucket under test")
	}
	// Credited: [base, base+10s] from the in-tolerance step, and
	// [base+58s, base+60s] from the last one. The 48 seconds between are not.
	if want := 12 * time.Second; target.GetObserved().AsDuration() != want {
		t.Fatalf("observed = %s, want %s — the 48s silence must be uncredited, because a socket "+
			"that stopped talking is not a socket anyone can vouch for",
			target.GetObserved().AsDuration(), want)
	}
	if got := target.GetBreaks(); got != 0 {
		t.Errorf("breaks = %d, want 0 — nothing was OBSERVED to fail; a silence is the absence of "+
			"evidence, and recording it as a fault would claim knowledge nobody has", got)
	}
	if target.GetObserved().AsDuration() >= coverage.Resolution {
		t.Error("a bucket with an unexplained silence in it must not read as whole")
	}
}

// A BREAK IS COUNTED, AND THE OUTAGE IT OPENS IS NOT CREDITED WHEN THE FEED
// RETURNS.
func TestABreakStopsCreditAndIsCounted(t *testing.T) {
	pub := &recordingPub{}
	rec := newRec(t, pub, 45*time.Second)
	obs := rec.Observe(series, "binance:trades:BTCUSDT")

	obs.Live(base.Add(-10 * time.Second))
	obs.Live(base.Add(10 * time.Second))
	obs.Down(base.Add(20*time.Second), errors.New("read: connection reset"))
	obs.Live(base.Add(40 * time.Second)) // back, but the 20s hole is nobody's
	obs.Live(base.Add(70 * time.Second))

	var target *marketpb.IngestionCoverage
	for _, r := range pub.records() {
		if r.GetBucketStart().AsTime().Equal(base) {
			target = r
		}
	}
	if target == nil {
		t.Fatal("no attestation for the bucket under test")
	}
	// Credited: [base, base+20s] up to the break, then [base+40s, base+60s].
	if want := 40 * time.Second; target.GetObserved().AsDuration() != want {
		t.Fatalf("observed = %s, want %s — the outage between the break and the recovery belongs "+
			"to nobody", target.GetObserved().AsDuration(), want)
	}
	if got := target.GetBreaks(); got != 1 {
		t.Fatalf("breaks = %d, want 1 — a fault that was actually SEEN is different from a silence, "+
			"and only this number tells them apart", got)
	}
}

// A FEED THAT DIES PUBLISHES ITS BUCKETS ANYWAY, WITH THE COVERAGE IT PROVED.
//
// Without the sweep those buckets would never close, and nothing published is
// exactly what a platform that was never looking also produces. This is the
// difference between "we watched 12 of these 60 seconds and then lost it" and
// silence.
func TestASweepClosesTheBucketsOfADeadFeed(t *testing.T) {
	pub := &recordingPub{}
	rec := newRec(t, pub, 45*time.Second)
	obs := rec.Observe(series, "binance:trades:BTCUSDT")

	obs.Live(base.Add(2 * time.Second))
	obs.Live(base.Add(14 * time.Second))
	// ...and then nothing. The process is up; the socket is not.

	rec.Flush(context.Background(), base.Add(3*time.Minute+30*time.Second))

	got := pub.records()
	if len(got) < 3 {
		t.Fatalf("the sweep published %d attestations, want the 3 elapsed buckets — a bucket left "+
			"open by a dead feed is indistinguishable from a platform that never looked", len(got))
	}
	if want := 12 * time.Second; got[0].GetObserved().AsDuration() != want {
		t.Errorf("first bucket observed = %s, want %s", got[0].GetObserved().AsDuration(), want)
	}
	for _, r := range got[1:] {
		if r.GetObserved().AsDuration() != 0 {
			t.Errorf("bucket %s observed = %s, want 0 — the sweep observes nothing and must credit "+
				"nothing", r.GetBucketStart().AsTime(), r.GetObserved().AsDuration())
		}
	}
	// And the in-progress bucket is NOT published: an interval that has not
	// elapsed cannot be attested, and its absence honestly reads as UNKNOWN.
	inProgress := base.Add(3 * time.Minute)
	for _, r := range got {
		if r.GetBucketStart().AsTime().Equal(inProgress) {
			t.Fatal("the sweep attested a bucket that had not finished — a partial interval " +
				"published as a complete attestation is a claim about time nobody has reached")
		}
	}
}

// AN OBSERVATION IS NEVER CREDITED TWICE, AND A CLOCK THAT WENT BACKWARDS
// CANNOT UN-SETTLE TIME THAT WAS ALREADY DECIDED.
func TestOutOfOrderObservationsDoNotDoubleCredit(t *testing.T) {
	pub := &recordingPub{}
	rec := newRec(t, pub, 45*time.Second)
	obs := rec.Observe(series, "binance:trades:BTCUSDT")

	obs.Live(base.Add(-10 * time.Second))
	obs.Live(base.Add(20 * time.Second))
	obs.Live(base.Add(5 * time.Second)) // backwards
	obs.Live(base.Add(40 * time.Second))
	obs.Live(base.Add(70 * time.Second))

	var target *marketpb.IngestionCoverage
	for _, r := range pub.records() {
		if r.GetBucketStart().AsTime().Equal(base) {
			target = r
		}
	}
	if target == nil {
		t.Fatal("no attestation for the bucket under test")
	}
	if got := target.GetObserved().AsDuration(); got > coverage.Resolution {
		t.Fatalf("observed = %s, which exceeds the %s bucket — an over-claim is the one direction "+
			"this record must never fail in", got, coverage.Resolution)
	}
	if want := time.Minute; target.GetObserved().AsDuration() != want {
		t.Fatalf("observed = %s, want %s", target.GetObserved().AsDuration(), want)
	}
}

// TWO VENUES ARE TWO RECORDS. A live Binance subscription says nothing whatever
// about what OKX was doing, and one record covering both would vouch for a feed
// nobody was holding.
func TestCoverageIsPerVenue(t *testing.T) {
	pub := &recordingPub{}
	rec := newRec(t, pub, 45*time.Second)
	bin := rec.Observe(coverage.Series{InstrumentID: "BTC-USDT", Venue: "XBIN"}, "binance:trades:BTCUSDT")
	rec.Observe(coverage.Series{InstrumentID: "BTC-USDT", Venue: "OKX"}, "okx:trades:BTC-USDT")

	for at := base.Add(-time.Minute); !at.After(base.Add(time.Minute)); at = at.Add(20 * time.Second) {
		bin.Live(at)
	}
	rec.Flush(context.Background(), base.Add(3*time.Minute))

	for _, r := range pub.records() {
		if r.GetMic() == "OKX" {
			t.Fatalf("an OKX attestation was published from a Binance subscription's observations: %v", r)
		}
	}
}
