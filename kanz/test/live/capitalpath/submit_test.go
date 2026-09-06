package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/eighred/kanz/internal/gatewaysig"
)

func TestGatewaySubmitterUsesOnlyTheAuthenticatedFrontDoor(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/orders" {
			t.Errorf("request=%s %s want POST /v1/orders", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer opaque-bearer" {
			t.Errorf("Authorization=%q", got)
		}
		id := r.Header.Get("Idempotency-Key")
		if len(id) != 32 {
			t.Errorf("Idempotency-Key=%q want 32-character canonical order id", id)
		}
		raw, _ := io.ReadAll(r.Body)
		if got, want := r.Header.Get(gatewaysig.Header), gatewaysig.Sign([]byte("gateway-signing-key"), r.Method, r.URL.Path, raw); got != want {
			t.Errorf("gateway signature=%q want %q", got, want)
		}
		var order orderpb.SubmitOrder
		if err := protojson.Unmarshal(raw, &order); err != nil {
			t.Fatalf("decode order: %v", err)
		}
		if order.GetOrderId() != id || order.GetPortfolioId() != "PF-CERT" ||
			order.GetInstrumentId() != "BTC-USDT" || order.GetVenue() != "XOKX" {
			t.Errorf("order=%+v idempotency=%q", &order, id)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	cfg := validConfig(testNow())
	cfg.gatewayURL = server.URL
	client := newGatewayClient(server.Client())
	orderID, err := client.submit(context.Background(), cfg)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if len(orderID) != 32 || requests.Load() != 1 {
		t.Fatalf("orderID=%q requests=%d", orderID, requests.Load())
	}
}

func TestGatewaySubmitterNeverRetriesAnAmbiguousResponse(t *testing.T) {
	var requests atomic.Int32
	client := newGatewayClient(&http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, context.DeadlineExceeded
	})})
	cfg := validConfig(testNow())
	_, err := client.submit(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("err=%v want explicit ambiguous outcome", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests=%d want exactly one; placement retries belong to OMS reconciliation", requests.Load())
	}
}

func TestGatewayCancellationUsesTheSameAuthenticatedFrontDoor(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/orders/order-1/cancel" {
			t.Errorf("request=%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer opaque-bearer" {
			t.Errorf("missing authenticated front door")
		}
		if got, want := r.Header.Get(gatewaysig.Header), gatewaysig.Sign([]byte("gateway-signing-key"), r.Method, r.URL.Path, nil); got != want {
			t.Errorf("cancel gateway signature=%q want %q", got, want)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	cfg := validConfig(testNow())
	cfg.gatewayURL = server.URL
	if err := newGatewayClient(server.Client()).cancel(context.Background(), cfg, "order-1"); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests=%d want 1", requests.Load())
	}
}

func TestGatewaySubmitterRefusesAnythingBut202(t *testing.T) {
	client := newGatewayClient(&http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Body:       io.NopCloser(strings.NewReader("denied by mandate")),
			Header:     make(http.Header),
		}, nil
	})})
	_, err := client.submit(context.Background(), validConfig(testNow()))
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "denied by mandate") {
		t.Fatalf("err=%v want bounded refusal body", err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testNow() time.Time { return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC) }
