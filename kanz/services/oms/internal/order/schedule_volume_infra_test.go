package order

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/types/known/timestamppb"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/marketedge/trades"
	"github.com/eighred/kanz/internal/marketedge/volprofile"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/internal/volprofilefeed"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/schedule"
)

// #897's "VERIFIED WHEN", AGAINST REAL INFRASTRUCTURE.
//
// # Why the in-process two-pod test is not enough
//
// TestScheduleE2E_ASecondPodDerivesTheIdenticalSchedule proves the ARITHMETIC:
// given two registries holding different things, a pinned derivation agrees. It
// hands both registries their profiles directly, so it proves nothing about the
// transport — and the transport is the whole of #897. CLAUDE.md names the limit:
// "fakeBus does not validate envelopes, so it accepts what a real broker rejects.
// A green suite using it is not a broker proof."
//
// What this adds, and each of these has been the actual failure at least once in
// this repository:
//
//	the envelope    a real broker runs bus.Validate; a profile published without
//	                a tenant lands nowhere while the fold keeps running.
//	the subject     a subject no stream carries is a HARD publish failure, not
//	                fire-and-forget.
//	the replay      a cold registry is rebuilt from the stream's retained
//	                history, which is the property that makes a pin survive a
//	                pod roll — and SubscribeReplay rather than SubscribeBroadcast
//	                is what makes it fold more than one series.
//	the database    two pods share an order store, not a process. The pinned
//	                version has to survive proto marshalling into a BYTEA column
//	                and come back on the other pod.
//
// # Gated, and on BOTH
//
//	go install github.com/nats-io/nats-server/v2@latest
//	nats-server -js -sd <dir>
//	TEST_NATS_URL=nats://localhost:4222 \
//	TEST_POSTGRES_URL=postgres://... \
//	  go test -p 1 ./services/oms/internal/order/ -run TestVolumeProfileReachesASecondPod
//
// # IT MUST PASS ON A DIRTY BROKER
//
// The subject rides the retained MARKET stream, so a broker reused across runs
// hands this test every profile every previous run published. Nothing here counts
// messages; every assertion is scoped to an instrument id unique to this run, and
// the registries are expected to hold other series. A counting assertion on a
// shared retained stream passes once and fails forever after, which is a defect
// this repository has already paid for.

func infraGate(t *testing.T) (string, string) {
	t.Helper()
	nurl := os.Getenv("TEST_NATS_URL")
	if nurl == "" {
		t.Skip("set TEST_NATS_URL to run the volume-profile transport test")
	}
	if os.Getenv("TEST_POSTGRES_URL") == "" {
		t.Skip("set TEST_POSTGRES_URL to run the volume-profile transport test")
	}
	return nurl, os.Getenv("TEST_POSTGRES_URL")
}

// liveRegistry folds the subject's retained history into its own registry over
// its own connection, and returns once the backlog that existed at subscribe time
// has been folded.
//
// A SEPARATE CONNECTION AND A SEPARATE CONSUMER PER POD, which is what two
// replicas actually are. Sharing one would prove that one fold reaches two
// readers, which is not the claim.
func liveRegistry(t *testing.T, ctx context.Context, url, name string) *volprofilefeed.Registry {
	t.Helper()
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: name})
	if err != nil {
		t.Fatalf("%s dial: %v", name, err)
	}
	t.Cleanup(func() { _ = client.Close() })

	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatalf("%s consumer: %v", name, err)
	}
	reg := volprofilefeed.NewRegistry(0)
	armed := make(chan struct{})
	go func() {
		_ = consumer.SubscribeReplay(ctx, volprofilefeed.Subject, reg.Handle, func() { close(armed) })
	}()
	select {
	case <-armed:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s never finished folding the retained volume-profile history", name)
	}
	return reg
}

