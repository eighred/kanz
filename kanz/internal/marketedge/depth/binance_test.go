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

func TestBinanceSourceWithholdsUnconfirmedRESTAnchor(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame string
	}{
		{name: "no update"},
		{name: "wrong instrument", frame: `{"e":"depthUpdate","E":1000,"s":"ETHUSDT","U":101,"u":101}`},
		{name: "wrong stream", frame: `{"e":"trade","E":1000,"s":"BTCUSDT","U":101,"u":101}`},
		{name: "missing first sequence", frame: `{"e":"depthUpdate","E":1000,"s":"BTCUSDT","u":101}`},
		{name: "reversed sequence", frame: `{"e":"depthUpdate","E":1000,"s":"BTCUSDT","U":102,"u":101}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []string
			if tc.frame != "" {
				events = []string{tc.frame}
			}
			f := newFakeBinanceDepth(t, []string{`{"lastUpdateId":100,"bids":[["101","2"]]}`}, events)
			s := binanceSourceOver(f)
			defer func() { _ = s.Close() }()
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			update, err := s.Recv(ctx)
			if err == nil || update.Snapshot != nil || update.Delta != nil || f.calls() != 1 {
				t.Fatalf("unconfirmed REST anchor: update=%v err=%v REST requests=%d", update, err, f.calls())
			}
		})
	}
}

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
			requireRefusal := func() {
				t.Helper()
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				u, err := s.Recv(ctx)
				if err == nil || u.Snapshot != nil || u.Delta != nil {
					t.Fatalf("expected a clock refusal without an update: %+v, %v", u, err)
				}
			}
			if snap := recvOK(t, s).Snapshot; snap.GetLastUpdateSequence() != 101 || snap.GetEventTime().AsTime().UnixMilli() != 1000 {
				t.Fatalf("first clocked book = %v", snap)
			}
			requireRefusal()
			requireRefusal() // The REST anchor at 200 cannot replace the venue clock.
			if snap := recvOK(t, s).Snapshot; snap.GetLastUpdateSequence() != 301 || snap.GetEventTime().AsTime().UnixMilli() != 1000 {
				t.Fatalf("same-millisecond book after recovery = %v", snap)
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
	if recvOK(t, s).Snapshot.GetLastUpdateSequence() != 101 {
		t.Fatal("first connection did not establish its venue clock")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
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
		t.Fatal("first Recv must include the venue update")
	}
	if snap.GetLastUpdateSequence() != 101 || snap.GetEventTime().AsTime().UnixMilli() != 2 {
		t.Fatalf("first visible book has wrong sequence or event time: %v", snap)
	}
	if snap.GetMic() != "BINANCE" || snap.GetInstrumentId() != "BTC-USD" {
		t.Fatalf("snapshot identity = %s/%s, want BINANCE/BTC-USD", snap.GetMic(), snap.GetInstrumentId())
	}

	// The published book contains both the REST levels and the straddling update.
	// The old buffered event must not add its bid.
	if len(snap.Bids) != 2 || len(snap.Asks) != 1 ||
		dec.FromProto(snap.Bids[0].Price).Cmp(big.NewRat(50001, 1)) != 0 ||
		dec.FromProto(snap.Bids[0].Size).Cmp(big.NewRat(3, 1)) != 0 ||
		dec.FromProto(snap.Bids[1].Price).Cmp(big.NewRat(50000, 1)) != 0 ||
		dec.FromProto(snap.Asks[0].Price).Cmp(big.NewRat(50010, 1)) != 0 {
		t.Fatalf("wrong merged levels: %v", snap)
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
			`{"e":"depthUpdate","E":3,"s":"BTCUSDT","U":201,"u":201,"b":[],"a":[["50501","4"]]}`,
		})
	s := binanceSourceOver(f)
	defer func() { _ = s.Close() }()

	if recvOK(t, s).Snapshot.GetLastUpdateSequence() != 101 {
		t.Fatal("want the first clocked book at 101")
	}

	// The gapped event must NOT surface as a delta — it must re-anchor.
	u := recvOK(t, s)
	if u.Delta != nil {
		t.Fatalf("a gapped delta was emitted (prev=%d) — the book would fold out of order",
			u.Delta.GetPrevUpdateSequence())
	}
	if u.Snapshot == nil || u.Snapshot.GetLastUpdateSequence() != 201 || u.Snapshot.EventTime.AsTime().UnixMilli() != 3 ||
		len(u.Snapshot.Bids) != 1 || dec.FromProto(u.Snapshot.Bids[0].Price).Cmp(big.NewRat(50500, 1)) != 0 || len(u.Snapshot.Asks) != 1 {
		t.Fatalf("want a re-anchoring merged snapshot at 201, got %+v", u.Snapshot)
	}
	if f.calls() != 2 {
		t.Fatalf("REST anchor fetched %d times, want 2 (the gap must force a re-anchor)", f.calls())
	}
}

func TestBinanceSourceFoldsGapFrameIntoRefreshedAnchor(t *testing.T) {
	f := newFakeBinanceDepth(t,
		[]string{`{"lastUpdateId":100}`, `{"lastUpdateId":149,"bids":[["2","8"]]}`},
		[]string{
			`{"e":"depthUpdate","E":1,"s":"BTCUSDT","U":101,"u":101}`,
			`{"e":"depthUpdate","E":2,"s":"BTCUSDT","U":150,"u":151,"b":[["2","3"]]}`,
		})
	s := binanceSourceOver(f)
	defer func() { _ = s.Close() }()
	if got := recvOK(t, s).Snapshot.GetLastUpdateSequence(); got != 101 {
		t.Fatalf("initial sequence = %d", got)
	}
	snap := recvOK(t, s).Snapshot
	if snap.GetLastUpdateSequence() != 151 || snap.GetEventTime().AsTime().UnixMilli() != 2 || len(snap.Bids) != 1 ||
		dec.FromProto(snap.Bids[0].Size).Cmp(big.NewRat(3, 1)) != 0 || f.calls() != 2 {
		t.Fatalf("gap frame failed to confirm refreshed anchor: %v", snap)
	}
}

// A size of 0 is the venue's "remove this level" instruction and must be
// preserved, not filtered out as noise — dropping it would leave a phantom level
// resting in the book forever.
func TestBinanceSource_ZeroSizeLevelDeletesAnchorBid(t *testing.T) {
	f := newFakeBinanceDepth(t,
		[]string{`{"lastUpdateId":100,"bids":[["50000.00","1"]],"asks":[]}`},
		[]string{`{"e":"depthUpdate","E":1,"s":"BTCUSDT","U":101,"u":101,"b":[["50000.00","0"]],"a":[]}`},
	)
	s := binanceSourceOver(f)
	defer func() { _ = s.Close() }()

	snap := recvOK(t, s).Snapshot
	if snap == nil || len(snap.Bids) != 0 || snap.LastUpdateSequence != 101 {
		t.Fatalf("deleted bid still present or anchor escaped: %v", snap)
	}
}
