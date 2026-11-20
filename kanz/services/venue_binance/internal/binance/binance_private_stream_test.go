package binance

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/internal/venueadapter/accountproof"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	ns "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

const confirmed = `{"id":"private-stream","status":200,"result":{"subscriptionId":0}}`
const fixtureTime int64 = 1700000000000

func privateStreamFixture(t *testing.T, serve func(context.Context, *websocket.Conn)) (*binanceREST, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/account", func(w http.ResponseWriter, r *http.Request) {
		mac := hmac.New(sha256.New, []byte("fixture-secret"))
		fmt.Fprint(mac, "recvWindow=5000&timestamp=1700000000000")
		query := r.URL.Query()
		if r.Header.Get("X-MBX-APIKEY") != "fixture" || query.Get("timestamp") != "1700000000000" ||
			query.Get("recvWindow") != "5000" || query.Get("signature") != hex.EncodeToString(mac.Sum(nil)) {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, `{"uid":123}`)
	})
	mux.HandleFunc("/ws-api/v3", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" || r.Header.Get("X-MBX-APIKEY") != "" {
			t.Error("credential or unexpected query on upgrade")
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error("fixture upgrade failed")
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		typ, raw, err := conn.Read(ctx)
		var req struct {
			ID     string
			Method string
			Params map[string]json.RawMessage
		}
		mac := hmac.New(sha256.New, []byte("fixture-secret"))
		fmt.Fprint(mac, "apiKey=fixture&timestamp=1700000000000")
		wantSig := fmt.Sprintf("%q", hex.EncodeToString(mac.Sum(nil)))
		if err != nil || typ != websocket.MessageText || json.Unmarshal(raw, &req) != nil || req.ID != "private-stream" ||
			req.Method != "userDataStream.subscribe.signature" || len(req.Params) != 3 || string(req.Params["apiKey"]) != `"fixture"` ||
			string(req.Params["timestamp"]) != "1700000000000" || string(req.Params["signature"]) != wantSig {
			t.Error("private subscription request or independently computed HMAC differs")
			return
		}
		serve(ctx, conn)
	})
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	rest := newBinanceREST(restConfig{BaseURL: server.URL, APIKey: "fixture", APISecret: "fixture-secret", HTTPClient: server.Client(),
		Bucket: newWeightBucket(1200, time.Minute, nil), Now: func() time.Time { return time.UnixMilli(fixtureTime) }})
	endpoint, err := privateStreamEndpoint(rest.baseURL, "wss"+strings.TrimPrefix(server.URL, "https"))
	if err != nil {
		t.Fatal(err)
	}
	return rest, endpoint
}

