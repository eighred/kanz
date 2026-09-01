package volprofilefeed_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/marketedge/volprofile"
	"github.com/eighred/kanz/internal/volprofilefeed"
	"github.com/eighred/kanz/pkg/bus"
)

// THE CONSUMER'S REGISTRY (#897): resolve the version a parent order pinned, on
// any pod, for as long as the parent is being worked.

// curve builds a KNOWN profile whose first bucket carries `head` and whose noon
// bucket carries the rest of a `level` session — a shape whose numbers a reader
// can check without running anything.
func curve(t *testing.T, asOf time.Time, head, level int64) *marketpb.VolumeProfile {
	t.Helper()
	n := volprofile.BucketsPerSession(volprofile.DefaultBucket)
	shares := make([]*big.Rat, n)
	for i := range shares {
		shares[i] = new(big.Rat)
	}
	shares[0] = new(big.Rat).SetFrac64(head, level)
	shares[24] = new(big.Rat).SetFrac64(level-head, level)

	pb, err := volprofilefeed.Encode(volprofile.Answer{
		Series:        volprofile.Series{InstrumentID: inst, Venue: venue},
		Bucket:        volprofile.DefaultBucket,
		AsOf:          asOf,
		Verdict:       volprofile.VerdictKnown,
		Sessions:      3,
		Oldest:        asOf.Add(-3 * volprofile.Session),
		Newest:        asOf.Add(-volprofile.Session),
		SessionVolume: new(big.Rat).SetInt64(level),
		Shares:        shares,
	}, volprofile.DefaultHorizon)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return pb
}

func mustDecode(t *testing.T, pb *marketpb.VolumeProfile) volprofilefeed.Profile {
	t.Helper()
	p, err := volprofilefeed.Decode(pb)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return p
}

// A PIN RESOLVES THE CURVE IT NAMES, NOT THE NEWEST ONE.
//
// This is the property the whole design is bought for. A parent admitted against
// version A and worked past a session boundary must keep deriving against A —
// otherwise services/oms/internal/order.authorizeChild re-derives its child's
// quantity from B, compares as an exact rational, and refuses the driver's own
// slice as a forgery.
func TestResolve_ReturnsThePinnedVersionAfterANewerOneArrives(t *testing.T) {
	reg := volprofilefeed.NewRegistry(0)

	old := mustDecode(t, curve(t, day1, 100, 400))
	reg.Fold(old)
	if got, ok := reg.Current(ser); !ok || got.Version != old.Version {
		t.Fatalf("Current = %q (%v), want the only version folded", got.Version, ok)
	}

	fresh := mustDecode(t, curve(t, day2, 300, 400))
	reg.Fold(fresh)
	if old.Version == fresh.Version {
		t.Fatal("two different curves hashed to one version; the fixture is not exercising this")
	}

	// CURRENT MOVED, which is right: admission plans against what is newest.
	if got, ok := reg.Current(ser); !ok || got.Version != fresh.Version {
		t.Fatalf("Current = %q, want the newer %q", got.Version, fresh.Version)
	}
	// AND THE PIN DID NOT.
	got, ok := reg.Resolve(ser, old.Version)
	if !ok {
		t.Fatal("the pinned version stopped resolving the moment a newer curve arrived — every " +
			"parent worked across a session boundary would stop advancing")
	}
	if got.Expected[0].Cmp(rat(100)) != 0 {
		t.Errorf("the pinned curve's first bucket is %s, want 100 — Resolve returned a different "+
			"shape under the pinned name", got.Expected[0].RatString())
	}
}

// AN UNKNOWN VERSION IS UNKNOWN, NEVER THE NEAREST CURVE.
//
// A nearest match would keep the order working against a market it was never
// sized for, and every fill would be attributed to a schedule that was never
// derived. Refusing is what makes the driver report an unworkable schedule
// instead.
func TestResolve_RefusesAVersionItDoesNotHold(t *testing.T) {
	reg := volprofilefeed.NewRegistry(0)
	reg.Fold(mustDecode(t, curve(t, day2, 100, 400)))

	for _, v := range []string{"", "deadbeefdeadbeefdeadbeefdeadbeef"} {
		if p, ok := reg.Resolve(ser, v); ok {
			t.Fatalf("version %q resolved to %q — a pin this registry never received must be "+
				"UNKNOWN, not the closest thing it has", v, p.Version)
		}
	}
	if _, ok := reg.Resolve(volprofilefeed.Series{InstrumentID: "ETH-USDT", Venue: venue},
		mustDecode(t, curve(t, day2, 100, 400)).Version); ok {
		t.Fatal("a version resolved on a series it was not measured for — a schedule would be " +
			"sized from another instrument's curve")
	}
}

