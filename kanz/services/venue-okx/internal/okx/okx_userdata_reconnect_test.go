package okx

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

// endlessFills pushes an orders-channel fill on every Recv. THE VENUE IS HEALTHY
// — it is the store that is down, so Connect succeeds every time.
type endlessFills struct{}

func (endlessFills) Recv(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []byte(`{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","ordId":"312","clOrdId":"o1",` +
		`"state":"filled","fillSz":"1","fillPx":"50000","accFillSz":"1","tradeId":"7",` +
		`"fillFee":"-0.05","fillFeeCcy":"USDT","uTime":"1700000000000"}]}`), nil
}

// A STORE OUTAGE MUST NOT BECOME AN EXCHANGE BAN (#1047).
//
// The identical exposure, one connector over — which is why the policy lives in
// execution.UserDataBackoff and not in either loop. Both backed off only on a
// failed Connect and reset the delay on every successful one, so an unreadable
// order view behind a healthy exchange produced an unbounded re-dial loop. OKX
// rate-limits and then bans the account for it, which stops every order on it
// over a database blip that would otherwise have healed itself.
func TestOKXUserDataReconnect_IsRateLimitedWhileTheOrderViewIsDown(t *testing.T) {
	var dials atomic.Int64
	c := &OKXConnector{settings: VenueSettings{MIC: "OKX"}}
	c.dialUserData = func(context.Context) (UserDataStream, func(), error) {
		dials.Add(1)
		return endlessFills{}, func() {}, nil
	}
	view := orderview.NewSeam(&blindStore{Memory: orderview.NewMemory()}, func(error) {})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.runUserData(ctx, WorkerDeps{
			Orders: view, Publisher: &okxCapture{},
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		close(done)
	}()

	const window = 300 * time.Millisecond
	time.Sleep(window)
	cancel()
	<-done

	got := dials.Load()
	if got == 0 {
		t.Fatal("the run loop never dialled — this test would pass against any back-off, " +
			"including one that never reconnects at all")
	}
	const maxDials = 3
	if got > maxDials {
		t.Fatalf("%d exchange dials in %v while the order view was unreadable — the loop is "+
			"re-dialling a healthy exchange at full speed because the store behind it is down. "+
			"OKX rate-limits and then bans this account for it", got, window)
	}
	t.Logf("%d dial(s) in %v (bound %d)", got, window, maxDials)
}

// THE RESET CONDITION, pinned on this connector too: publishing a fill is
// progress, failing to read the view is not. Resetting on a successful CONNECT —
// what the loop did before — is what made the bound above unreachable.
func TestOKXUserDataReconnect_OnlyAResolvedReportCountsAsProgress(t *testing.T) {
	frame := `{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","ordId":"312","clOrdId":"o1",` +
		`"state":"filled","fillSz":"1","fillPx":"50000","accFillSz":"1","tradeId":"7",` +
		`"fillFee":"-0.05","fillFeeCcy":"USDT","uTime":"1700000000000"}]}`

	healthy := newOKXUserDataIngester(OKXUserDataConfig{
		Stream: &okxStream{frames: [][]byte{[]byte(frame)}}, Orders: newOKXOrders("o1"),
		Pub: &okxCapture{}, Venue: "OKX", Tenant: "fund-alpha",
	})
	_ = healthy.Run(context.Background())
	if !healthy.resolvedAReport() {
		t.Error("a session that published a fill FACT is not counted as progress — the back-off " +
			"would keep growing on a healthy adapter and delay every reconnect by up to the cap")
	}

	blind := newOKXUserDataIngester(OKXUserDataConfig{
		Stream: &okxStream{frames: [][]byte{[]byte(frame)}}, Orders: newUnreadableOrders("o1"),
		Pub: &okxCapture{}, Venue: "OKX", Tenant: "fund-alpha",
	})
	_ = blind.Run(context.Background())
	if blind.resolvedAReport() {
		t.Error("a session that could not read the order view is counted as progress — the " +
			"back-off resets on every attempt and the loop re-dials at full speed for the outage")
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
	c := &OKXConnector{settings: VenueSettings{MIC: "OKX"}}
	c.dialUserData = func(context.Context) (UserDataStream, func(), error) {
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		return endlessFills{}, func() {}, nil
	}
	c.newUserDataBackoff = func() *UserDataBackoff {
		return NewUserDataBackoffWith(50*time.Millisecond, 2*time.Second)
	}
	view := orderview.NewSeam(&blindStore{Memory: orderview.NewMemory()}, func(error) {})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.runUserData(ctx, WorkerDeps{
			Orders: view, Publisher: &okxCapture{},
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
