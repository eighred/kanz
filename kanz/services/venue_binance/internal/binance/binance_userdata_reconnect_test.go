package binance

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/venueadapter/orderview"
)

// blindStore fails every read — a Postgres outage behind a healthy exchange.
type blindStore struct{ *orderview.Memory }

func (s *blindStore) Get(context.Context, string) (*orderpb.OrderState, orderview.Revision, bool, error) {
	return nil, orderview.Revision{}, false, errUnreadableView
}

// endlessFills hands the ingester a fill report on every Recv. THE VENUE IS
// HEALTHY here, which is the entire point of the case: it is the store that is
// down, so Connect succeeds every time and only the ingester fails.
type endlessFills struct{}

func (endlessFills) Recv(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []byte(`{"e":"executionReport","s":"BTCUSDT","c":"o1","S":"BUY","x":"TRADE","X":"FILLED",` +
		`"l":"1","L":"50000","z":"1","q":"1","t":7,"T":1700000000000}`), nil
}

// A STORE OUTAGE MUST NOT BECOME AN EXCHANGE BAN (#1047).
//
// # What this pins, and why it is not the same bug as the drop
//
// Refusing an unresolvable execution report means Run returns an error where it
// used to return nil. The reconnect loop backed off only when Connect FAILED and
// reset the delay on every successful connect — which bounds nothing here,
// because the exchange is fine and Connect keeps succeeding. Every session then
// connected, died on the first report, reset the delay and re-dialled with no
// pause: measured at 38,688 dials in 300ms on this very fixture.
//
// Binance rate-limits and then IP-BANS that. The cost is not a slow reconnect —
// it is a venue-level lockout of the whole account, which stops every order on
// it, caused by a database blip that would otherwise have healed itself. That is
// strictly worse than the silent drop the refusal was introduced to fix, so the
// refusal is only correct with this bound beside it.
//
// # Why a wall-clock window rather than a fake clock
//
// The property is a RATE, and the defect was that no delay was consulted at all.
// A window measures the thing itself: with the base delay at one second, a loop
// that backs off cannot dial more than twice in 300ms, and a loop that does not
// dials tens of thousands of times. No clock seam can be wired wrongly in a way
// that makes this pass.
func TestUserDataReconnect_IsRateLimitedWhileTheOrderViewIsDown(t *testing.T) {
	var dials atomic.Int64
	c := &BinanceConnector{settings: VenueSettings{MIC: "BINANCE"}}
	c.dialUserData = func(context.Context) (UserDataStream, func(), error) {
		dials.Add(1)
		return endlessFills{}, func() {}, nil
	}
	view := orderview.NewSeam(&blindStore{Memory: orderview.NewMemory()}, func(error) {})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.runUserData(ctx, WorkerDeps{
			Orders: view, Publisher: &reconCapture{},
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		close(done)
	}()

	const window = 300 * time.Millisecond
	time.Sleep(window)
	cancel()
	<-done

	got := dials.Load()
	// NON-VACUITY: the loop must actually have run, or the bound below is a
	// statement about a goroutine that never started.
	if got == 0 {
		t.Fatal("the run loop never dialled — this test would pass against any back-off, " +
			"including one that never reconnects at all")
	}
	// THE BOUND. Base delay is one second, so at most the opening dial plus one
	// more can land inside the window; 3 leaves room for a slow box without
	// admitting anything resembling the 38,688 the unbounded loop managed.
	const maxDials = 3
	if got > maxDials {
		t.Fatalf("%d exchange dials in %v while the order view was unreadable — the loop is "+
			"re-dialling a healthy exchange at full speed because the store behind it is down. "+
			"Binance rate-limits and then IP-bans this, which locks the whole account out of "+
			"trading over a database blip", got, window)
	}
	t.Logf("%d dial(s) in %v (bound %d)", got, window, maxDials)
}

// THE RESET CONDITION ITSELF, which is what makes the back-off both safe and
// non-punitive. A session that resolved a report has proved the whole path
// works and gets the base delay back; a session killed by an unreadable view has
// proved nothing and must not. Resetting on a successful CONNECT — what the loop
// did before — is what made the bound above unreachable.
func TestUserDataReconnect_OnlyAResolvedReportCountsAsProgress(t *testing.T) {
	report := `{"e":"executionReport","s":"BTCUSDT","c":"o1","S":"BUY","x":"TRADE","X":"FILLED",` +
		`"l":"1","L":"50000","z":"1","q":"1","t":7,"T":1700000000000}`

	healthy := ingesterOverView([][]byte{[]byte(report)}, &reconCapture{}, newFakeOrders("o1"))
	_ = healthy.Run(context.Background())
	if !healthy.resolvedAReport() {
		t.Error("a session that published a fill FACT is not counted as progress — the back-off " +
			"would keep growing on a perfectly healthy adapter and delay every reconnect by up " +
			"to the cap, losing fills the stream would otherwise have delivered")
	}

	blind := newUserDataIngester(UserDataConfig{
		Stream: &fakeStream{frames: [][]byte{[]byte(report)}},
		Orders: newUnreadableOrders("o1"), Pub: &reconCapture{},
		Venue: "BINANCE", Tenant: "fund-alpha",
	})
	_ = blind.Run(context.Background())
	if blind.resolvedAReport() {
		t.Error("a session that could not read the order view is counted as progress — the " +
			"back-off resets on every attempt and the loop re-dials the exchange at full speed " +
			"for the whole outage")
	}
}

// THE DELAY MUST GROW, NOT MERELY EXIST (#1047).
//
// # Why this test exists, in its own right
//
// The window bound above was written first and a mutation SURVIVED it: replacing
// the progress-conditional reset with an unconditional one — which is the very
// defect, restored — still produced one dial in 300ms, because a delay pinned at
// the one-second base is quiet inside a 300ms window too. The bound proved a
// floor and said nothing about the curve.
//
// One dial per second is not quiet over an outage. It is 3,600 an hour against a
// venue whose connection limit is counted in hundreds per five minutes, so a
// pinned base reaches the same rate-limit and the same ban by a slower road. The
// property that actually protects the account is that consecutive failures cost
// consecutively more, and it is asserted here on the real run loop.
//
// # Why the bounds are injected
//
// On the production bounds the third gap is four seconds and the test would
// spend eight seconds proving one inequality. The policy seam takes explicit
// bounds so the same curve is observable in milliseconds — the loop under test
// is the production loop; only the constants differ.
func TestUserDataReconnect_TheDelayGrowsWhileNothingIsResolved(t *testing.T) {
	var mu sync.Mutex
	var at []time.Time
	c := &BinanceConnector{settings: VenueSettings{MIC: "BINANCE"}}
	c.dialUserData = func(context.Context) (UserDataStream, func(), error) {
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		return endlessFills{}, func() {}, nil
	}
	c.newUserDataBackoff = func() *UserDataBackoff {
		return newUserDataBackoffWith(50*time.Millisecond, 2*time.Second)
	}
	view := orderview.NewSeam(&blindStore{Memory: orderview.NewMemory()}, func(error) {})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.runUserData(ctx, WorkerDeps{
			Orders: view, Publisher: &reconCapture{},
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		close(done)
	}()
	time.Sleep(800 * time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	stamps := append([]time.Time(nil), at...)
	mu.Unlock()

	if len(stamps) < 4 {
		t.Fatalf("only %d dial(s) in 800ms with a 50ms base — too few to say anything about the "+
			"curve; the loop stalled or never ran", len(stamps))
	}
	var gaps []time.Duration
	for i := 1; i < len(stamps); i++ {
		gaps = append(gaps, stamps[i].Sub(stamps[i-1]))
	}
	// The first gap is one base delay; by the third the delay has doubled twice.
	// 2x rather than 4x leaves room for scheduler noise while still refusing a
	// delay that never grows — the shape the surviving mutation had.
	if gaps[2] < 2*gaps[0] {
		t.Fatalf("re-dial gaps %v do not grow while nothing is being resolved — the delay is "+
			"pinned near its base, so a sustained order-view outage re-dials the exchange at a "+
			"constant rate until it rate-limits and bans this account", gaps)
	}
	t.Logf("re-dial gaps: %v", gaps)
}
