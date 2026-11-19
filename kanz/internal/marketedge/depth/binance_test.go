package depth

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/eighred/kanz/internal/dec"
)

func TestBinanceSourceRefusesInvalidVenueClockAndReanchors(t *testing.T) {
	for _, stamp := range []string{"", `"E":null,`, `"E":0,`, `"E":-1,`, `"E":253402300800000,`, `"E":"1",`, `"E":999,`} {
		t.Run("field="+stamp, func(t *testing.T) {
			frame := func(clock string, seq int) string {
				return fmt.Sprintf(`{"e":"depthUpdate",%s"s":"BTCUSDT","U":%d,"u":%d,"b":[],"a":[]}`, clock, seq, seq)
			}
			f := newFakeBinanceDepth(t,
				[]string{`{"lastUpdateId":100}`, `{"lastUpdateId":200}`, `{"lastUpdateId":300}`},
				[]string{
					frame(`"E":1000,`, 101), frame(stamp, 102),
					frame(`"E":999,`, 201), frame(`"E":1000,`, 301),
				})
			s := binanceSourceOver(f)
			defer func() { _ = s.Close() }()
			requireAnchor := func(want uint64) {
				t.Helper()
				if got := recvOK(t, s).Snapshot.GetLastUpdateSequence(); got != want {
					t.Fatalf("snapshot sequence = %d, want %d", got, want)
				}
			}
			requireRefusal := func() {
				t.Helper()
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				u, err := s.Recv(ctx)
				if err == nil || u.Snapshot != nil || u.Delta != nil {
					t.Fatalf("expected a clock refusal without an update: %+v, %v", u, err)
				}
			}
			requireAnchor(100)
			if d := recvOK(t, s).Delta; d.GetLastUpdateSequence() != 101 || d.GetEventTime().AsTime().UnixMilli() != 1000 {
				t.Fatalf("first valid venue delta = %v", d)
			}
			requireRefusal()
			requireAnchor(200)
			requireRefusal() // A new REST anchor cannot replace the last venue clock.
			requireAnchor(300)
			if d := recvOK(t, s).Delta; d.GetPrevUpdateSequence() != 300 || d.GetEventTime().AsTime().UnixMilli() != 1000 {
				t.Fatalf("same-millisecond delta after recovery = %v", d)
			}
		})
	}
}

func TestBinanceSourcePreservesVenueClockAcrossReconnect(t *testing.T) {
	f := newFakeBinanceDepth(t, []string{`{"lastUpdateId":100}`, `{"lastUpdateId":200}`},
		[]string{
			`{"e":"depthUpdate","s":"BTCUSDT","U":101,"u":101,"E":1000}`,
			`{"e":"depthUpdate","s":"BTCUSDT","U":201,"u":201,"E":999}`,
		})
	s := binanceSourceOver(f)
	defer func() { _ = s.Close() }()
	if recvOK(t, s).Snapshot.GetLastUpdateSequence() != 100 || recvOK(t, s).Delta.GetLastUpdateSequence() != 101 {
		t.Fatal("first connection did not establish its venue clock")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if recvOK(t, s).Snapshot.GetLastUpdateSequence() != 200 {
		t.Fatal("reconnected stream did not re-anchor")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if update, err := s.Recv(ctx); err == nil || update.Delta != nil || update.Snapshot != nil {
		t.Fatalf("expected post-reconnect clock regression refusal: %+v, %v", update, err)
	}
}

// The Binance depth source is certified over the REAL coder/websocket transport
// against an httptest server — hermetic, no network, no keys (depth is public).

// fakeBinanceDepth serves the REST depth anchor and the diff-depth websocket. The
// REST handler walks `snapshots` on successive calls, so a test can assert that a
// sequence gap actually causes a RE-anchor rather than an out-of-order fold.
type fakeBinanceDepth struct {
	srv *httptest.Server

	mu        sync.Mutex
	snapshots []string // successive REST bodies
	restCalls int

	events []string // frames pushed on the websocket, in order
}

func newFakeBinanceDepth(t *testing.T, snapshots, events []string) *fakeBinanceDepth {
	t.Helper()
	f := &fakeBinanceDepth{snapshots: snapshots, events: events}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/depth", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		i := f.restCalls
		if i >= len(f.snapshots) {
			i = len(f.snapshots) - 1
		}
		f.restCalls++
		_, _ = w.Write([]byte(f.snapshots[i]))
	})
	mux.HandleFunc("/ws/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		// CloseRead reads in the background and cancels ctx when the peer hangs up,
		// so the handler returns promptly instead of pinning httptest's Close.
		ctx := conn.CloseRead(r.Context())
		// Push every scripted frame immediately: they queue in the socket exactly
		// as the venue's do while the client is fetching its REST anchor.
		for _, ev := range f.events {
			if err := conn.Write(ctx, websocket.MessageText, []byte(ev)); err != nil {
				return
			}
		}
		<-ctx.Done()
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBinanceDepth) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.restCalls
}

func binanceSourceOver(f *fakeBinanceDepth) *BinanceSource {
	return NewBinanceSource(BinanceConfig{
		InstrumentID: "BTC-USD", Symbol: "BTCUSDT", MIC: "BINANCE",
		WSBase: f.srv.URL, RESTBase: f.srv.URL,
		HTTPClient: f.srv.Client(), // no DNS bypass needed against httptest
	})
}

