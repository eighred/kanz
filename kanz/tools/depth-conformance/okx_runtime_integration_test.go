package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/marketdata/mark"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

// Real Rust process, real JetStream, Go validation and the actual mark fold.
// Exchange data is local and synthetic; production mTLS/ACLs are not exercised.
func TestRustOKXDepthProcessThroughJetStream(t *testing.T) {
	binary, endpoint := os.Getenv("KANZ_OKX_DEPTH_BIN"), os.Getenv("TEST_NATS_URL")
	if binary == "" {
		t.Skip("KANZ_OKX_DEPTH_BIN not configured; architecture-migration requires this runtime proof")
	}
	if endpoint == "" || !filepath.IsAbs(binary) {
		t.Fatal("configured integration requires NATS and absolute Rust binary")
	}
	if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() {
		t.Fatal("configured Rust binary missing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	nc, err := nats.Connect(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	unique := fmt.Sprint(time.Now().UnixNano())
	instrument := "OKX-BTC-" + unique
	subjects := []string{"market.book.snapshot", "market.crypto.quote"}
	bustest.EnsureSubjects(t, ctx, js, "OKX_DEPTH_IT_"+unique, subjects)
	legacy, err := nc.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	subs := map[string]*nats.Subscription{}
	for _, subject := range subjects {
		sub, err := legacy.PullSubscribe(subject, "", nats.DeliverNew(), nats.AckExplicit())
		if err != nil {
			t.Fatal(err)
		}
		subs[subject] = sub
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
	frames := make(chan string)
	connections := make(chan struct{}, 8)
	fixtureErrors := make(chan string, 8)
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/v5/public", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_, raw, err := conn.Read(r.Context())
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
		if json.Unmarshal(raw, &sub) != nil || sub.Op != "subscribe" || len(sub.Args) != 1 || sub.Args[0].Channel != "books" || sub.Args[0].InstID != "BTC-USDT" {
			fixtureErrors <- "incorrect OKX subscription"
			return
		}
		readCtx := conn.CloseRead(r.Context())
		if conn.Write(readCtx, websocket.MessageText, []byte(`{"event":"subscribe","arg":{"channel":"books","instId":"BTC-USDT"}}`)) != nil {
			return
		}
		connections <- struct{}{}
		for {
			select {
			case <-readCtx.Done():
				return
			case frame := <-frames:
				if conn.Write(readCtx, websocket.MessageText, []byte(frame)) != nil {
					return
				}
			}
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	cmd := exec.CommandContext(ctx, binary)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "OKX_DEPTH_") && !strings.HasPrefix(entry, "SPIFFE_ENDPOINT_SOCKET=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "OKX_DEPTH_DEVELOPMENT=true", "OKX_DEPTH_INSTRUMENT_ID="+instrument, "OKX_DEPTH_SYMBOL=BTC-USDT", "OKX_DEPTH_TENANT=runtime-fixture", "OKX_DEPTH_SOURCE=okx-depth/"+unique, "OKX_DEPTH_VERSION=integration", "OKX_DEPTH_NATS_URL="+endpoint, "OKX_DEPTH_WS_URL="+strings.Replace(server.URL, "http://", "ws://", 1)+"/ws/v5/public", "OKX_DEPTH_LISTEN="+address)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = cmd.Process.Kill()
			<-done
		}
	})
	httpc := &http.Client{Timeout: time.Second}
	waitStatus := func(want int) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			resp, err := httpc.Get("http://" + address + "/readyz")
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == want {
					return
				}
			}
			select {
			case err := <-done:
				stopped = true
				t.Fatalf("Rust exited: %v", err)
			case msg := <-fixtureErrors:
				t.Fatal(msg)
			default:
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("readiness never became %d", want)
	}
	waitConnected := func() {
		t.Helper()
		select {
		case <-connections:
		case msg := <-fixtureErrors:
			t.Fatal(msg)
		case <-ctx.Done():
			t.Fatal("OKX subscription never arrived")
		}
	}
	send := func(action string, seq, prev int64, bid, size string) int64 {
		t.Helper()
		ms := time.Now().UnixMilli()
		raw := fmt.Sprintf(`{"arg":{"channel":"books","instId":"BTC-USDT"},"action":%q,"data":[{"bids":[[%q,%q,"0","1"]],"asks":[["50001","2","0","1"]],"ts":%q,"seqId":%d,"prevSeqId":%d}]}`, action, bid, size, fmt.Sprint(ms), seq, prev)
		select {
		case frames <- raw:
		case <-ctx.Done():
			t.Fatal("fixture send timed out")
		}
		return ms
	}
	waitConnected()
	waitStatus(http.StatusServiceUnavailable)
	marks := mark.New(time.Now, 30*time.Second, mark.WithTouchMaxAge(6*time.Second))
	seenIDs := map[string]bool{}
	producerSeq := map[string]uint64{}
	var forbiddenMS int64
	check := func(sequence uint64, ms int64, wantBid, wantSize int64) {
		t.Helper()
		for _, subject := range subjects {
			for {
				messages, err := subs[subject].Fetch(1, nats.MaxWait(8*time.Second))
				if err != nil {
					t.Fatal(err)
				}
				msg := messages[0]
				_ = msg.Ack()
				frame := new(envelopepb.EventFrame)
				if err := proto.Unmarshal(msg.Data, frame); err != nil {
					t.Fatal(err)
				}
				env := frame.GetEnvelope()
				if env.GetPartitionKey() != instrument {
					continue
				}
				if err := bus.Validate(env); err != nil {
					t.Fatalf("Go rejected Rust frame: %v", err)
				}
				if forbiddenMS != 0 && env.GetEventTime().AsTime().UnixMilli() == forbiddenMS {
					t.Fatal("continuity failure was published instead of invalidated")
				}
				if env.GetTenantId() != "runtime-fixture" || env.GetEventType() != subject || env.GetSource() != "okx-depth/"+unique || env.GetProducerVersion() != "integration" || msg.Header.Get("Kanz-Partition-Key") != instrument || msg.Header.Get("Nats-Msg-Id") != env.GetIdempotencyKey() {
					t.Fatal("attribution/routing/dedup mismatch")
				}
				id, err := uuid.Parse(env.GetEventId())
				if err != nil || id.Version() != 7 || seenIDs[env.GetEventId()] {
					t.Fatal("expected unique UUIDv7")
				}
				seenIDs[env.GetEventId()] = true
				if env.GetProducerSequence() != producerSeq[subject]+1 {
					t.Fatal("producer sequence skipped or reset")
				}
				producerSeq[subject] = env.GetProducerSequence()
				if env.GetEventTime().AsTime().UnixMilli() != ms {
					continue
				}
				if subject == subjects[0] {
					book := new(marketpb.OrderBookSnapshot)
					if err := proto.Unmarshal(frame.Payload, book); err != nil {
						t.Fatal(err)
					}
					if env.GetPayloadSchemaRef() != "market.v1.OrderBookSnapshot:1" || book.InstrumentId != instrument || book.Symbol != instrument || book.Mic != "OKX" || book.LastUpdateSequence != sequence || book.EventTime.AsTime().UnixMilli() != ms || len(book.Bids) != 1 || len(book.Asks) != 1 {
						t.Fatal("snapshot identity/sequence mismatch")
					}
					if dec.FromProto(book.Bids[0].Price).Cmp(big.NewRat(wantBid, 1)) != 0 || dec.FromProto(book.Bids[0].Size).Cmp(big.NewRat(wantSize, 1)) != 0 || dec.FromProto(book.Asks[0].Price).Cmp(big.NewRat(50001, 1)) != 0 || dec.FromProto(book.Asks[0].Size).Cmp(big.NewRat(2, 1)) != 0 {
						t.Fatal("snapshot depth mismatch")
					}
				} else {
					ev := new(marketpb.MarketDataEvent)
					if err := proto.Unmarshal(frame.Payload, ev); err != nil {
						t.Fatal(err)
					}
					if ev.EventTime == nil || ev.EventTime.CheckValid() != nil || ev.EventTime.AsTime().UnixMilli() != ms || !proto.Equal(ev.EventTime, env.EventTime) {
						t.Fatal("quote payload must carry the exact venue/envelope timestamp")
					}
					if env.GetPayloadSchemaRef() != "market.v1.MarketDataEvent:1" || ev.SourceSequence != sequence || ev.InstrumentId != instrument || ev.Symbol != instrument || ev.Mic != "OKX" || ev.GetQuote() == nil {
						t.Fatal("quote identity/sequence mismatch")
					}
					quote := ev.GetQuote()
					if dec.FromProto(quote.BidPrice).Cmp(big.NewRat(wantBid, 1)) != 0 || dec.FromProto(quote.AskPrice).Cmp(big.NewRat(50001, 1)) != 0 || dec.FromProto(quote.BidSize).Cmp(big.NewRat(wantSize, 1)) != 0 || dec.FromProto(quote.AskSize).Cmp(big.NewRat(2, 1)) != 0 {
						t.Fatal("quote prices/quantities differ from the same snapshot")
					}
					if err := marks.Handle(ctx, env, frame.Payload); err != nil {
						t.Fatal(err)
					}
				}
				break
			}
		}
		bid, ask, at, ok := marks.Touch(instrument)
		if !ok || bid.Cmp(big.NewRat(wantBid, 1)) != 0 || ask.Cmp(big.NewRat(50001, 1)) != 0 || at.UnixMilli() != ms {
			t.Fatal("quote did not reach Go live mark width")
		}
		waitStatus(http.StatusOK)
	}
	check(100, send("snapshot", 100, -1, "50000", "3"), 50000, 3)
	check(101, send("update", 101, 100, "50000", "4"), 50000, 4)
	check(0, send("update", 0, 101, "50000", "5"), 50000, 5)
	check(1, send("update", 1, 0, "50000", "6"), 50000, 6)
	// A broken previous sequence must revoke readiness before a replacement
	// snapshot. No guessed delta may turn this deliberately crossed input live.
	forbiddenMS = send("update", 999, 777, "60000", "9")
	waitConnected()
	waitStatus(http.StatusServiceUnavailable)
	check(200, send("snapshot", 200, -1, "49999", "7"), 49999, 7)
	if runtime.GOOS != "windows" {
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			stopped = true
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("Rust did not drain on SIGTERM")
		}
	}
}
