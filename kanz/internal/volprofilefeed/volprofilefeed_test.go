package volprofilefeed_test

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/marketedge/trades"
	"github.com/eighred/kanz/internal/marketedge/volprofile"
	"github.com/eighred/kanz/internal/volprofilefeed"
	"github.com/eighred/kanz/pkg/bus"
)

// THE VOLUME PROFILE AS A PUBLISHED FACT (#897).
//
// AN EXTERNAL TEST PACKAGE ON PURPOSE, exactly as internal/execution/marketview's
// suite is: everything below goes through the surface a market-ingest and an OMS
// actually use, so nothing here can assert a property a caller could not rely on.
//
// THE FIXTURE IS THE SAME ONE #869 USED, and the sameness is deliberate — two
// identical sessions in which 100 units print in the first half-hour and 300 in
// the half-hour starting at noon, so the shape is 1/4 in bucket 0, 3/4 in bucket
// 24 and zero everywhere else, and the mean session volume is 400. Every expected
// number below is a hand-evaluated product of those two, which means the wire
// mapping can be checked against arithmetic rather than against itself.

const venue = "XBIT"

var (
	day0 = time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	day1 = day0.Add(volprofile.Session)
	day2 = day1.Add(volprofile.Session)
	inst = "BTC-USDT"
	ser  = volprofilefeed.Series{InstrumentID: inst, Venue: venue}
)

func rat(i int64) *big.Rat { return new(big.Rat).SetInt64(i) }

func trade(at time.Time, size int64) trades.Trade {
	return trades.Trade{Price: rat(1), Size: rat(size), EventTime: at}
}

// foldTwoSessions builds the fixture inside a Collector's own store, so the fold
// under test is the one a market-ingest would run.
func foldTwoSessions(t *testing.T, c *volprofilefeed.Collector) {
	t.Helper()
	for _, start := range []time.Time{day0, day1} {
		c.Observe(ser, trade(start.Add(10*time.Minute), 100))
		c.Observe(ser, trade(start.Add(12*time.Hour+10*time.Minute), 300))
	}
}

// ===== THE PRODUCER: A REAL bus.Producer, A REAL ENVELOPE =====

// captureClient records every framed message a real producer emits.
type captureClient struct{ sent []bus.Message }

func (c *captureClient) Publish(_ context.Context, m bus.Message) error {
	c.sent = append(c.sent, m)
	return nil
}
func (c *captureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *captureClient) Close() error { return nil }

// newCollector builds the collector over a REAL bus.Producer.
//
// TIER B, AND THE TIER IS THE POINT. A double that accepts a bus.Event without
// running bus.Validate proves nothing about what a broker will take — the failure
// pkg/bus/producer.go records is an envelope that every in-process test accepted
// and the first real broker rejected, after which the service folded perfectly
// and published nothing.
func newCollector(t *testing.T, minSessions int) (*volprofilefeed.Collector, *captureClient) {
	t.Helper()
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source:          "market-ingest/test",
		ProducerVersion: "test-1.0.0",
		Tenant:          "__system__",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	c, err := volprofilefeed.NewCollector(volprofilefeed.Config{
		Publisher:   prod,
		Tenant:      "__system__",
		MinSessions: minSessions,
	})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	return c, cc
}

func unframe(t *testing.T, m bus.Message) (*envelopepb.Envelope, *marketpb.VolumeProfile) {
	t.Helper()
	env, payload, err := bus.Unframe(m.Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Fatalf("a real broker would REJECT this envelope: %v", err)
	}
	var pb marketpb.VolumeProfile
	if err := proto.Unmarshal(payload, &pb); err != nil {
		t.Fatalf("unmarshal VolumeProfile: %v", err)
	}
	return env, &pb
}