// A VERSION PAST THE RETENTION HORIZON STOPS RESOLVING, AND THE SERIES IS
// EVICTED WITH IT.
//
// The registry is fed off the wire, so its key space is not bounded by anything
// this process configured: a producer that published a typo'd instrument once
// would otherwise leave an entry behind for the life of the pod.
func TestFold_BoundsBothTheVersionsAndTheSeries(t *testing.T) {
	reg := volprofilefeed.NewRegistry(2 * volprofile.Session)

	first := mustDecode(t, curve(t, day0, 100, 400))
	reg.Fold(first)
	if reg.Versions(ser) != 1 {
		t.Fatalf("versions = %d, want 1", reg.Versions(ser))
	}

	// Two sessions later, the first is exactly at the horizon; three later it is
	// past it.
	reg.Fold(mustDecode(t, curve(t, day0.Add(3*volprofile.Session), 300, 400)))
	if _, ok := reg.Resolve(ser, first.Version); ok {
		t.Fatal("a version older than the retention horizon still resolves — the horizon bounds " +
			"len and not the heap, which is #862 in a new store")
	}
	if reg.Versions(ser) != 1 {
		t.Errorf("versions = %d after the horizon dropped one, want 1", reg.Versions(ser))
	}
	_, _, evicted, series := reg.Stats()
	if evicted != 1 {
		t.Errorf("evicted = %d, want 1", evicted)
	}
	if series != 1 {
		t.Errorf("series = %d, want 1", series)
	}
}

// A REPLAY THAT REDELIVERS THE SAME MESSAGE REBUILDS THE SAME REGISTRY.
//
// SubscribeReplay folds a subject's whole retained history on every start, so a
// fold that appended rather than replaced would give a pod that restarted twice a
// version list three times as long as the pod beside it — and Current would then
// depend on how many times a process had booted.
func TestFold_IsIdempotent(t *testing.T) {
	reg := volprofilefeed.NewRegistry(0)
	p := mustDecode(t, curve(t, day2, 100, 400))
	for range 4 {
		reg.Fold(p)
	}
	if got := reg.Versions(ser); got != 1 {
		t.Fatalf("versions = %d after folding one message four times, want 1", got)
	}
}

// THE REGISTRY HANDS OUT THE CALLER'S OWN CURVE.
//
// *big.Rat is a pointer. A registry returning its own would let one caller's
// arithmetic silently rewrite the shape every later caller schedules against —
// and the two pods this design keeps in agreement would disagree with nothing in
// any log to say why.
func TestResolve_HandsOutACopy(t *testing.T) {
	reg := volprofilefeed.NewRegistry(0)
	p := mustDecode(t, curve(t, day2, 100, 400))
	reg.Fold(p)

	first, _ := reg.Resolve(ser, p.Version)
	first.Expected[0].SetInt64(999999)

	second, _ := reg.Resolve(ser, p.Version)
	if second.Expected[0].Cmp(rat(100)) != 0 {
		t.Fatalf("bucket 0 is now %s — one reader's mutation reached the registry",
			second.Expected[0].RatString())
	}
}

// ===== THE BUS HANDLER =====

// A MESSAGE THIS BUILD CANNOT READ IS DROPPED AND COUNTED, NEVER NACKED.
//
// The replay class has an unbounded MaxDeliver, so returning an error would
// redeliver one bad message forever while every message behind it went unread —
// and a registry stuck three entries into a replay reports "no profile" with
// exactly the confidence of one that folded all of them.
func TestHandle_DropsWhatItCannotReadAndCountsIt(t *testing.T) {
	reg := volprofilefeed.NewRegistry(0)
	env := &envelopepb.Envelope{TenantId: "__system__"}

	good := curve(t, day2, 100, 400)
	goodBytes, err := proto.Marshal(good)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	tampered := proto.Clone(good).(*marketpb.VolumeProfile)
	tampered.Version = "not-the-hash-of-this-content"
	tamperedBytes, err := proto.Marshal(tampered)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	for _, payload := range [][]byte{[]byte("not a protobuf at all\xff\xff"), tamperedBytes, goodBytes} {
		if err := reg.Handle(context.Background(), env, payload); err != nil {
			t.Fatalf("Handle nacked a message: %v — the fold would stall behind it forever", err)
		}
	}

	folded, refused, _, series := reg.Stats()
	if folded != 1 || refused != 2 {
		t.Errorf("folded = %d, refused = %d; want 1 and 2 — the refusal count is the ONLY thing "+
			"separating a quiet feed from a producer this build cannot read", folded, refused)
	}
	if series != 1 {
		t.Errorf("series = %d, want 1", series)
	}
	if _, ok := reg.Resolve(ser, good.GetVersion()); !ok {
		t.Error("the message that WAS readable did not reach the registry")
	}
}

// THE HANDLER IS A bus.EventHandler, asserted at build time: a signature drift
// would leave the subscription in the composition root failing to compile rather
// than the fold silently unwired.
var _ bus.EventHandler = (&volprofilefeed.Registry{}).Handle
