package binance

// This is the destructive half of the Binance testnet certification for #72.
// It is deliberately separate from the ordinary signed read in
// binance_testnet_test.go: enabling that read must never acquire the side effect
// of placing an order.
//
// The test requires a fresh PASS artifact from Test-TestnetOmsGoLive.ps1, the
// exact deployed/verifier release commit, a separately recorded approval id,
// and operator-selected price/quantity inside the approved mandate. It places a
// resting testnet LIMIT with one deterministic newClientOrderId, deliberately
// loses the accepted POST response, and proves the connector recovers with GET.
// It then deliberately repeats the POST and proves Binance still identifies one
// exchange order. Cleanup cancels that exact client id under a detached bound.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/orderid"
	"github.com/eighred/kanz/pkg/secret"
)

const liveOrderProofGate = "I_ACCEPT_ONE_CONTROLLED_TESTNET_ORDER"

type preflightEvidence struct {
	Format         string    `json:"format"`
	ObservedAt     time.Time `json:"observed_at"`
	VerifierCommit string    `json:"verifier_commit"`
	DeployedCommit string    `json:"deployed_commit"`
	CommandID      string    `json:"command_id"`
	Verdict        string    `json:"verdict"`
}

func requireFreshOrderAuthorization(t *testing.T) preflightEvidence {
	t.Helper()
	if os.Getenv("TEST_BINANCE_TESTNET_ORDER_PROOF") == "" {
		t.Skip("set TEST_BINANCE_TESTNET_ORDER_PROOF=1 only after the capital-admission preflight passes")
	}
	if os.Getenv("KANZ_TESTNET_ORDER_AUTHORIZATION") != liveOrderProofGate {
		t.Fatalf("KANZ_TESTNET_ORDER_AUTHORIZATION must equal %q", liveOrderProofGate)
	}
	raw, err := secret.Read("KANZ_TESTNET_PREFLIGHT_EVIDENCE")
	if err != nil {
		t.Fatalf("resolve preflight evidence: %v", err)
	}
	if strings.TrimSpace(raw) == "" {
		t.Fatal("KANZ_TESTNET_PREFLIGHT_EVIDENCE_FILE must name the fresh non-secret preflight artifact")
	}
	var evidence preflightEvidence
	if err := json.Unmarshal([]byte(raw), &evidence); err != nil {
		t.Fatalf("decode preflight evidence: %v", err)
	}
	wantCommit := strings.TrimSpace(os.Getenv("KANZ_TESTNET_RELEASE_COMMIT"))
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(wantCommit) {
		t.Fatal("KANZ_TESTNET_RELEASE_COMMIT must be the exact 40-character merged release commit")
	}
	if evidence.Format != "kanz-oms-preflight-v1" || evidence.Verdict != "PASS" ||
		evidence.CommandID == "" || evidence.VerifierCommit != wantCommit || evidence.DeployedCommit != wantCommit {
		t.Fatalf("preflight does not authorize release %s: format=%q verdict=%q verifier=%q deployed=%q command_present=%t",
			wantCommit, evidence.Format, evidence.Verdict, evidence.VerifierCommit,
			evidence.DeployedCommit, evidence.CommandID != "")
	}
	age := time.Since(evidence.ObservedAt)
	if age < 0 || age > 15*time.Minute {
		t.Fatalf("preflight evidence is not fresh: age=%s", age)
	}
	if strings.TrimSpace(os.Getenv("KANZ_TESTNET_ORDER_APPROVAL_ID")) == "" {
		t.Fatal("KANZ_TESTNET_ORDER_APPROVAL_ID must identify the separately retained dual-control approval")
	}
	return evidence
}

// loseAcceptedPlacementTransport passes the first order POST to Binance, drains
// its response, then returns a deadline error and cancels the placement context.
// GET and cleanup requests pass through unchanged. This puts the uncertainty at
// the real network boundary: Binance has answered, while the connector has not.
type loseAcceptedPlacementTransport struct {
	next   http.RoundTripper
	lose   sync.Once
	cancel context.CancelFunc
	posts  atomic.Int32
	gets   atomic.Int32
}

func (t *loseAcceptedPlacementTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path != "/api/v3/order" {
		return t.next.RoundTrip(req)
	}
	switch req.Method {
	case http.MethodPost:
		t.posts.Add(1)
		lost := false
		t.lose.Do(func() { lost = true })
		if !lost {
			return t.next.RoundTrip(req)
		}
		resp, err := t.next.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		_, copyErr := io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		closeErr := resp.Body.Close()
		if copyErr != nil || closeErr != nil || resp.StatusCode >= http.StatusBadRequest {
			return nil, fmt.Errorf("test transport could not establish an accepted placement response: status=%d copy=%v close=%v",
				resp.StatusCode, copyErr, closeErr)
		}
		t.cancel()
		return nil, context.DeadlineExceeded
	case http.MethodGet:
		t.gets.Add(1)
	}
	return t.next.RoundTrip(req)
}