// THE PUBLISHED ENVELOPE IS ONE A REAL BROKER ACCEPTS, and it carries the curve
// the fold measured.
func TestSweep_PublishesAValidEnvelopeCarryingTheMeasuredCurve(t *testing.T) {
	c, cc := newCollector(t, 2)
	foldTwoSessions(t, c)

	c.Sweep(context.Background(), day2, []volprofilefeed.Series{ser})

	if len(cc.sent) != 1 {
		t.Fatalf("messages=%d want 1", len(cc.sent))
	}
	env, pb := unframe(t, cc.sent[0])

	if env.GetEventType() != volprofilefeed.Subject {
		t.Errorf("event type = %q, want %q", env.GetEventType(), volprofilefeed.Subject)
	}
	if env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Errorf("event class = %v — a measured curve is observed, not requested", env.GetEventClass())
	}
	if string(cc.sent[0].Key) != inst {
		t.Errorf("partition key = %q, want the instrument %q — a consumer folding one series must "+
			"read its versions in publication order", string(cc.sent[0].Key), inst)
	}
	if pb.GetVerdict() != marketpb.VolumeProfileVerdict_VOLUME_PROFILE_VERDICT_KNOWN {
		t.Fatalf("verdict = %v, want KNOWN over two folded sessions", pb.GetVerdict())
	}

	// THE CURVE IS THE SHAPE TIMES THE LEVEL, evaluated by hand from the fixture:
	// 1/4 of a mean 400-unit session in bucket 0, 3/4 in the bucket at noon.
	prof, err := volprofilefeed.Decode(pb)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got, want := len(prof.Expected), volprofile.BucketsPerSession(volprofile.DefaultBucket); got != want {
		t.Fatalf("%d buckets, want %d", got, want)
	}
	for i, want := range map[int]int64{0: 100, 24: 300} {
		if prof.Expected[i].Cmp(rat(want)) != 0 {
			t.Errorf("bucket %d expects %s, want %d — the wire is not the shape the fold measured",
				i, prof.Expected[i].RatString(), want)
		}
	}
	total := new(big.Rat)
	for _, e := range prof.Expected {
		total.Add(total, e)
	}
	if total.Cmp(rat(400)) != 0 {
		t.Errorf("the whole session sums to %s, want 400", total.RatString())
	}
}

// A SERIES WITH NO CURVE IS STILL ANNOUNCED, AND SAYS WHY.
//
// This is the distinction the whole three-value model rests on, at the point it
// crosses a process boundary: "nobody is folding this instrument" and "folded,
// and there is not enough of it yet" must not both be silence. A consumer that
// received nothing could not tell them apart, and the refusal a desk reads would
// name the market in both cases.
func TestSweep_AnnouncesASeriesWithNoCurveAndSaysWhy(t *testing.T) {
	c, cc := newCollector(t, 5) // the fixture has two sessions; the desk wants five
	foldTwoSessions(t, c)

	c.Sweep(context.Background(), day2, []volprofilefeed.Series{ser})

	if len(cc.sent) != 1 {
		t.Fatalf("messages=%d want 1 — a series with no curve must still be announced", len(cc.sent))
	}
	_, pb := unframe(t, cc.sent[0])
	if pb.GetVerdict() != marketpb.VolumeProfileVerdict_VOLUME_PROFILE_VERDICT_TOO_FEW_SESSIONS {
		t.Fatalf("verdict = %v, want TOO_FEW_SESSIONS", pb.GetVerdict())
	}
	if len(pb.GetExpectedVolume()) != 0 {
		t.Error("a non-KNOWN profile carries a curve — a reader gating on one field and reading " +
			"the other would schedule against it")
	}
	if pb.GetSessions() != 2 {
		t.Errorf("sessions = %d, want 2 — a refusal that carries no counts cannot tell an "+
			"operator 'nearly ready' from 'nothing here'", pb.GetSessions())
	}

	prof, err := volprofilefeed.Decode(pb)
	if err != nil {
		t.Fatalf("Decode refused a legitimate non-KNOWN profile: %v", err)
	}
	if prof.Known() {
		t.Fatal("a TOO_FEW_SESSIONS profile decoded as KNOWN")
	}
}

// AN UNCHANGED CURVE IS NOT REPUBLISHED.
//
// The version is derived from the content INCLUDING the as-of, so republishing on
// every sweep would put a new version of one curve on the bus every minute — and
// a consumer's retained version list would fill with copies and evict the pins
// still in use. This is the assertion that keeps the pin resolvable.
func TestSweep_DoesNotRepublishAnUnchangedCurve(t *testing.T) {
	c, cc := newCollector(t, 2)
	foldTwoSessions(t, c)

	for i := range 5 {
		c.Sweep(context.Background(), day2.Add(time.Duration(i)*time.Minute), []volprofilefeed.Series{ser})
	}
	if len(cc.sent) != 1 {
		t.Fatalf("messages=%d after five sweeps of one unchanged curve, want 1", len(cc.sent))
	}
	publishes, unchanged, refused, failed := c.Stats()
	if publishes != 1 || unchanged != 4 || refused != 0 || failed != 0 {
		t.Errorf("stats = (publish %d, unchanged %d, refused %d, failed %d), want (1, 4, 0, 0)",
			publishes, unchanged, refused, failed)
	}
}

