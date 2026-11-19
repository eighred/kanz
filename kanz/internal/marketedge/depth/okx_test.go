package depth

import (
	"context"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/eighred/kanz/internal/dec"
)

// The OKX depth source is certified over the REAL coder/websocket transport
// against an httptest server — hermetic, no network, no keys (depth is public).

// fakeOKXDepth serves the public v5 websocket. Each successive CONNECTION is
// served the next script, so a test can assert that a sequence gap really does
// drop the connection and resubscribe to re-anchor.
type fakeOKXDepth struct {
	srv *httptest.Server

	mu      sync.Mutex
	scripts [][]string // frames per connection
	conns   int

	sawSubscribe bool
}

func newFakeOKXDepth(t *testing.T, scripts [][]string) *fakeOKXDepth {
	t.Helper()
	f := &fakeOKXDepth{scripts: scripts}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()

		f.mu.Lock()
		i := f.conns
		if i >= len(f.scripts) {
			i = len(f.scripts) - 1
		}
		f.conns++
		script := f.scripts[i]
		f.mu.Unlock()

		ctx := r.Context()
		// The client subscribes first; ack it, then push the scripted frames.
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
		f.mu.Lock()
		f.sawSubscribe = true
		f.mu.Unlock()

		ack := `{"event":"subscribe","arg":{"channel":"books","instId":"BTC-USDT"}}`
		if err := conn.Write(ctx, websocket.MessageText, []byte(ack)); err != nil {
			return
		}
		for _, frame := range script {
			if err := conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
				return
			}
		}
		// Drain until the peer hangs up (the client may send keepalive pings).
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOKXDepth) connCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns
}