// awaitVersion waits until a registry can resolve a version, which is what a
// replica's convergence actually looks like.
func awaitVersion(t *testing.T, reg *volprofilefeed.Registry, s volprofilefeed.Series, version, who string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := reg.Resolve(s, version); ok {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	folded, refused, _, series := reg.Stats()
	t.Fatalf("%s never received profile version %s for %s (folded=%d refused=%d series=%d) — "+
		"a refused count above zero means the producer is speaking and this build cannot read it",
		who, version, s, folded, refused, series)
}

// TestVolumeProfileReachesASecondPod is #897 end to end over the real spine.
func TestVolumeProfileReachesASecondPod(t *testing.T) {
	nurl, _ := infraGate(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// THE SUBJECT MUST BE CARRIED BY A STREAM, and on a CI broker it already is
	// (MARKET carries market.>). EnsureSubjects reuses that rather than creating a
	// second stream on the same subject space, which a broker refuses outright.
	nc, err := nats.Connect(nurl)
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	bustest.EnsureSubjects(t, ctx, js, "VOLPROFILE_IT", []string{volprofilefeed.Subject})

	// A PER-RUN INSTRUMENT, so every assertion below is scoped to messages this
	// run published. The broker is expected to be dirty.
	instrument := fmt.Sprintf("VPIT-%d", time.Now().UnixNano())
	series := volprofilefeed.Series{InstrumentID: instrument, Venue: profileVenue}

	// ===== THE PRODUCER: a real market-data edge fold, on a real producer =====
	pubClient, err := bus.DialNATS(ctx, bus.NATSConfig{URL: nurl, Name: "volprofile-it-producer"})
	if err != nil {
		t.Fatalf("producer dial: %v", err)
	}
	defer func() { _ = pubClient.Close() }()
	producer, err := bus.NewProducer(pubClient, bus.ProducerConfig{
		Source: "market-ingest", ProducerVersion: "it", Tenant: testTenant,
	})
	if err != nil {
		t.Fatalf("producer: %v", err)
	}
	collector, err := volprofilefeed.NewCollector(volprofilefeed.Config{
		Publisher: producer, Tenant: testTenant, MinSessions: 2,
	})
	if err != nil {
		t.Fatalf("collector: %v", err)
	}

	// TWO SESSIONS OF REAL PRINTS, folded by the real fold: 1/4 of each session's
	// volume in the first half-hour and 3/4 at noon. Nothing constructs an Answer
	// by hand, so the curve on the wire is one the fold actually measured.
	day := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	for _, start := range []time.Time{day.Add(-2 * volprofile.Session), day.Add(-volprofile.Session)} {
		collector.Observe(series, trades.Trade{
			Price: big.NewRat(1, 1), Size: big.NewRat(100, 1), EventTime: start.Add(10 * time.Minute)})
		collector.Observe(series, trades.Trade{
			Price: big.NewRat(1, 1), Size: big.NewRat(300, 1), EventTime: start.Add(12*time.Hour + 10*time.Minute)})
	}
	collector.Sweep(ctx, day, []volprofilefeed.Series{series})
	if pubs, _, refused, failed := collector.Stats(); pubs != 1 || refused != 0 || failed != 0 {
		t.Fatalf("the edge published %d profiles (refused %d, failed %d) — a real broker refused "+
			"the envelope this fold produced", pubs, refused, failed)
	}

	// ===== TWO PODS, EACH FOLDING THE SPINE INDEPENDENTLY =====
	regA := liveRegistry(t, ctx, nurl, "oms-pod-a")
	regB := liveRegistry(t, ctx, nurl, "oms-pod-b")

	// The version both pods must converge on. Taken from pod A's own fold rather
	// than recomputed here, so this test cannot agree with itself about a curve
	// the wire never carried.
	awaitVersion(t, regA, series, currentVersion(t, regA, series), "pod A")
	pinned := currentVersion(t, regA, series)
	awaitVersion(t, regB, series, pinned, "pod B")

	// ===== ONE ORDER STORE, TWO SERVICES =====
	pool := newPool(t)
	store := NewPostgres(pool)
	at := day.Add(-time.Hour)
	newPod := func(reg *volprofilefeed.Registry) *Service {
		svc, err := NewService(testTenant, store, NewEmitter(&fakeBus{}), nil,
			execution.NewRouter([]execution.Venue{execution.NewSimVenue(profileVenue)}), nil, nil,
			WithHaltGate(halt.OpenGate(nil)), WithVolumeProfiles(reg))
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		svc.now = func() time.Time { return at }
		return svc
	}
	podA, podB := newPod(regA), newPod(regB)

	// POD A ADMITS a VWAP parent against the curve that reached it over the bus.
	orderID := "VPIT" + fmt.Sprint(time.Now().UnixNano())
	cmd := vwapOrder(orderID, orderpb.ExecutionAlgo_EXECUTION_ALGO_VWAP, profileVenue)
	cmd.InstrumentId = instrument
	cmd.Metadata.TargetId = orderID
	cmd.ExecutionSchedule.WindowStart = timestamppb.New(day)
	cmd.ExecutionSchedule.WindowEnd = timestamppb.New(day.Add(volprofile.Session))
	if err := podA.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	st, _, err := store.Load(context.Background(), orderID)
	if err != nil {
		t.Fatalf("pod A did not admit the VWAP parent (it is refused unless a measured curve "+
			"reached it over the bus): %v", err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_WORKING_SCHEDULED {
		t.Fatalf("status = %v, want WORKING_SCHEDULED", st.GetStatus())
	}
	if got := st.GetExecutionSchedule().GetVolumeProfileVersion(); got != pinned {
		t.Fatalf("the order came back from Postgres pinned to %q, want %q", got, pinned)
	}

	// ===== THE MARKET MOVES UNDER POD B =====
	//
	// A third session, entirely in the morning, which inverts the shape. Pod B
	// folds it from the bus, so its CURRENT curve is no longer the pinned one —
	// which is the state a driver reading "newest" would derive the wrong parent
	// from.
	collector.Observe(series, trades.Trade{
		Price: big.NewRat(1, 1), Size: big.NewRat(4000, 1), EventTime: day.Add(20 * time.Minute)})
	collector.Sweep(ctx, day.Add(volprofile.Session), []volprofilefeed.Series{series})
	moved := waitForNewCurrent(t, regB, series, pinned)
	if _, ok := regB.Resolve(series, pinned); !ok {
		t.Fatal("pod B dropped the pinned version when a newer one arrived")
	}

	// ===== POD B DRIVES, AND DERIVES THE PINNED SCHEDULE =====
	at = day.Add(volprofile.Session)
	n, err := podB.DriveSchedules(driveCtx())
	if err != nil {
		t.Fatalf("pod B could not advance a parent pod A admitted: %v", err)
	}
	if n != 2 {
		t.Fatalf("pod B created %d children, want 2", n)
	}
	children, err := store.ListByParent(context.Background(), orderID)
	if err != nil {
		t.Fatalf("ListByParent: %v", err)
	}
	want := map[string]int64{
		schedule.ChildID(orderID, 0): 15, // 1/4 of a 60-unit parent, from the PINNED curve
		schedule.ChildID(orderID, 1): 45,
	}
	for _, c := range children {
		w, ok := want[c.GetOrderId()]
		if !ok {
			t.Fatalf("unexpected child %s", c.GetOrderId())
		}
		got := dec.FromProto(c.GetOrderedQuantity())
		if got.Cmp(new(big.Rat).SetInt64(w)) != 0 {
			t.Fatalf("pod B worked child %s as %s, want %d — it derived against its own CURRENT "+
				"curve (%s) rather than the version the order names (%s)",
				c.GetOrderId(), got.RatString(), w, moved, pinned)
		}
	}

	// AND POD A AUTHORIZES POD B's CHILD, which is the check that breaks in
	// production when two pods disagree: the quantity is compared as an exact
	// rational, so a child sized against a different curve is refused as a forgery.
	child := findChild(t, children, schedule.ChildID(orderID, 0))
	childCmd := submitFromState(child, nil)
	childCmd.OrderId = child.GetOrderId()
	childCmd.ParentOrderId = orderID
	childCmd.Quantity = child.GetOrderedQuantity()
	childCmd.ExecutionSchedule = nil
	if _, rej, err := podA.authorizeChild(context.Background(), childCmd); err != nil || rej != nil {
		t.Fatalf("pod A refused a child pod B derived from the same order: rej=%v err=%v", rej, err)
	}
}

func currentVersion(t *testing.T, reg *volprofilefeed.Registry, s volprofilefeed.Series) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if p, ok := reg.Current(s); ok {
			return p.Version
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("no profile for %s ever reached this registry: %s", s, reg)
	return ""
}

// waitForNewCurrent waits until the registry's newest version for a series is no
// longer `old`, and returns the new one.
func waitForNewCurrent(t *testing.T, reg *volprofilefeed.Registry, s volprofilefeed.Series, old string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if p, ok := reg.Current(s); ok && p.Version != old {
			return p.Version
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("the moved curve never reached this pod, so this test would prove nothing about "+
		"deriving against a pin while the market moves: %s", reg)
	return ""
}