// A NEW SESSION IS A NEW VERSION. The pin's whole value is that a version names
// one curve, so a curve that has moved must not keep its old name.
func TestSweep_ANewSessionIsANewVersion(t *testing.T) {
	c, cc := newCollector(t, 2)
	foldTwoSessions(t, c)
	c.Sweep(context.Background(), day2, []volprofilefeed.Series{ser})

	// A third session, thin and entirely in the morning, which moves the shape.
	c.Observe(ser, trade(day2.Add(20*time.Minute), 40))
	c.Sweep(context.Background(), day2.Add(volprofile.Session), []volprofilefeed.Series{ser})

	if len(cc.sent) != 2 {
		t.Fatalf("messages=%d, want 2 — a moved curve was not announced", len(cc.sent))
	}
	_, first := unframe(t, cc.sent[0])
	_, second := unframe(t, cc.sent[1])
	if first.GetVersion() == second.GetVersion() {
		t.Fatal("two different curves were published under ONE version — a parent pinned to it " +
			"would resolve whichever copy a pod happened to fold last, which is the divergence " +
			"the pin exists to prevent")
	}
}

// ===== THE VERSION IS RECOMPUTED, NOT TRUSTED =====

// A MESSAGE WHOSE VERSION DOES NOT DESCRIBE ITS OWN CONTENT IS REFUSED.
//
// The version is the handle a parent order pins, so a message carrying somebody
// else's version puts a curve behind a name that addresses a different one — and
// the two pods this design exists to keep in agreement would then agree on the
// version and disagree on the market. Every mutation below is a field that
// changes what a schedule derives.
func TestDecode_RefusesAMessageWhoseVersionDoesNotMatchItsContent(t *testing.T) {
	c, cc := newCollector(t, 2)
	foldTwoSessions(t, c)
	c.Sweep(context.Background(), day2, []volprofilefeed.Series{ser})
	_, good := unframe(t, cc.sent[0])

	for _, tt := range []struct {
		name   string
		break_ func(*marketpb.VolumeProfile)
	}{
		{"a bucket's expected volume", func(p *marketpb.VolumeProfile) {
			p.ExpectedVolume[0] = &commonpb.Decimal{Coefficient: 1, Exponent: 0}
		}},
		{"the verdict", func(p *marketpb.VolumeProfile) {
			p.Verdict = marketpb.VolumeProfileVerdict_VOLUME_PROFILE_VERDICT_STALE
		}},
		{"the instrument", func(p *marketpb.VolumeProfile) { p.InstrumentId = "ETH-USDT" }},
		{"the venue", func(p *marketpb.VolumeProfile) { p.Mic = "XOTHER" }},
		{"the retention horizon", func(p *marketpb.VolumeProfile) {
			p.Horizon.Seconds += 3600
		}},
		{"the version itself", func(p *marketpb.VolumeProfile) { p.Version = "deadbeef" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tampered := proto.Clone(good).(*marketpb.VolumeProfile)
			tt.break_(tampered)
			if _, err := volprofilefeed.Decode(tampered); !errors.Is(err, volprofilefeed.ErrProfile) {
				t.Fatalf("Decode accepted a message whose version addresses a different curve "+
					"(err = %v)", err)
			}
		})
	}

	// AND THE UNTAMPERED MESSAGE DECODES, so the arms above are proving the check
	// rather than a decoder that refuses everything.
	if _, err := volprofilefeed.Decode(good); err != nil {
		t.Fatalf("Decode refused the message the producer actually sent: %v", err)
	}

	// AND THE as-of IS DELIBERATELY OUTSIDE THE IDENTITY, asserted rather than left
	// to be discovered. It is when the producer last asserted the curve, not part of
	// what the curve IS — including it would give every sweep of an unchanged shape
	// a new version and give two replicas sweeping a second apart two versions of
	// one curve, which is the divergence the pin exists to remove. What that costs
	// is stated in versionOf: a tampered as-of can reorder a consumer's retained
	// list, and cannot make a pinned derivation resolve a different shape.
	moved := proto.Clone(good).(*marketpb.VolumeProfile)
	moved.AsOf.Seconds += 60
	if _, err := volprofilefeed.Decode(moved); err != nil {
		t.Fatalf("the as-of is inside the version identity, so an unchanged curve republished a "+
			"minute later is a different version: %v", err)
	}
}

// THE VERSION IS A FUNCTION OF THE CONTENT AND OF NOTHING ELSE, so two processes
// folding the same sessions publish the same version — which is what makes a pin
// resolvable on a pod that never saw the one that took it.
func TestEncode_TwoIndependentFoldsOfTheSameSessionsAgreeOnTheVersion(t *testing.T) {
	a, ca := newCollector(t, 2)
	b, cb := newCollector(t, 2)
	foldTwoSessions(t, a)
	foldTwoSessions(t, b)

	a.Sweep(context.Background(), day2, []volprofilefeed.Series{ser})
	b.Sweep(context.Background(), day2, []volprofilefeed.Series{ser})

	_, pa := unframe(t, ca.sent[0])
	_, pb := unframe(t, cb.sent[0])
	if pa.GetVersion() != pb.GetVersion() {
		t.Fatalf("two folds of the same sessions published versions %q and %q — a pin taken "+
			"against one replica would be unresolvable on the other", pa.GetVersion(), pb.GetVersion())
	}
}

