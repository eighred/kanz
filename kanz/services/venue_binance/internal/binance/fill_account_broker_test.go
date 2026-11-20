package binance

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/internal/venueadapter/orderview"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/test/backing"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

func TestVerifiedAccountsShareRawTradeAliasAcrossAdapterRestart(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("requires isolated JetStream and restricted PostgreSQL")
	}
	open := backing.VenueStore(t, "../../migrations")
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	bustest.EnsureSubjects(t, ctx, js, "SCOPED_BINANCE_FILLS", []string{fillfact.SubjectFilled})
	stream, err := bustest.StreamFor(ctx, js, fillfact.SubjectFilled)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := js.CreateConsumer(ctx, stream, jetstream.ConsumerConfig{
		FilterSubject: fillfact.SubjectFilled, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "scoped-binance"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	pub, err := bus.NewProducer(client, bus.ProducerConfig{Source: "venue-binance", ProducerVersion: "fixture", Tenant: "fund-alpha"})
	if err != nil {
		t.Fatal(err)
	}
	keys := make(map[string]string)
	for index, account := range []string{"desk-a", "desk-b"} {
		uid := 101 + index
		id := "order-" + account
		state := marketOrder(id)
		state.PortfolioId, state.VenueAccountId = "fund-alpha", account
		store := open()
		if err := store.Record(ctx, state); err != nil {
			t.Fatal(err)
		}
		legacy := proto.Clone(state).(*orderpb.OrderState)
		legacy.OrderId, legacy.VenueAccountId = "unscoped-"+account, ""
		if err := store.Record(ctx, legacy); err != nil {
			t.Fatal(err)
		}
		var submitted atomic.Int32
		// Each account really returns the same symbol and trade 0. The order
		// update time differs from the signed per-match time.
		report := func(order string) string {
			return fmt.Sprintf(`{"e":"executionReport","E":1700000000001,"s":"BTCUSDT","c":%q,"S":"BUY","x":"TRADE","X":"FILLED","l":"1","L":"50000","z":"1","q":"1","t":0,"T":1700000000000,"n":"0.1","N":"USDT"}`, order)
		}
		rest, _ := privateAccountStreamFixture(t, uid, func(session context.Context, conn *websocket.Conn) {
			for _, frame := range []string{confirmed, `{"subscriptionId":0,"event":` + report(legacy.OrderId) + "}", `{"subscriptionId":0,"event":` + report(id) + "}"} {
				if err := conn.Write(session, websocket.MessageText, []byte(frame)); err != nil {
					return
				}
			}
			_, _, _ = conn.Read(session)
		}, func(mux *http.ServeMux) {
			mux.HandleFunc("/api/v3/", func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Error(err)
					return
				}
				q := r.Form
				signature := q.Get("signature")
				q.Del("signature")
				mac := hmac.New(sha256.New, []byte("fixture-secret"))
				fmt.Fprint(mac, q.Encode())
				if r.Header.Get("X-MBX-APIKEY") != "fixture" || signature != hex.EncodeToString(mac.Sum(nil)) || q.Get("symbol") != "BTCUSDT" {
					t.Error("signed fixture request had wrong scope")
					w.WriteHeader(401)
					return
				}
				switch r.URL.Path {
				case "/api/v3/order":
					if r.Method == "POST" {
						submitted.Add(1)
						if q.Get("newClientOrderId") != id {
							t.Error("wrong placement identity")
						}
					} else if q.Get("origClientOrderId") != id {
						t.Error("wrong query identity")
					}
					fmt.Fprintf(w, `{"symbol":"BTCUSDT","orderId":42,"clientOrderId":%q,"status":"FILLED","side":"BUY","origQty":"1","executedQty":"1","transactTime":1700000000010,"fills":[{"price":"50000","qty":"1","commission":"0.1","commissionAsset":"USDT","tradeId":0}]}`, id)
				case "/api/v3/myTrades":
					if q.Get("orderId") != "42" {
						t.Error("wrong trade history identity")
					}
					fmt.Fprint(w, `[{"symbol":"BTCUSDT","orderId":42,"id":0,"price":"50000","qty":"1","commission":"0.1","commissionAsset":"USDT","isBuyer":true,"time":1700000000000}]`)
				default:
					http.NotFound(w, r)
				}
			})
		})
		var previous *orderpb.Fill
		for generation := 0; generation < 2; generation++ {
			durable := open()
			conn := NewBinanceConnector(VenueSettings{MIC: "BINANCE", Account: account, BaseURL: rest.baseURL, APIKey: "fixture", APISecret: "fixture-secret", Symbols: map[string]string{"BTC-USD": "BTCUSDT"}, WeightBudget: 1200}, "wss"+strings.TrimPrefix(rest.baseURL, "https"), durable)
			conn.rest.httpc, conn.rest.now = rest.httpc, rest.now
			before := submitted.Load()
			if _, err := conn.venue.Execute(ctx, state); !errors.Is(err, execution.ErrReconcileEvidence) || submitted.Load() != before {
				t.Fatal("unproven order reached signed placement", err)
			}
			session, stop := context.WithCancel(ctx)
			proof, err := conn.Start(session, strconv.Itoa(uid), WorkerDeps{Orders: orderview.NewSeam(durable, func(err error) { t.Error(err) }), Publisher: pub, Tenant: "fund-alpha", TickerInterval: time.Hour})
			if err != nil || !proof.Verified || proof.ExchangeAccountID != strconv.Itoa(uid) {
				stop()
				t.Fatal("expected bound exchange account proof", err)
			}
			for _, foreign := range []string{"", "other"} {
				invalid := proto.Clone(state).(*orderpb.OrderState)
				invalid.VenueAccountId = foreign
				if _, err := conn.venue.Execute(ctx, invalid); !errors.Is(err, execution.ErrReconcileEvidence) || submitted.Load() != before {
					stop()
					t.Fatal("foreign/unscoped order reached placement", err)
				}
			}
			var direct []*orderpb.Fill
			if generation == 0 {
				direct, err = conn.venue.Execute(ctx, state)
			} else {
				var view OrderView
				view, err = conn.venue.QueryOrder(ctx, state)
				direct = view.Fills
			}
			if err != nil || len(direct) != 1 {
				stop()
				t.Fatalf("generation %d signed trade receipt: %v (%d fills)", generation, err, len(direct))
			}
			msg, err := reader.Next(jetstream.FetchMaxWait(3 * time.Second))
			if err != nil {
				stop()
				t.Fatal(err)
			}
			env, body, err := bus.Unframe(msg.Data())
			var fact orderpb.OrderFilled
			if err != nil || env.GetTenantId() != "fund-alpha" || env.GetPartitionKey() != id || proto.Unmarshal(body, &fact) != nil {
				stop()
				t.Fatal("unexpected broker fill identity", err)
			}
			fill := fact.GetFill()
			if fill.GetFillId() != "BTCUSDT-0" || fill.GetVenueExecutionId() != "0" || fill.GetVenueAccountId() != account || fill.GetExecutedAt().AsTime().UnixMilli() != fixtureTime ||
				!fillfact.SameExecution(direct[0], fill) || previous != nil && !fillfact.SameExecution(previous, fill) {
				stop()
				t.Fatalf("signed fill mismatch: direct=%v broker=%v", direct[0], fill)
			}
			key := fillfact.ExecutionKey(fill)
			if key == fill.FillId || keys[account] != "" && keys[account] != key {
				stop()
				t.Fatal("scoped key changed across restart")
			}
			keys[account] = key
			previous = fill
			if err := msg.DoubleAck(ctx); err != nil {
				stop()
				t.Fatal(err)
			}
			stop()
			rejected, _, ok, err := durable.Get(ctx, legacy.OrderId)
			if err != nil || !ok || rejected.GetQuarantine() == nil || rejected.GetVenueAccountId() != "" {
				t.Fatal("legacy order was not retained and held", err)
			}
		}
		if submitted.Load() != 1 {
			t.Fatal("restart submitted another order")
		}
	}
	if keys["desk-a"] == keys["desk-b"] {
		t.Fatal("separate verified accounts collided on trade zero")
	}
}