func TestPrivateStreamRequiresAccountProofBeforeStart(t *testing.T) {
	started := make(chan struct{}, 1)
	rest, _ := privateStreamFixture(t, func(ctx context.Context, conn *websocket.Conn) {
		_ = conn.Write(ctx, websocket.MessageText, []byte(confirmed))
		started <- struct{}{}
		_, _, _ = conn.Read(ctx)
	})
	wsBase := "wss" + strings.TrimPrefix(rest.baseURL, "https")
	connector := NewBinanceConnector(VenueSettings{BaseURL: rest.baseURL, Account: "fixture-account", APIKey: rest.apiKey, APISecret: string(rest.apiSecret)}, wsBase)
	connector.rest.httpc, connector.rest.now = rest.httpc, rest.now
	deps := WorkerDeps{Orders: newFakeOrders(), TickerInterval: time.Hour}
	for _, tc := range []struct {
		uid  string
		want error
	}{
		{"", accountproof.ErrUnverifiable}, {"321", accountproof.ErrWrongAccount},
	} {
		proof, err := connector.Start(t.Context(), tc.uid, deps)
		if !errors.Is(err, tc.want) || proof.Verified {
			t.Fatal("unproven account was accepted", err)
		}
	}
	connector.rest.apiKey = "unknown-fixture"
	if _, err := connector.Start(t.Context(), "123", deps); !errors.Is(err, accountproof.ErrUnverifiable) {
		t.Fatal("refused account lookup was accepted")
	}
	select {
	case <-started:
		t.Fatal("subscriber started before the bound account was proven")
	default:
	}
	connector.rest.apiKey = rest.apiKey
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	proof, err := connector.Start(ctx, "123", deps)
	if err != nil || !proof.Verified || proof.ExchangeAccountID != "123" {
		t.Fatal("correctly bound account refused", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("verified connector did not subscribe")
	}
}

func TestPrivateStreamChargesConnectionAndSubscriptionWeight(t *testing.T) {
	calls := make(chan struct{}, 1)
	rest, uri := privateStreamFixture(t, func(ctx context.Context, conn *websocket.Conn) {
		calls <- struct{}{}
		_ = conn.Write(ctx, websocket.MessageText, []byte(confirmed))
	})
	rest.bucket = newWeightBucket(3, time.Minute, nil)
	if _, _, err := rest.subscribeUserData(t.Context(), uri); !errors.Is(err, ErrRateLimited) {
		t.Fatal("insufficient budget reached the private stream")
	}
	select {
	case <-calls:
		t.Fatal("request sent without full connection and subscription budget")
	default:
	}
	rest.bucket = newWeightBucket(4, time.Minute, rest.now)
	_, stop, err := rest.subscribeUserData(t.Context(), uri)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if rest.bucket.Allow(1) {
		t.Fatal("private stream did not charge full request weight")
	}
}

func TestPrivateStreamConfirmation(t *testing.T) {
	for _, tc := range []struct{ name, reply string }{
		{"other request", `{"id":"other","status":200,"result":{"subscriptionId":0}}`},
		{"denied", `{"id":"private-stream","status":401,"error":{"msg":"fixture-secret"}}`},
		{"mixed outcome", `{"id":"private-stream","status":200,"result":{"subscriptionId":0},"error":{"msg":"fixture-secret"}}`},
		{"missing id", `{"id":"private-stream","status":200,"result":{}}`},
		{"null id", `{"id":"private-stream","status":200,"result":{"subscriptionId":null}}`},
		{"negative id", `{"id":"private-stream","status":200,"result":{"subscriptionId":-1}}`},
		{"duplicate id", `{"id":"other","id":"private-stream","status":200,"result":{"subscriptionId":0}}`},
		{"wrong case", `{"id":"private-stream","status":200,"result":{"subscriptionID":0}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closed := make(chan error, 1)
			c, uri := privateStreamFixture(t, func(ctx context.Context, conn *websocket.Conn) {
				if err := conn.Write(ctx, websocket.MessageText, []byte(tc.reply)); err != nil {
					t.Error(err)
					return
				}
				_, _, err := conn.Read(ctx)
				if ctx.Err() != nil {
					err = ctx.Err()
				}
				closed <- err
			})
			stream, stop, err := c.subscribeUserData(t.Context(), uri)
			if err == nil || strings.Contains(err.Error(), "fixture") || stream != nil || stop != nil {
				t.Fatal("unconfirmed subscription accepted or remote detail disclosed")
			}
			select {
			case err := <-closed:
				if err == nil || errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("client did not close rejected session")
				}
			case <-time.After(6 * time.Second):
				t.Fatal("server did not observe disconnect")
			}
		})
	}
}

func TestPrivateStreamEvents(t *testing.T) {
	for _, tc := range []struct {
		name, frame string
		typ         websocket.MessageType
		ok          bool
	}{
		{"account event", `{"subscriptionId":0,"event":{"e":"outboundAccountPosition","E":5,"B":[]}}`, websocket.MessageText, true},
		{"other subscription", `{"subscriptionId":1,"event":{"e":"executionReport"}}`, websocket.MessageText, false},
		{"duplicate subscription", `{"subscriptionId":1,"subscriptionId":0,"event":{"e":"executionReport"}}`, websocket.MessageText, false},
		{"subscription missing", `{"event":{"e":"executionReport"}}`, websocket.MessageText, false},
		{"unwrapped", `{"e":"executionReport"}`, websocket.MessageText, false},
		{"no event type", `{"subscriptionId":0,"event":{"E":5}}`, websocket.MessageText, false},
		{"terminated", `{"subscriptionId":0,"event":{"e":"eventStreamTerminated","E":7}}`, websocket.MessageText, false},
		{"expired", `{"subscriptionId":0,"event":{"e":"listenKeyExpired","E":7}}`, websocket.MessageText, false},
		{"shutdown", `{"event":{"e":"serverShutdown","E":6,"T":7}}`, websocket.MessageText, false},
		{"binary", `{"subscriptionId":0,"event":{"e":"executionReport"}}`, websocket.MessageBinary, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rest, uri := privateStreamFixture(t, func(ctx context.Context, conn *websocket.Conn) {
				_ = conn.Write(ctx, websocket.MessageText, []byte(confirmed))
				_ = conn.Write(ctx, tc.typ, []byte(tc.frame))
			})
			stream, stop, err := rest.subscribeUserData(t.Context(), uri)
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			raw, err := stream.Recv(t.Context())
			if tc.ok != (err == nil) || tc.ok != (len(raw) > 0) {
				t.Fatal("unexpected private event disposition", err)
			}
		})
	}
}

func TestPrivateStreamCancellationAndMalformedExecution(t *testing.T) {
	for _, cancelRead := range []bool{true, false} {
		t.Run(fmt.Sprintf("cancel=%t", cancelRead), func(t *testing.T) {
			closed := make(chan error, 1)
			rest, uri := privateStreamFixture(t, func(ctx context.Context, conn *websocket.Conn) {
				reply := strings.Replace(confirmed, `"subscriptionId":0`, `"subscriptionId":7`, 1)
				if err := conn.Write(ctx, websocket.MessageText, []byte(reply)); err != nil {
					t.Error(err)
					return
				}
				if !cancelRead {
					frame := `{"subscriptionId":7,"event":{"e":"executionReport","x":"TRADE","t":"fixture-secret"}}`
					if err := conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
						t.Error(err)
						return
					}
				}
				_, _, err := conn.Read(ctx)
				if ctx.Err() != nil {
					err = ctx.Err()
				}
				closed <- err
			})
			stream, stop, err := rest.subscribeUserData(t.Context(), uri)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if cancelRead {
				cancel()
				_, err = stream.Recv(ctx)
				if !errors.Is(err, context.Canceled) {
					t.Fatal("private read did not honor cancellation")
				}
			} else {
				ing := newUserDataIngester(UserDataConfig{Stream: stream})
				if err := ing.Run(ctx); err == nil || err.Error() != "binance: malformed private execution report" {
					t.Fatal("malformed execution must end the session with a redacted error")
				}
			}
			stop()
			select {
			case err := <-closed:
				if err == nil || errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("peer did not observe session teardown")
				}
			case <-time.After(6 * time.Second):
				t.Fatal("socket remained open")
			}
		})
	}
}

func TestPrivateStreamExecutionRequiresWireIdentity(t *testing.T) {
	const trade = `{"e":"executionReport","x":"TRADE","s":"BTCUSDT","c":"o1","X":"FILLED","l":"1","L":"2","z":"1","t":0,"T":1700000000000}`
	for _, field := range []string{`"x":"TRADE",`, `"s":"BTCUSDT",`, `"c":"o1",`, `"X":"FILLED",`, `"t":0,`, `,"T":1700000000000`} {
		ing := newUserDataIngester(UserDataConfig{})
		if err := ing.handle(t.Context(), []byte(strings.Replace(trade, field, "", 1))); err == nil || err.Error() != "binance: malformed private execution report" {
			t.Fatalf("missing %s did not end the session", field)
		}
	}
	ing := newUserDataIngester(UserDataConfig{Orders: newFakeOrders("o1"), Pub: &reconCapture{}})
	if err := ing.handle(t.Context(), []byte(trade)); err != nil || !ing.resolvedAReport() {
		t.Fatal("valid exchange trade zero was refused", err)
	}
	for _, field := range []string{"l", "L"} {
		raw := strings.Replace(trade, fmt.Sprintf(`"%s":"`, field), fmt.Sprintf(`"%s":"fixture-secret`, field), 1)
		if err := ing.handle(t.Context(), []byte(raw)); err == nil || strings.Contains(err.Error(), "fixture-secret") {
			t.Fatalf("invalid %s was accepted or exposed", field)
		}
	}
}

func TestPrivateStreamOriginAndRedirect(t *testing.T) {
	const testREST = "https://testnet.binance.vision"
	for _, ws := range []string{"wss://ws-api.binance.com", "ws://ws-api.testnet.binance.vision", "wss://ws-api.testnet.binance.vision/wrong", "wss://example.test", "wss://user@ws-api.testnet.binance.vision", "wss://ws-api.testnet.binance.vision?key=x"} {
		if _, err := privateStreamEndpoint(testREST, ws); err == nil {
			t.Fatalf("accepted invalid test origin %s", ws)
		}
	}
	if got, err := privateStreamEndpoint(testREST, "wss://ws-api.testnet.binance.vision"); err != nil || got != "wss://ws-api.testnet.binance.vision/ws-api/v3" {
		t.Fatal("testnet origin refused")
	}
	if _, err := privateStreamEndpoint("https://api.binance.com:443", "wss://ws-api.binance.com:443"); err != nil {
		t.Fatal("explicit default port refused")
	}
	connector := NewBinanceConnector(VenueSettings{BaseURL: testREST}, "wss://ws-api.binance.com")
	if _, err := connector.Start(t.Context(), "123", WorkerDeps{Orders: newFakeOrders()}); err == nil {
		t.Fatal("connector started with a private stream in a different environment")
	}
	redirected := make(chan bool, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirected" {
			redirected <- true
			return
		}
		http.Redirect(w, r, "/redirected", http.StatusFound)
	}))
	defer server.Close()
	rest := newBinanceREST(restConfig{HTTPClient: server.Client(), APIKey: "fixture", APISecret: "fixture-secret", Bucket: newWeightBucket(100, time.Minute, nil)})
	_, _, err := rest.subscribeUserData(t.Context(), "wss"+strings.TrimPrefix(server.URL, "https")+"/ws-api/v3")
	if err == nil {
		t.Fatal("redirect accepted")
	}
	select {
	case <-redirected:
		t.Fatal("redirect followed")
	default:
	}
}

func TestPrivateStreamReplayOnJetStream(t *testing.T) {
	server, err := ns.NewServer(&ns.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	server.Start()
	t.Cleanup(func() { server.Shutdown(); server.WaitForShutdown() })
	if !server.ReadyForConnections(5 * time.Second) {
		t.Fatal("local JetStream unavailable")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	nc, err := nats.Connect(server.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	history, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "EXECUTION", Subjects: []string{fillfact.SubjectFilled}})
	if err != nil {
		t.Fatal(err)
	}
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: server.ClientURL(), Name: "private-stream-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	prod, err := bus.NewProducer(client, bus.ProducerConfig{Source: "venue-binance", ProducerVersion: "test", Tenant: "fund-alpha"})
	if err != nil {
		t.Fatal(err)
	}
	const fill = `{"e":"executionReport","E":1700000000002,"s":"BTCUSDT","c":"o1","S":"BUY","x":"TRADE","X":"FILLED","l":"1","L":"50000","z":"1","q":"1","t":8,"T":1700000000000}`
	rest, uri := privateStreamFixture(t, func(ctx context.Context, conn *websocket.Conn) {
		for _, frame := range []string{confirmed, `{"subscriptionId":0,"event":` + fill + "}", `{"subscriptionId":1,"event":` + fill + "}"} {
			if err := conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
				t.Error(err)
				return
			}
		}
	})
	// The credential used to subscribe belongs to the same REST client that resolved the bound account.
	proof, err := accountproof.Resolve(ctx, rest, accountproof.Want{Account: "fixture-account", ExchangeUID: "123"}, slog.Default())
	if err != nil || !proof.Verified {
		t.Fatal("fixture account proof failed", err)
	}
	view := newFakeOrders("o1")
	for range 2 {
		stream, stop, err := rest.subscribeUserData(ctx, uri)
		if err != nil {
			t.Fatal(err)
		}
		ing := newUserDataIngester(UserDataConfig{Stream: stream, Orders: view, Pub: prod, Tenant: "fund-alpha"})
		err = ing.Run(ctx)
		stop()
		if err == nil || !strings.Contains(err.Error(), "unexpected private subscription") || !ing.resolvedAReport() {
			t.Fatal("session did not process and then close at the foreign envelope", err)
		}
	}
	info, err := history.Info(ctx)
	if err != nil || info.State.Msgs != 2 {
		t.Fatal("want one authenticated fill per subscription", info, err)
	}
	var first *orderpb.Fill
	for seq := uint64(1); seq <= 2; seq++ {
		msg, err := history.GetMsg(ctx, seq)
		if err != nil {
			t.Fatal(err)
		}
		env, bytes, err := bus.Unframe(msg.Data)
		if err != nil || env.GetTenantId() != "fund-alpha" || env.GetPartitionKey() != "o1" {
			t.Fatal("fill envelope scope changed", err)
		}
		var payload orderpb.OrderFilled
		if err := proto.Unmarshal(bytes, &payload); err != nil {
			t.Fatal(err)
		}
		fill := payload.GetFill()
		if fill.GetFillId() != "BTCUSDT-8" || fill.GetVenueExecutionId() != "8" || fill.GetOrderId() != "o1" || dec.Cmp(fill.GetQuantity(), bdec("1")) != 0 || fill.GetExecutedAt().AsTime().UnixMilli() != fixtureTime {
			t.Fatal("fill attribution changed", fill)
		}
		if first != nil && (!fillfact.SameExecution(first, fill) || fillfact.ExecutionKey(first) != fillfact.ExecutionKey(fill)) {
			t.Fatal("replayed trade had a different economic identity")
		}
		first = fill
	}
	if dec.Cmp(view.get(t, "o1").GetFilledQuantity(), bdec("1")) != 0 {
		t.Fatal("duplicate trade increased the adapter's cumulative fill")
	}
}