// A valid sequence identifies the next change but cannot supply its event time.
func TestOKXSourceRefusesInvalidVenueTime(t *testing.T) {
	for _, action := range []string{"snapshot", "update"} {
		for _, ts := range []string{"", "invalid", "0", "-1", "253402300800000"} {
			t.Run(action+"/"+ts, func(t *testing.T) {
				bad := strings.Replace(okxBook(action, 101, 100, "101"), "1700000000000", ts, 1)
				if ts == "" {
					bad = strings.Replace(bad, `"ts":"",`, "", 1)
				}
				first := []string{bad}
				if action == "update" {
					first = append([]string{okxBook("snapshot", 100, -1, "100")}, first...)
				}
				f := newFakeOKXDepth(t, [][]string{first, {okxBook("update", 102, 101, "999"), okxBook("snapshot", 200, -1, "200")}})
				s := okxSourceOver(f)
				defer func() { _ = s.Close() }()
				if action == "update" {
					recvOK(t, s)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				u, err := s.Recv(ctx)
				if err == nil || !strings.Contains(err.Error(), "event time") || u.Snapshot != nil || u.Delta != nil {
					t.Fatalf("invalid venue time %q: got %+v, err=%v", ts, u, err)
				}
				if got := recvOK(t, s).Snapshot; got.GetLastUpdateSequence() != 200 || f.connCount() != 2 {
					t.Fatalf("missing new anchor: %v", got)
				}
			})
		}
	}
}

func TestOKXSourceRequiresNondecreasingVenueTimeAcrossReanchor(t *testing.T) {
	frame := func(action string, seq, prev int64, ts string) string {
		return strings.Replace(okxBook(action, seq, prev, "100"), "1700000000000", ts, 1)
	}
	const start = "1700000000123"
	const advanced = "1700000000124"
	f := newFakeOKXDepth(t, [][]string{
		{frame("snapshot", 100, -1, start), frame("update", 101, 100, advanced), frame("update", 102, 101, start)},
		{frame("snapshot", 200, -1, start)},
		{frame("update", 301, 300, advanced), frame("snapshot", 400, -1, advanced), frame("update", 401, 400, advanced)},
	})
	s := okxSourceOver(f)
	defer func() { _ = s.Close() }()
	if ts := recvOK(t, s).Snapshot.GetEventTime(); ts.GetSeconds() != 1700000000 || ts.GetNanos() != 123000000 {
		t.Fatalf("snapshot venue time %v", ts)
	}
	if ts := recvOK(t, s).Delta.GetEventTime(); ts.GetSeconds() != 1700000000 || ts.GetNanos() != 124000000 {
		t.Fatalf("delta venue time %v", ts)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for _, action := range []string{"delta", "reconnect snapshot"} {
		u, err := s.Recv(ctx)
		if err == nil || !strings.Contains(err.Error(), "regressed") || u.Snapshot != nil || u.Delta != nil {
			t.Fatalf("%s regressed: got %+v, err=%v", action, u, err)
		}
	}
	if got := recvOK(t, s).Snapshot; got.GetLastUpdateSequence() != 400 || got.GetEventTime().GetNanos() != 124000000 {
		t.Fatalf("new anchor %v", got)
	}
	if got := recvOK(t, s).Delta; got.GetPrevUpdateSequence() != 400 || got.GetLastUpdateSequence() != 401 || got.GetEventTime().GetNanos() != 124000000 || f.connCount() != 3 {
		t.Fatalf("same-millisecond delta %v, connections=%d", got, f.connCount())
	}
}

func okxSourceOver(f *fakeOKXDepth) *OKXSource {
	return NewOKXSource(OKXConfig{
		InstrumentID: "BTC-USD", InstID: "BTC-USDT", MIC: "OKX",
		WSURL:      f.srv.URL,
		HTTPClient: f.srv.Client(), // no DNS bypass needed against httptest
	})
}

func okxBook(action string, seq, prev int64, bid string) string {
	return `{"arg":{"channel":"books","instId":"BTC-USDT"},"action":"` + action +
		`","data":[{"bids":[["` + bid + `","1","0","1"]],"asks":[],"ts":"1700000000000",` +
		`"seqId":` + strconv.FormatInt(seq, 10) + `,"prevSeqId":` + strconv.FormatInt(prev, 10) + `}]}`
}

// The subscribe pushes a snapshot, then updates chain off it via prevSeqId.
func TestOKXSource_SnapshotThenChainedUpdates(t *testing.T) {
	f := newFakeOKXDepth(t, [][]string{{
		okxBook("snapshot", 100, -1, "50000.00"),
		okxBook("update", 101, 100, "50001.00"),
		okxBook("update", 102, 101, "50002.00"),
	}})
	s := okxSourceOver(f)
	defer func() { _ = s.Close() }()

	snap := recvOK(t, s).Snapshot
	if snap == nil {
		t.Fatal("first Recv must be the subscribe snapshot")
	}
	if snap.GetLastUpdateSequence() != 100 {
		t.Fatalf("anchor = %d, want 100", snap.GetLastUpdateSequence())
	}
	if snap.GetMic() != "OKX" || snap.GetInstrumentId() != "BTC-USD" {
		t.Fatalf("snapshot identity = %s/%s, want OKX/BTC-USD", snap.GetMic(), snap.GetInstrumentId())
	}

	d1 := recvOK(t, s).Delta
	if d1 == nil || d1.GetPrevUpdateSequence() != 100 || d1.GetLastUpdateSequence() != 101 {
		t.Fatalf("first delta chain = %+v, want prev 100 last 101", d1)
	}
	// Exactness: the price survives as an exact rational, never a float.
	if got := dec.FromProto(d1.GetBids()[0].GetPrice()); got.Cmp(big.NewRat(50001, 1)) != 0 {
		t.Fatalf("bid price = %s, want exactly 50001", got.RatString())
	}

	d2 := recvOK(t, s).Delta
	if d2 == nil || d2.GetPrevUpdateSequence() != 101 || d2.GetLastUpdateSequence() != 102 {
		t.Fatalf("second delta chain = %+v, want prev 101 last 102", d2)
	}
	if f.connCount() != 1 {
		t.Fatalf("connected %d times, want 1 (an unbroken chain must not reconnect)", f.connCount())
	}
}

// OKX signals "no change in depth" by repeating the seqId. That frame must be
// skipped, not re-folded.
func TestOKXSource_NoChangeFrameSkipped(t *testing.T) {
	f := newFakeOKXDepth(t, [][]string{{
		okxBook("snapshot", 100, -1, "50000.00"),
		okxBook("update", 100, 100, "50000.00"), // seqId == prevSeqId ⇒ no change
		okxBook("update", 101, 100, "50001.00"),
	}})
	s := okxSourceOver(f)
	defer func() { _ = s.Close() }()

	_ = recvOK(t, s) // snapshot
	d := recvOK(t, s).Delta
	if d == nil || d.GetLastUpdateSequence() != 101 {
		t.Fatalf("want the no-change frame skipped and the 101 delta next, got %+v", d)
	}
}

// A hole in the venue chain drops the connection and resubscribes, so OKX
// re-pushes a snapshot — we re-anchor rather than fold out of order.
func TestOKXSource_SequenceGapReAnchors(t *testing.T) {
	f := newFakeOKXDepth(t, [][]string{
		{
			okxBook("snapshot", 100, -1, "50000.00"),
			// GAP: prevSeqId=150, but the chain is at 100.
			okxBook("update", 151, 150, "50002.00"),
		},
		{ // the second connection re-anchors
			okxBook("snapshot", 200, -1, "50500.00"),
		},
	})
	s := okxSourceOver(f)
	defer func() { _ = s.Close() }()

	if recvOK(t, s).Snapshot.GetLastUpdateSequence() != 100 {
		t.Fatal("want the first anchor at 100")
	}

	// The gapped frame must NOT surface as a delta — it must re-anchor.
	u := recvOK(t, s)
	if u.Delta != nil {
		t.Fatalf("a gapped delta was emitted (prev=%d) — the book would fold out of order",
			u.Delta.GetPrevUpdateSequence())
	}
	if u.Snapshot == nil || u.Snapshot.GetLastUpdateSequence() != 200 {
		t.Fatalf("want a re-anchoring snapshot at 200, got %+v", u.Snapshot)
	}
	if f.connCount() != 2 {
		t.Fatalf("connected %d times, want 2 (the gap must force a resubscribe)", f.connCount())
	}
}

// A size of 0 is the venue's "remove this level" instruction and must survive.
func TestOKXSource_ZeroSizeLevelPreserved(t *testing.T) {
	f := newFakeOKXDepth(t, [][]string{{
		okxBook("snapshot", 100, -1, "50000.00"),
		`{"arg":{"channel":"books","instId":"BTC-USDT"},"action":"update","data":[{"bids":[["50000.00","0","0","0"]],"asks":[],"ts":"1700000000000","seqId":101,"prevSeqId":100}]}`,
	}})
	s := okxSourceOver(f)
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

// A subscribe rejection surfaces loudly rather than silently idling on a book
// that will never arrive.
func TestOKXSource_SubscribeErrorSurfaces(t *testing.T) {
	f := newFakeOKXDepth(t, [][]string{{
		`{"event":"error","code":"60012","msg":"Invalid request: unknown instId"}`,
	}})
	s := okxSourceOver(f)
	defer func() { _ = s.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.Recv(ctx); err == nil {
		t.Fatal("Recv = nil error on a rejected subscribe — a book that will never arrive must fail loud")
	}
}

func TestOKXSourceRejectsMisboundFramesAndRequiresFreshAnchor(t *testing.T) {
	for _, action := range []string{"snapshot", "update"} {
		for _, tc := range []struct{ name, from, to string }{
			{"channel", "books", "books5"}, {"instrument", "BTC-USDT", "ETH-USDT"},
			{"missing-instrument", "BTC-USDT", ""}, {"action", action, "replace"},
		} {
			t.Run(action+"/"+tc.name, func(t *testing.T) {
				frame := strings.Replace(okxBook(action, 101, 100, "999"), tc.from, tc.to, 1)
				frames := []string{frame}
				if action == "update" {
					frames = append([]string{okxBook("snapshot", 100, -1, "100")}, frames...)
				}
				f := newFakeOKXDepth(t, [][]string{frames, {okxBook("update", 102, 101, "999"), okxBook("snapshot", 200, -1, "200"), okxBook("update", 201, 200, "201")}})
				s := okxSourceOver(f)
				defer func() { _ = s.Close() }()
				if action == "update" {
					if recvOK(t, s).Snapshot.GetLastUpdateSequence() != 100 {
						t.Fatal("missing first anchor")
					}
				}
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				got, err := s.Recv(ctx)
				if err == nil || got.Snapshot != nil || got.Delta != nil {
					t.Fatalf("misbound frame returned %+v, %v", got, err)
				}
				if recvOK(t, s).Snapshot.GetLastUpdateSequence() != 200 || recvOK(t, s).Delta.GetPrevUpdateSequence() != 200 || f.connCount() != 2 {
					t.Fatal("subscription did not re-anchor before emitting its next delta")
				}
			})
		}
	}
}