func TestTestnet_IdempotencyAndAmbiguousTimeoutAgainstRealVenue(t *testing.T) {
	evidence := requireFreshOrderAuthorization(t)
	key, secret := os.Getenv("BINANCE_TESTNET_KEY"), os.Getenv("BINANCE_TESTNET_SECRET")
	price, quantity := strings.TrimSpace(os.Getenv("BINANCE_TESTNET_PROOF_PRICE")), strings.TrimSpace(os.Getenv("BINANCE_TESTNET_PROOF_QUANTITY"))
	if key == "" || secret == "" || price == "" || quantity == "" {
		t.Fatal("BINANCE_TESTNET_KEY/_SECRET and operator-approved BINANCE_TESTNET_PROOF_PRICE/_QUANTITY are required")
	}
	priceValue, priceOK := new(big.Rat).SetString(price)
	quantityValue, quantityOK := new(big.Rat).SetString(quantity)
	if !priceOK || !quantityOK || priceValue.Sign() <= 0 || quantityValue.Sign() <= 0 {
		t.Fatal("proof price and quantity must be exact positive base-10 decimals")
	}
	orderID := orderid.Mint()
	st := &orderpb.OrderState{
		OrderId: orderID, InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
		OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, TimeInForce: orderpb.TimeInForce_TIME_IN_FORCE_GTC,
		OrderedQuantity: bdec(quantity), LimitPrice: bdec(price),
	}
	placementCtx, expirePlacement := context.WithCancel(context.Background())
	baseClient := newExchangeHTTPClient(0)
	next := baseClient.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	loss := &loseAcceptedPlacementTransport{next: next, cancel: expirePlacement}
	client := &http.Client{Transport: loss, Timeout: 10 * time.Second}
	rest := newBinanceREST(restConfig{
		BaseURL: "https://testnet.binance.vision", APIKey: key, APISecret: secret,
		Bucket: newWeightBucket(1200, time.Minute, nil), HTTPClient: client,
	})
	venue := NewBinanceVenue(BinanceConfig{
		MIC: "BINANCE", Symbols: StaticSymbolMap{"BTC-USD": "BTCUSDT"}, REST: rest,
	})

	cleanupCtx, stopCleanup := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCleanup()
	defer func() {
		if err := venue.CancelOrder(cleanupCtx, st); err != nil {
			t.Errorf("CRITICAL: cleanup could not cancel testnet order %s: %v", orderID, err)
		}
	}()

	if fills, err := venue.Execute(placementCtx, st); err != nil {
		t.Fatalf("ambiguous placement was not recovered by client-order-id query: %v", err)
	} else if len(fills) != 0 {
		t.Fatalf("proof order unexpectedly traded during ambiguous recovery: fills=%d", len(fills))
	}
	first, err := rest.queryOrder(cleanupCtx, "BTCUSDT", orderID)
	if err != nil {
		t.Fatalf("query accepted proof order: %v", err)
	}
	if first.ClientOrderID != orderID || first.OrderID == 0 || first.Status != "NEW" {
		t.Fatalf("accepted order is not the intended resting order: client_match=%t venue_id_present=%t status=%q",
			first.ClientOrderID == orderID, first.OrderID != 0, first.Status)
	}

	if fills, err := venue.Execute(context.Background(), st); err != nil {
		t.Fatalf("deliberate duplicate was not recovered by client-order-id query: %v", err)
	} else if len(fills) != 0 {
		t.Fatalf("duplicate proof unexpectedly returned fills: %d", len(fills))
	}
	second, err := rest.queryOrder(cleanupCtx, "BTCUSDT", orderID)
	if err != nil {
		t.Fatalf("query after deliberate duplicate: %v", err)
	}
	if second.OrderID != first.OrderID || second.ClientOrderID != orderID {
		t.Fatalf("duplicate produced a different exchange order: same_venue_id=%t client_match=%t",
			second.OrderID == first.OrderID, second.ClientOrderID == orderID)
	}
	if got := loss.posts.Load(); got != 2 {
		t.Fatalf("placement requests=%d, want initial plus one deliberate duplicate", got)
	}
	if got := loss.gets.Load(); got < 3 {
		t.Fatalf("client-order-id queries=%d, want recovery plus before/after identity reads", got)
	}
	if !errors.Is(placementCtx.Err(), context.Canceled) {
		t.Fatalf("fault injection did not expire placement context: %v", placementCtx.Err())
	}

	t.Logf("real-venue proof passed: release=%s preflight_command=%s client_order_id=%s one_exchange_order=true",
		evidence.DeployedCommit, evidence.CommandID, orderID)
}
