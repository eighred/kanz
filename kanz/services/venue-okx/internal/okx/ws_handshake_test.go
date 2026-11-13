package okx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const handshakeSecret = "synthetic-peer-secret-do-not-reflect"

// These are dummy fixture values, never credentials for a venue account.
func handshakeClient(url string) *okxUserDataWS {
	return newOKXUserDataWS(url, "fixture-api-key", "fixture-api-secret", "fixture-passphrase")
}

// Each fixture reads the real signed login over a loopback socket. It never
// returns or logs that frame, including when the handshake fails.
func handshakePeer(t *testing.T, afterLogin func(context.Context, *websocket.Conn) error) (string, <-chan error) {
	t.Helper()
	done := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			done <- errors.New("fixture could not accept socket")
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		_, login, err := conn.Read(ctx)
		if err != nil || !containsOp(login, "login") {
			done <- errors.New("fixture did not receive login")
			return
		}
		done <- afterLogin(ctx, conn)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http"), done
}

func handshakePeerClosed(ctx context.Context, conn *websocket.Conn) error {
	_, _, err := conn.Read(ctx)
	if err == nil {
		return errors.New("client sent a frame after failed login")
	}
	if ctx.Err() != nil {
		return errors.New("client did not close failed socket")
	}
	return nil
}

func awaitHandshakePeer(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(6 * time.Second):
		t.Error("handshake fixture did not terminate")
	}
}

func assertHandshakeRefused(t *testing.T, ws *okxUserDataWS, stop func(), err error) {
	t.Helper()
	if err == nil {
		t.Error("Connect accepted a failed handshake")
	} else {
		for _, secret := range []string{handshakeSecret, "fixture-api-key", "fixture-api-secret", "fixture-passphrase"} {
			if strings.Contains(err.Error(), secret) {
				t.Error("handshake error reflected secret fixture text")
			}
		}
	}
	if stop != nil {
		t.Error("failed handshake returned a keepalive stopper")
		stop()
	}
	if ws.conn != nil {
		t.Error("failed handshake retained a usable session")
		_ = ws.conn.CloseNow()
	}
}

func TestOKXHandshakeRejectsInvalidLoginACK(t *testing.T) {
	for _, tc := range []struct {
		name string
		ack  string
	}{
		{"error", `{"event":"error","code":"60009","msg":"` + handshakeSecret + `"}`},
		{"malformed", `{"event":"login","code":`},
		{"wrong_event", `{"event":"subscribe","code":"0"}`},
		{"wrong_code", `{"event":"login","code":"60009"}`},
		{"empty_code", `{"event":"login","code":""}`},
		{"missing_event", `{"code":"0"}`},
		{"missing_code", `{"event":"login"}`},
		{"numeric_code", `{"event":"login","code":0}`},
		{"null_code", `{"event":"login","code":null}`},
		{"null", `null`},
		{"array", `[]`},
		{"trailing_data", `{"event":"login","code":"0"} {}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, done := handshakePeer(t, func(ctx context.Context, conn *websocket.Conn) error {
				if err := conn.Write(ctx, websocket.MessageText, []byte(tc.ack)); err != nil {
					return errors.New("fixture could not write ACK")
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

func TestOKXHandshakePreservesFirstOrderFrame(t *testing.T) {
	frame := `{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","clOrdId":"fixture-order","state":"filled","fillSz":"1"}]}`
	url, done := handshakePeer(t, func(ctx context.Context, conn *websocket.Conn) error {
		if err := conn.Write(ctx, websocket.MessageText, []byte(`{"event":"login","code":"0","msg":"","connId":"fixture"}`)); err != nil {
			return errors.New("fixture could not write ACK")
		}
		_, sub, err := conn.Read(ctx)
		if err != nil || !containsOp(sub, "subscribe") {
			return errors.New("fixture did not receive subscription")
		}
		if err := conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
			return errors.New("fixture could not write order frame")
		}
		return handshakePeerClosed(ctx, conn)
	})
	ws := handshakeClient(url)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stop, err := ws.Connect(ctx)
	if err != nil {
		t.Fatalf("valid login was refused: %v", err)
	}
	defer stop()
	defer func() { _ = ws.conn.CloseNow() }()
	got, err := ws.Recv(ctx)
	if err != nil || string(got) != frame {
		t.Error("successful handshake did not preserve first order frame")
	}
	stop()
	_ = ws.conn.CloseNow()
	awaitHandshakePeer(t, done)
}

func TestOKXHandshakeReadCancellationClosesSocket(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			loginRead := make(chan struct{})
			url, done := handshakePeer(t, func(ctx context.Context, conn *websocket.Conn) error {
				close(loginRead)
				return handshakePeerClosed(ctx, conn)
			})
			ws := handshakeClient(url)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			type outcome struct {
				stop func()
				err  error
			}
			connected := make(chan outcome, 1)
			go func() {
				stop, err := ws.Connect(ctx)
				connected <- outcome{stop, err}
			}()
			select {
			case <-loginRead:
			case <-time.After(5 * time.Second):
				t.Fatal("fixture did not reach login read")
			}
			want := context.DeadlineExceeded
			if mode == "cancel" {
				want = context.Canceled
				cancel()
			}
			select {
			case got := <-connected:
				assertHandshakeRefused(t, ws, got.stop, got.err)
				if !errors.Is(got.err, want) {
					t.Error("handshake lost context cancellation identity")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Connect did not terminate after cancelled read")
			}
			awaitHandshakePeer(t, done)
		})
	}
}

func TestOKXHandshakeRedactsPeerCloseReason(t *testing.T) {
	url, done := handshakePeer(t, func(_ context.Context, conn *websocket.Conn) error {
		// coder/websocket includes this peer-supplied reason in its Read error.
		_ = conn.Close(websocket.StatusPolicyViolation, handshakeSecret)
		return nil
	})
	ws := handshakeClient(url)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stop, err := ws.Connect(ctx)
	assertHandshakeRefused(t, ws, stop, err)
	awaitHandshakePeer(t, done)
}

func TestOKXHandshakeSubscriptionWriteFailureClosesSocket(t *testing.T) {
	url, done := handshakePeer(t, func(ctx context.Context, conn *websocket.Conn) error {
		if err := conn.Write(ctx, websocket.MessageText, []byte(`{"event":"login","code":"0"}`)); err != nil {
			return errors.New("fixture could not write ACK")
		}
		return handshakePeerClosed(ctx, conn)
	})
	ws := handshakeClient(url)
	writes := 0
	ws.writeSubscribe = func(_ context.Context, typ websocket.MessageType, payload []byte) error {
		writes++
		if typ != websocket.MessageText || !containsOp(payload, "subscribe") {
			t.Error("write seam did not receive the normal subscription")
		}
		return errors.New(handshakeSecret + " fixture-api-key fixture-api-secret fixture-passphrase")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stop, err := ws.Connect(ctx)
	assertHandshakeRefused(t, ws, stop, err)
	if writes != 1 {
		t.Errorf("subscription writes = %d, want 1", writes)
	}
	awaitHandshakePeer(t, done)
}

func TestOKXHandshakeCancelledDialPreservesContext(t *testing.T) {
	ws := handshakeClient("ws://127.0.0.1:1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stop, err := ws.Connect(ctx)
	assertHandshakeRefused(t, ws, stop, err)
	if !errors.Is(err, context.Canceled) {
		t.Error("dial lost context cancellation identity")
	}
}