// The venue's documented procedure end to end: the REST snapshot anchors the
// book, events wholly older than the anchor are dropped, the straddling event is
// applied, and the stream then chains exactly.
func TestBinanceSource_AnchorsAndChains(t *testing.T) {
	f := newFakeBinanceDepth(t,
		[]string{`{"lastUpdateId":100,"bids":[["50000.00","1"]],"asks":[["50010.00","2"]]}`},
		[]string{
			// u=95 <= lastUpdateId=100: predates the snapshot ⇒ must be dropped.
			`{"e":"depthUpdate","E":1,"s":"BTCUSDT","U":90,"u":95,"b":[["49000.00","9"]],"a":[]}`,
			// Straddles the anchor: U=99 <= 101 <= u=101 ⇒ the first applied event.
			`{"e":"depthUpdate","E":2,"s":"BTCUSDT","U":99,"u":101,"b":[["50001.00","3"]],"a":[]}`,
			// Chains exactly: U == prev u + 1.
			`{"e":"depthUpdate","E":3,"s":"BTCUSDT","U":102,"u":103,"b":[],"a":[["50011.00","4"]]}`,
		})
	s := binanceSourceOver(f)
	defer func() { _ = s.Close() }()

	snap := recvOK(t, s).Snapshot
	if snap == nil {
		t.Fatal("first Recv must be the REST-anchored snapshot")
	}
	if snap.GetLastUpdateSequence() != 100 {
		t.Fatalf("anchor = %d, want 100", snap.GetLastUpdateSequence())
	}
	if snap.GetMic() != "BINANCE" || snap.GetInstrumentId() != "BTC-USD" {
		t.Fatalf("snapshot identity = %s/%s, want BINANCE/BTC-USD", snap.GetMic(), snap.GetInstrumentId())
	}

	// The stale event is skipped; the straddling event is the first delta.
	d1 := recvOK(t, s).Delta
	if d1 == nil {
		t.Fatal("want the straddling delta, got no delta (was the stale event folded?)")
	}
	if d1.GetPrevUpdateSequence() != 100 || d1.GetLastUpdateSequence() != 101 {
		t.Fatalf("delta chain = prev %d last %d, want prev 100 last 101",
			d1.GetPrevUpdateSequence(), d1.GetLastUpdateSequence())
	}
	// Exactness: the price survives as an exact rational, never a float.
	if got := dec.FromProto(d1.GetBids()[0].GetPrice()); got.Cmp(big.NewRat(50001, 1)) != 0 {
		t.Fatalf("bid price = %s, want exactly 50001", got.RatString())
	}

	d2 := recvOK(t, s).Delta
	if d2 == nil || d2.GetPrevUpdateSequence() != 101 || d2.GetLastUpdateSequence() != 103 {
		t.Fatalf("second delta chain = %+v, want prev 101 last 103", d2)
	}
	if f.calls() != 1 {
		t.Fatalf("REST anchor fetched %d times, want 1 (an unbroken chain must not re-anchor)", f.calls())
	}
}

// A hole in the venue sequence re-anchors on a fresh REST snapshot rather than
// folding an out-of-order delta — a mis-folded book prices every downstream
// decision wrong.
func TestBinanceSource_SequenceGapReAnchors(t *testing.T) {
	f := newFakeBinanceDepth(t,
		[]string{
			`{"lastUpdateId":100,"bids":[["50000.00","1"]],"asks":[]}`,
			`{"lastUpdateId":200,"bids":[["50500.00","1"]],"asks":[]}`, // the re-anchor
		},
		[]string{
			`{"e":"depthUpdate","E":1,"s":"BTCUSDT","U":101,"u":101,"b":[["50001.00","3"]],"a":[]}`,
			// GAP: U=150, but the chain expects 102.
			`{"e":"depthUpdate","E":2,"s":"BTCUSDT","U":150,"u":151,"b":[["50002.00","3"]],"a":[]}`,
		})
	s := binanceSourceOver(f)
	defer func() { _ = s.Close() }()

	if recvOK(t, s).Snapshot.GetLastUpdateSequence() != 100 {
		t.Fatal("want the first anchor at 100")
	}
	if d := recvOK(t, s).Delta; d == nil || d.GetLastUpdateSequence() != 101 {
		t.Fatalf("want the in-chain delta at 101, got %+v", d)
	}

	// The gapped event must NOT surface as a delta — it must re-anchor.
	u := recvOK(t, s)
	if u.Delta != nil {
		t.Fatalf("a gapped delta was emitted (prev=%d) — the book would fold out of order",
			u.Delta.GetPrevUpdateSequence())
	}
	if u.Snapshot == nil || u.Snapshot.GetLastUpdateSequence() != 200 {
		t.Fatalf("want a re-anchoring snapshot at 200, got %+v", u.Snapshot)
	}
	if f.calls() != 2 {
		t.Fatalf("REST anchor fetched %d times, want 2 (the gap must force a re-anchor)", f.calls())
	}
}

// A size of 0 is the venue's "remove this level" instruction and must be
// preserved, not filtered out as noise — dropping it would leave a phantom level
// resting in the book forever.
func TestBinanceSource_ZeroSizeLevelPreserved(t *testing.T) {
	f := newFakeBinanceDepth(t,
		[]string{`{"lastUpdateId":100,"bids":[["50000.00","1"]],"asks":[]}`},
		[]string{`{"e":"depthUpdate","E":1,"s":"BTCUSDT","U":101,"u":101,"b":[["50000.00","0"]],"a":[]}`},
	)
	s := binanceSourceOver(f)
	defer func() { _ = s.Close() }()

	_ = recvOK(t, s) // snapshot
	d := recvOK(t, s).Delta
	if d == nil || len(d.GetBids()) != 1 {
		t.Fatalf("the zero-size level was dropped: %+v", d)
	}
	if !dec.IsZero(d.GetBids()[0].GetSize()) {
		t.Fatal("want size 0 (the remove-level instruction) preserved on the wire")
	}
}