// ===== THE VERDICT VOCABULARIES ARE ONE VOCABULARY =====

// EVERY volprofile.Verdict HAS A WIRE VALUE AND SURVIVES THE ROUND TRIP.
//
// A verdict that lost its identity on the wire would arrive as ABSENT — which
// fails closed, and is therefore the failure that never shows: an operator
// reading "nobody has measured this" about a series that is merely stale would go
// looking for a missing subscription.
func TestVerdictSurvivesTheRoundTrip(t *testing.T) {
	for _, v := range []volprofile.Verdict{
		volprofile.VerdictAbsent, volprofile.VerdictStale,
		volprofile.VerdictTooFewSessions, volprofile.VerdictKnown,
	} {
		t.Run(v.String(), func(t *testing.T) {
			a := volprofile.Answer{
				Series:  volprofile.Series{InstrumentID: inst, Venue: venue},
				Bucket:  volprofile.DefaultBucket,
				AsOf:    day2,
				Verdict: v,
			}
			if v.Known() {
				a.SessionVolume = rat(400)
				a.Shares = make([]*big.Rat, volprofile.BucketsPerSession(volprofile.DefaultBucket))
				for i := range a.Shares {
					a.Shares[i] = new(big.Rat)
				}
				a.Shares[0] = new(big.Rat).SetFrac64(1, 1)
			}
			pb, err := volprofilefeed.Encode(a, volprofile.DefaultHorizon)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			got, err := volprofilefeed.Decode(pb)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got.Verdict != v {
				t.Fatalf("verdict %s became %s across the wire", v, got.Verdict)
			}
		})
	}
}

// THE FEED'S Series IS THE FOLD'S Series, field for field. Two copies of a key
// that drifted would key a registry by something the producer did not.
func TestSeriesMatchesTheFold(t *testing.T) {
	a := volprofilefeed.Series{InstrumentID: "X", Venue: "Y"}
	b := volprofile.Series{InstrumentID: "X", Venue: "Y"}
	if a.String() != b.String() {
		t.Fatalf("%q != %q", a.String(), b.String())
	}
}

// ===== A COLLECTOR THAT COULD PUBLISH NOTHING IS REFUSED AT CONSTRUCTION =====

// EVERY REFUSAL IS AT WIRING TIME, because a collector missing any of these folds
// perfectly and announces nothing — which reads downstream as an instrument
// nobody trades, not as a misconfiguration.
func TestNewCollector_RefusesAWiringThatCouldNeverPublish(t *testing.T) {
	prod, err := bus.NewProducer(&captureClient{}, bus.ProducerConfig{
		Source: "t", ProducerVersion: "v", Tenant: "__system__",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	for _, tt := range []struct {
		name string
		cfg  volprofilefeed.Config
	}{
		{"no publisher", volprofilefeed.Config{Tenant: "t", MinSessions: 2}},
		{"no tenant", volprofilefeed.Config{Publisher: prod, MinSessions: 2}},
		{"no session floor", volprofilefeed.Config{Publisher: prod, Tenant: "t"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := volprofilefeed.NewCollector(tt.cfg); err == nil {
				t.Fatal("a collector that could never publish anything was built")
			}
		})
	}
}

// A PROFILE THAT CANNOT BE PUT ON THE WIRE IS REFUSED WHOLE, never published with
// a hole: a curve missing one bin says nothing trades in it, which sizes every
// other slice wrongly and refuses any slice landing there.
func TestEncode_RefusesAKnownAnswerThatIsNotAWholeCurve(t *testing.T) {
	a := volprofile.Answer{
		Series:        volprofile.Series{InstrumentID: inst, Venue: venue},
		Bucket:        volprofile.DefaultBucket,
		AsOf:          day2,
		Verdict:       volprofile.VerdictKnown,
		SessionVolume: rat(400),
		Shares:        []*big.Rat{new(big.Rat).SetFrac64(1, 1)}, // one bucket of forty-eight
	}
	_, err := volprofilefeed.Encode(a, volprofile.DefaultHorizon)
	if !errors.Is(err, volprofilefeed.ErrProfile) {
		t.Fatalf("err = %v, want ErrProfile", err)
	}
	if !strings.Contains(err.Error(), "KNOWN") {
		t.Errorf("the refusal %q does not say the verdict asserted a shape that is not there", err)
	}
}
