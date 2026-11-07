package main

import (
	"context"
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

// TestRustDepthProcessThroughJetStream validates a real subprocess and broker.
// The exchange is loopback only; no live venue, credentials or production ACLs.
func TestRustDepthProcessThroughJetStream(t *testing.T) {
	endpoint, binary := os.Getenv("TEST_NATS_URL"), os.Getenv("KANZ_BINANCE_DEPTH_BIN")
	if binary == "" {
		t.Skip("KANZ_BINANCE_DEPTH_BIN not configured; architecture-migration requires this runtime proof")
	}
	if endpoint == "" {
		t.Fatal("both TEST_NATS_URL and KANZ_BINANCE_DEPTH_BIN are required")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("KANZ_BINANCE_DEPTH_BIN must be absolute")
	}
	if info, err := os.Stat(binary); err != nil || info.IsDir() {
		t.Fatal("configured Rust binary is missing or not a file")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	bustest.EnsureSubjects(t, ctx, js, "RUST_DEPTH_IT_"+unique, []string{"market.book.snapshot", "market.crypto.quote"})
	legacy, err := nc.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	subs := map[string]*nats.Subscription{}
	for _, subject := range []string{"market.book.snapshot", "market.crypto.quote"} {
		sub, err := legacy.PullSubscribe(subject, "", nats.DeliverNew(), nats.AckExplicit())
		if err != nil {
			t.Fatal(err)
		}
		subs[subject] = sub
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
	instrument := "BTC-USD-" + unique
	permit := make(chan struct{})
	clock := make(chan int64, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/depth", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"lastUpdateId":100,"bids":[["50000","3"]],"asks":[["50001","2"]]}`)
	})
	mux.HandleFunc("/ws/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		readCtx := conn.CloseRead(r.Context())
		select {
		case <-permit:
		case <-readCtx.Done():
			return
		}
		eventMS := time.Now().UnixMilli()
		select {
		case clock <- eventMS:
		default:
		}
		_ = conn.Write(readCtx, websocket.MessageText, []byte(fmt.Sprintf(`{"e":"depthUpdate","E":%d,"s":"BTCUSDT","U":100,"u":101,"b":[["50000","4"]],"a":[]}`, eventMS)))
		<-readCtx.Done()
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
	// Explicit test inputs replace inherited runtime settings without exposing
	// credentials through command arguments, diagnostics or fixture artifacts.
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "BINANCE_DEPTH_") && !strings.HasPrefix(entry, "SPIFFE_ENDPOINT_SOCKET=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "BINANCE_DEPTH_DEVELOPMENT=true", "BINANCE_DEPTH_INSTRUMENT_ID="+instrument, "BINANCE_DEPTH_SYMBOL=BTCUSDT", "BINANCE_DEPTH_TENANT=runtime-fixture", "BINANCE_DEPTH_SOURCE=binance-depth/"+unique, "BINANCE_DEPTH_VERSION=integration", "BINANCE_DEPTH_NATS_URL="+endpoint, "BINANCE_DEPTH_WS_BASE="+strings.Replace(server.URL, "http://", "ws://", 1), "BINANCE_DEPTH_REST_BASE="+server.URL, "BINANCE_DEPTH_LISTEN="+address)
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
				t.Fatalf("Rust process stopped before readiness: %v", err)
			default:
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("readiness did not become %d", want)
	}
	waitStatus(http.StatusServiceUnavailable)
	close(permit)
	var eventMS int64
	select {
	case eventMS = <-clock:
	case <-ctx.Done():
		t.Fatal("fixture websocket not consumed")
	}
	marks := mark.New(time.Now, 30*time.Second, mark.WithTouchMaxAge(6*time.Second))
	ids := map[string]bool{}
	for _, subject := range []string{"market.book.snapshot", "market.crypto.quote"} {
		var last uint64
		for count := 0; count < 2; {
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
				t.Fatalf("Go bus rejected Rust frame: %v", err)
			}
			if env.GetTenantId() != "runtime-fixture" || env.GetEventType() != subject || env.GetSource() != "binance-depth/"+unique || env.GetProducerVersion() != "integration" {
				t.Fatal("frame attribution mismatch")
			}
			if env.GetEventTime().AsTime().UnixMilli() != eventMS {
				t.Fatal("REST/publication clock substituted for websocket E")
			}
			if msg.Header.Get("Kanz-Partition-Key") != instrument || msg.Header.Get("Nats-Msg-Id") != env.GetIdempotencyKey() {
				t.Fatal("routing/dedup header mismatch")
			}
			id, err := uuid.Parse(env.GetEventId())
			if err != nil || id.Version() != 7 || ids[env.GetEventId()] {
				t.Fatal("event ID is not unique UUIDv7")
			}
			ids[env.GetEventId()] = true
			if env.GetProducerSequence() != last+1 {
				t.Fatal("producer sequence not consecutive per subject")
			}
			last = env.GetProducerSequence()
			if subject == "market.book.snapshot" {
				if env.GetPayloadSchemaRef() != "market.v1.OrderBookSnapshot:1" {
					t.Fatal("book schema mismatch")
				}
				book := new(marketpb.OrderBookSnapshot)
				if err := proto.Unmarshal(frame.Payload, book); err != nil {
					t.Fatal(err)
				}
				if book.InstrumentId != instrument || book.LastUpdateSequence != 101 || book.EventTime.AsTime().UnixMilli() != eventMS || book.Mic != "BINANCE" || book.Symbol != instrument {
					t.Fatal("book identity/sequence/clock mismatch")
				}
				if len(book.Bids) != 1 || len(book.Asks) != 1 || dec.FromProto(book.Bids[0].Price).Cmp(big.NewRat(50000, 1)) != 0 || dec.FromProto(book.Bids[0].Size).Cmp(big.NewRat(4, 1)) != 0 || dec.FromProto(book.Asks[0].Price).Cmp(big.NewRat(50001, 1)) != 0 || dec.FromProto(book.Asks[0].Size).Cmp(big.NewRat(2, 1)) != 0 {
					t.Fatal("snapshot does not contain anchor plus first straddler")
				}
			} else {
				if env.GetPayloadSchemaRef() != "market.v1.MarketDataEvent:1" {
					t.Fatal("quote schema mismatch")
				}
				quote := new(marketpb.MarketDataEvent)
				if err := proto.Unmarshal(frame.Payload, quote); err != nil {
					t.Fatal(err)
				}
				if quote.InstrumentId != instrument || quote.Symbol != instrument || quote.Mic != "BINANCE" || quote.SourceSequence != 101 || quote.EventTime.AsTime().UnixMilli() != eventMS || quote.GetQuote() == nil {
					t.Fatal("quote identity/sequence/clock mismatch")
				}
				if err := marks.Handle(ctx, env, frame.Payload); err != nil {
					t.Fatal(err)
				}
			}
			count++
		}
	}
	bid, ask, asOf, ok := marks.Touch(instrument)
	if !ok || bid.Cmp(big.NewRat(50000, 1)) != 0 || ask.Cmp(big.NewRat(50001, 1)) != 0 || asOf.UnixMilli() != eventMS {
		t.Fatal("Rust quote did not become expected live Go mark width")
	}
	waitStatus(http.StatusOK)
	if runtime.GOOS != "windows" {
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			stopped = true
			if err != nil {
				t.Fatalf("Rust graceful termination failed: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("Rust process did not drain on SIGTERM")
		}
	}
}
