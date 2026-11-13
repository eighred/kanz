package okx

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const ordersSubscriptionACK = `{"event":"subscribe","arg":{"channel":"orders","instType":"SPOT"},"connId":"fixture"}`

func subscriptionPeer(t *testing.T, subscribed func(context.Context, *websocket.Conn) error) (string, <-chan error) {
	t.Helper()
	return handshakePeer(t, func(ctx context.Context, conn *websocket.Conn) error {
		if err := conn.Write(ctx, websocket.MessageText, []byte(`{"event":"login","code":"0"}`)); err != nil {
			return errors.New("fixture could not acknowledge login")
		}
		_, sub, err := conn.Read(ctx)
		if err != nil || !containsOp(sub, "subscribe") {
			return errors.New("fixture did not receive subscription")
		}
		return subscribed(ctx, conn)
	})
}

func TestOKXSubscriptionPreservesOrderAroundACK(t *testing.T) {
	first := `{"arg":{"channel":"orders"},"data":[{"clOrdId":"first","state":"live"}]}`
	second := `{"arg":{"channel":"orders"},"data":[{"clOrdId":"second","state":"live"}]}`
	for _, early := range []bool{false, true} {
		name := "ACK_first"
		if early {
			name = "order_first"
		}
		t.Run(name, func(t *testing.T) {
			url, done := subscriptionPeer(t, func(ctx context.Context, conn *websocket.Conn) error {
				frames := []string{ordersSubscriptionACK, "pong", `{"event":"channel-conn-count","channel":"orders","connCount":"1"}`, first, second}
				if early {
					frames = []string{first, ordersSubscriptionACK, second}
				}
				for _, frame := range frames {
					if err := conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
						return errors.New("fixture could not write subscription/order frame")
					}
				}
				return handshakePeerClosed(ctx, conn)
			})
			ws := handshakeClient(url)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stop, err := ws.Connect(ctx)
			if err != nil {
				t.Fatalf("valid subscription refused: %v", err)
			}
			defer stop()
			for _, want := range []string{first, second} {
				got, err := ws.Recv(ctx)
				if err != nil || string(got) != want {
					t.Error("subscription lost, reordered or duplicated an order frame")
				}
			}
			stop()
			awaitHandshakePeer(t, done)
		})
	}
}

func TestOKXSubscriptionBoundsPreACKData(t *testing.T) {
	order := `{"arg":{"channel":"orders"},"data":[{"clOrdId":"early","state":"live"}]}`
	for _, next := range []struct{ name, frame string }{
		{"second_order", order},
		{"error_after_order", `{"event":"error","code":"60012","msg":"` + handshakeSecret + `"}`},
	} {
		t.Run(next.name, func(t *testing.T) {
			url, done := subscriptionPeer(t, func(ctx context.Context, conn *websocket.Conn) error {
				for _, frame := range []string{order, next.frame} {
					if err := conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
						return errors.New("fixture could not write pre-ACK frame")
					}
				}
				return handshakePeerClosed(ctx, conn)
			})
			ws := handshakeClient(url)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stop, err := ws.Connect(ctx)
			assertHandshakeRefused(t, ws, stop, err)
			awaitHandshakePeer(t, done)
		})
	}
}

