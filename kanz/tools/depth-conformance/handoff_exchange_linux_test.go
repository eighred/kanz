//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// This serves public exchange protocol frames only. Both production binaries
// still perform their own decoding, book recovery and canonical publication.
type handoffExchange struct {
	t            *testing.T
	venue        string
	server       *httptest.Server
	mu           sync.Mutex
	clients      map[*websocket.Conn]string
	accepted     map[string]int
	wg           sync.WaitGroup
	closing      bool
	restSequence int64
	restRequests int
	restGate     <-chan struct{}
}

func startHandoffExchange(t *testing.T, venue string) *handoffExchange {
	t.Helper()
	x := &handoffExchange{t: t, venue: venue, clients: make(map[*websocket.Conn]string), accepted: make(map[string]int), restSequence: 100}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/depth", func(w http.ResponseWriter, r *http.Request) {
		x.mu.Lock()
		if x.closing {
			x.mu.Unlock()
			http.Error(w, "fixture closing", http.StatusServiceUnavailable)
			return
		}
		x.wg.Add(1)
		defer x.wg.Done()
		x.restRequests++
		sequence, gate := x.restSequence, x.restGate
		x.mu.Unlock()
		if gate != nil {
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			case <-timer.C:
				http.Error(w, "fixture anchor gate expired", http.StatusServiceUnavailable)
				return
			}
		}
		_, _ = io.WriteString(w, fmt.Sprintf(`{"lastUpdateId":%d,"bids":[["50000","3"]],"asks":[["50001","2"]]}`, sequence))
	})
	mux.HandleFunc("/ws/", x.serve)
	x.server = httptest.NewUnstartedServer(mux)
	x.server.Config.ReadHeaderTimeout = 2 * time.Second
	x.server.Config.MaxHeaderBytes = 8192
	x.server.Start()
	t.Cleanup(func() {
		x.mu.Lock()
		x.closing = true
		for c := range x.clients {
			_ = c.CloseNow()
		}
		x.mu.Unlock()
		done := make(chan struct{})
		go func() { x.server.Close(); x.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("exchange handlers did not join")
		}
	})
	return x
}

func (x *handoffExchange) serve(w http.ResponseWriter, r *http.Request) {
	x.wg.Add(1)
	defer x.wg.Done()
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() {
		if err := c.CloseNow(); err != nil && !errors.Is(err, net.ErrClosed) {
			x.t.Error("exchange websocket cleanup failed")
		}
	}()
	x.mu.Lock()
	if x.closing {
		x.mu.Unlock()
		return
	}
	x.clients[c] = ""
	x.mu.Unlock()
	defer func() { x.mu.Lock(); delete(x.clients, c); x.mu.Unlock() }()
	c.SetReadLimit(4096)
	kind := "depth"
	ctx, cancel := context.WithTimeout(r.Context(), 150*time.Second)
	defer cancel()
	if x.venue == "okx" {
		_, raw, err := c.Read(ctx)
		if err != nil {
			return
		}
		var sub struct {
			Op   string `json:"op"`
			Args []struct {
				Channel string `json:"channel"`
				InstID  string `json:"instId"`
			} `json:"args"`
		}
		if json.Unmarshal(raw, &sub) != nil || sub.Op != "subscribe" || len(sub.Args) != 1 || sub.Args[0].InstID != "BTC-USDT" {
			return
		}
		channel := sub.Args[0].Channel
		if channel == "trades" {
			kind = "trade"
		} else if channel != "books" {
			return
		}
		ack := fmt.Sprintf(`{"event":"subscribe","arg":{"channel":%q,"instId":"BTC-USDT"}}`, channel)
		if c.Write(ctx, websocket.MessageText, []byte(ack)) != nil {
			return
		}
	} else if strings.HasSuffix(r.URL.Path, "@trade") {
		kind = "trade"
	} else if !strings.Contains(r.URL.Path, "@depth") {
		return
	}
	x.mu.Lock()
	x.clients[c] = kind
	x.accepted[kind]++
	x.mu.Unlock()
	for {
		_, raw, err := c.Read(ctx)
		if err != nil {
			return
		}
		if string(raw) == "ping" {
			if c.Write(ctx, websocket.MessageText, []byte("pong")) != nil {
				return
			}
		} else {
			return
		}
	}
}

// Hold the next real REST response until the caller has observed invalidation.
// Cleanup releases the gate even when an assertion aborts the rehearsal.
func (x *handoffExchange) holdAnchor(sequence int64) (int, func()) {
	x.mu.Lock()
	gate := make(chan struct{})
	x.restSequence, x.restGate = sequence, gate
	before := x.restRequests
	x.mu.Unlock()
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	x.t.Cleanup(release)
	return before, release
}

func (x *handoffExchange) anchorRequests() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.restRequests
}

func (x *handoffExchange) resetAnchor() {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.restSequence, x.restGate = 100, nil
}

func (x *handoffExchange) count(kind string) int {
	x.mu.Lock()
	defer x.mu.Unlock()
	n := 0
	for _, k := range x.clients {
		if k == kind {
			n++
		}
	}
	return n
}

func (x *handoffExchange) connections(kind string) int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.accepted[kind]
}

func (x *handoffExchange) send(kind, frame string) {
	x.t.Helper()
	x.mu.Lock()
	var clients []*websocket.Conn
	for c, k := range x.clients {
		if k == kind {
			clients = append(clients, c)
		}
	}
	x.mu.Unlock()
	if len(clients) != 1 {
		x.t.Fatalf("expected one controlled %s exchange owner, got %d", kind, len(clients))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if clients[0].Write(ctx, websocket.MessageText, []byte(frame)) != nil {
		x.t.Fatal("exchange frame delivery failed")
	}
}

func (x *handoffExchange) depth(snapshot bool, sequence, previous int64) int64 {
	stamp := time.Now().UnixMilli()
	if x.venue == "binance" {
		x.send("depth", fmt.Sprintf(`{"e":"depthUpdate","E":%d,"s":"BTCUSDT","U":%d,"u":%d,"b":[["50000","4"]],"a":[["50001","2"]]}`, stamp, sequence, sequence))
	} else {
		action := "update"
		if snapshot {
			action = "snapshot"
			previous = -1
		}
		x.send("depth", fmt.Sprintf(`{"arg":{"channel":"books","instId":"BTC-USDT"},"action":%q,"data":[{"bids":[["50000","4","0","1"]],"asks":[["50001","2","0","1"]],"ts":"%d","seqId":%d,"prevSeqId":%d}]}`, action, stamp, sequence, previous))
	}
	return stamp
}

func (x *handoffExchange) trade(stamp time.Time, id int) {
	if x.venue == "binance" {
		x.send("trade", fmt.Sprintf(`{"e":"trade","E":%d,"s":"BTCUSDT","t":%d,"p":"50000","q":"2","T":%d,"m":false}`, stamp.UnixMilli(), id, stamp.UnixMilli()))
	} else {
		x.send("trade", fmt.Sprintf(`{"arg":{"channel":"trades","instId":"BTC-USDT"},"data":[{"instId":"BTC-USDT","tradeId":"%d","px":"50000","sz":"2","side":"buy","ts":"%d"}]}`, id, stamp.UnixMilli()))
	}
}