func TestOKXSubscriptionFirstExecutionReachesIngester(t *testing.T) {
	for _, early := range []bool{false, true} {
		name := "ACK_first"
		if early {
			name = "execution_first"
		}
		t.Run(name, func(t *testing.T) {
			fill, _ := (endlessFills{}).Recv(context.Background())
			url, done := subscriptionPeer(t, func(ctx context.Context, conn *websocket.Conn) error {
				frames := []string{ordersSubscriptionACK, string(fill)}
				if early {
					frames = []string{string(fill), ordersSubscriptionACK}
				}
				frames = append(frames, `{"event":"channel-conn-count-error","channel":"orders"}`)
				for _, frame := range frames {
					if err := conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
						return errors.New("fixture could not write first execution")
					}
				}
				return handshakePeerClosed(ctx, conn)
			})
			ws := handshakeClient(url)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stop, err := ws.Connect(ctx)
			if err != nil {
				t.Fatal("valid subscription did not connect")
			}
			defer stop()
			pub := &okxCapture{}
			ing := newOKXUserDataIngester(OKXUserDataConfig{Stream: ws, Orders: newOKXOrders("o1"), Pub: pub,
				Venue: "OKX", Tenant: "fixture", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			err = ing.Run(ctx)
			if err == nil || len(pub.events) != 1 || !ing.resolvedAReport() {
				t.Error("subscription did not preserve exactly one real ingester execution before channel closure")
			}
			awaitHandshakePeer(t, done)
		})
	}
}

func TestOKXSubscriptionChannelErrorEndsIngester(t *testing.T) {
	for _, tc := range []struct{ name, frame string }{
		{"error", `{"event":"error","code":"60012","msg":"` + handshakeSecret + `"}`},
		{"connection_limit", `{"event":"channel-conn-count-error","channel":"orders","msg":"` + handshakeSecret + `"}`},
		{"unsubscribe", `{"event":"unsubscribe","arg":{"channel":"orders","instType":"SPOT"}}`},
		{"malformed", `{"event":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, done := subscriptionPeer(t, func(ctx context.Context, conn *websocket.Conn) error {
				for _, frame := range []string{ordersSubscriptionACK, tc.frame} {
					if err := conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
						return errors.New("fixture could not write channel frame")
					}
				}
				return handshakePeerClosed(ctx, conn)
			})
			ws := handshakeClient(url)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			stop, err := ws.Connect(ctx)
			if err != nil {
				t.Fatal("valid subscription did not reach active session")
			}
			defer stop()
			pub := &okxCapture{}
			ing := newOKXUserDataIngester(OKXUserDataConfig{Stream: ws, Orders: newOKXOrders("o1"), Pub: pub,
				Venue: "OKX", Tenant: "fixture", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			err = ing.Run(ctx)
			if err == nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				t.Error("channel error did not immediately leave Run for the connector backoff path")
			}
			assertHandshakeRefused(t, ws, nil, err)
			if ing.resolvedAReport() || len(pub.events) != 0 {
				t.Error("channel refusal counted as resolved execution progress")
			}
			if raw, err := ws.Recv(ctx); err == nil || raw != nil {
				t.Error("refused channel retained a usable session")
			}
			awaitHandshakePeer(t, done)
		})
	}
}

func TestOKXSubscriptionChannelErrorReachesReconnectLoop(t *testing.T) {
	url, peerDone := subscriptionPeer(t, func(ctx context.Context, conn *websocket.Conn) error {
		for _, frame := range []string{ordersSubscriptionACK, `{"event":"channel-conn-count-error","channel":"orders"}`} {
			if err := conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
				return errors.New("fixture could not write subscription/channel error")
			}
		}
		return handshakePeerClosed(ctx, conn)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	dials := 0
	c := &OKXConnector{settings: VenueSettings{MIC: "OKX"}}
	c.dialUserData = func(ctx context.Context) (UserDataStream, func(), error) {
		dials++
		if dials > 1 {
			cancel() // Observe re-entry without opening another fixture connection.
			return nil, nil, ctx.Err()
		}
		ws := handshakeClient(url)
		stop, err := ws.Connect(ctx)
		return ws, stop, err
	}
	pub := &okxCapture{}
	c.runUserData(ctx, WorkerDeps{Orders: newOKXOrders("o1"), Publisher: pub,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if dials != 2 || len(pub.events) != 0 {
		t.Error("channel refusal did not close and re-enter the existing reconnect loop without a fill")
	}
	awaitHandshakePeer(t, peerDone)
}

func TestOKXSubscriptionReadCancellation(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			ready := make(chan struct{})
			url, done := subscriptionPeer(t, func(ctx context.Context, conn *websocket.Conn) error {
				close(ready)
				return handshakePeerClosed(ctx, conn)
			})
			ws := handshakeClient(url)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() {
				stop, err := ws.Connect(ctx)
				if stop != nil {
					stop()
				}
				result <- err
			}()
			<-ready
			want := context.DeadlineExceeded
			if mode == "cancel" {
				want = context.Canceled
				cancel()
			}
			err := <-result
			assertHandshakeRefused(t, ws, nil, err)
			if !errors.Is(err, want) {
				t.Error("subscription read lost context cancellation identity")
			}
			awaitHandshakePeer(t, done)
		})
	}
}

func TestOKXSubscriptionActiveReadCancellation(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline", "stop_with_pending_order"} {
		t.Run(mode, func(t *testing.T) {
			url, done := subscriptionPeer(t, func(ctx context.Context, conn *websocket.Conn) error {
				if mode == "stop_with_pending_order" {
					if err := conn.Write(ctx, websocket.MessageText, []byte(`{"arg":{"channel":"orders"},"data":[{"clOrdId":"early","state":"live"}]}`)); err != nil {
						return errors.New("fixture could not write initial order")
					}
				}
				if err := conn.Write(ctx, websocket.MessageText, []byte(ordersSubscriptionACK)); err != nil {
					return errors.New("fixture could not acknowledge subscription")
				}
				return handshakePeerClosed(ctx, conn)
			})
			ws := handshakeClient(url)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stop, err := ws.Connect(ctx)
			if err != nil {
				t.Fatal("valid subscription did not connect")
			}
			defer stop()
			readCtx, cancelRead := context.WithTimeout(ctx, time.Second)
			defer cancelRead()
			want := context.DeadlineExceeded
			switch mode {
			case "cancel":
				cancelRead()
				want = context.Canceled
			case "stop_with_pending_order":
				stop()
				want = context.Canceled
			}
			raw, err := ws.Recv(readCtx)
			if raw != nil || !errors.Is(err, want) {
				t.Error("stopped/cancelled session returned data or lost cancellation identity")
			}
			assertHandshakeRefused(t, ws, nil, err)
			awaitHandshakePeer(t, done)
		})
	}
}

func TestOKXSubscriptionActivePeerCloseIsRedacted(t *testing.T) {
	url, done := subscriptionPeer(t, func(ctx context.Context, conn *websocket.Conn) error {
		if err := conn.Write(ctx, websocket.MessageText, []byte(ordersSubscriptionACK)); err != nil {
			return errors.New("fixture could not acknowledge subscription")
		}
		_ = conn.Close(websocket.StatusPolicyViolation, handshakeSecret)
		return nil
	})
	ws := handshakeClient(url)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stop, err := ws.Connect(ctx)
	if err != nil {
		t.Fatal("valid subscription did not connect")
	}
	defer stop()
	raw, err := ws.Recv(ctx)
	if raw != nil {
		t.Error("peer close became an order frame")
	}
	assertHandshakeRefused(t, ws, nil, err)
	awaitHandshakePeer(t, done)
}

func TestOKXSubscriptionSilentPeerHasBoundedWait(t *testing.T) {
	done := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			done <- errors.New("fixture could not accept socket")
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		_, _, err = conn.Read(ctx)
		if err == nil {
			err = conn.Write(ctx, websocket.MessageText, []byte(`{"event":"login","code":"0"}`))
		}
		if err == nil {
			_, _, err = conn.Read(ctx)
		}
		if err != nil {
			done <- errors.New("fixture did not reach subscription")
			return
		}
		done <- handshakePeerClosed(ctx, conn)
	}))
	defer srv.Close()
	ws := handshakeClient("ws" + strings.TrimPrefix(srv.URL, "http"))
	// This outer deadline is longer than the production ACK bound, and must
	// remain live after Connect refuses the silent peer.
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Second)
	defer cancel()
	stop, err := ws.Connect(ctx)
	assertHandshakeRefused(t, ws, stop, err)
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Error("silent peer did not hit the subscription phase's own deadline")
	}
	awaitHandshakePeer(t, done)
}

func TestOKXSubscriptionRejectsInvalidACK(t *testing.T) {
	for _, tc := range []struct{ name, ack string }{
		{"error", `{"event":"error","code":"60012","msg":"` + handshakeSecret + `"}`},
		{"malformed", `{"event":"subscribe",`},
		{"wrong_event", `{"event":"login","code":"0"}`},
		{"wrong_channel", `{"event":"subscribe","arg":{"channel":"account","instType":"SPOT"}}`},
		{"wrong_instrument_type", `{"event":"subscribe","arg":{"channel":"orders","instType":"SWAP"}}`},
		{"missing_arg", `{"event":"subscribe"}`},
		{"missing_channel", `{"event":"subscribe","arg":{"instType":"SPOT"}}`},
		{"missing_type", `{"event":"subscribe","arg":{"channel":"orders"}}`},
		{"missing_event", `{"arg":{"channel":"orders","instType":"SPOT"}}`},
		{"nonzero_code", `{"event":"subscribe","arg":{"channel":"orders","instType":"SPOT"},"code":"60012"}`},
		{"numeric_code", `{"event":"subscribe","arg":{"channel":"orders","instType":"SPOT"},"code":0}`},
		{"narrowed_instrument", `{"event":"subscribe","arg":{"channel":"orders","instType":"SPOT","instId":"BTC-USDT"}}`},
		{"narrowed_family", `{"event":"subscribe","arg":{"channel":"orders","instType":"SPOT","instFamily":"BTC-USDT"}}`},
		{"null", `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sent := make(chan struct{})
			url, done := subscriptionPeer(t, func(ctx context.Context, conn *websocket.Conn) error {
				err := conn.Write(ctx, websocket.MessageText, []byte(tc.ack))
				close(sent)
				if err != nil {
					return errors.New("fixture could not write subscription response")
				}
				return handshakePeerClosed(ctx, conn)
			})
			ws := handshakeClient(url)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stop, err := ws.Connect(ctx)
			<-sent
			assertHandshakeRefused(t, ws, stop, err)
			awaitHandshakePeer(t, done)
		})
	}
}
