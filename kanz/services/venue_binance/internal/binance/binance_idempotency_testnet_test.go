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
// A second typed Execute must be refused from the persisted dispatch row.
// The typed query and venue order ID must still identify the original placement.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/eighred/kanz/internal/orderid"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/internal/venueadapter/server"
	"github.com/eighred/kanz/pkg/secret"
	"github.com/eighred/kanz/test/backing"
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
		if t.posts.Add(1) != 1 {
			return nil, errors.New("testnet proof refused a second placement request")
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

func TestLostTestnetACKTransportForwardsOnePlacement(t *testing.T) {
	var forwarded atomic.Int32
	loopback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			forwarded.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer loopback.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	proof := &loseAcceptedPlacementTransport{next: loopback.Client().Transport, cancel: cancel}
	for _, requestCtx := range []context.Context{ctx, t.Context()} {
		req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, loopback.URL+"/api/v3/order", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := proof.RoundTrip(req); err == nil {
			t.Fatal("test transport returned a placement acknowledgement")
		}
	}
	if ctx.Err() == nil || forwarded.Load() != 1 || proof.posts.Load() != 2 {
		t.Fatalf("loopback POSTs=%d attempts=%d expired=%v", forwarded.Load(), proof.posts.Load(), ctx.Err())
	}
}

func TestTestnet_IdempotencyAndAmbiguousTimeoutAgainstRealVenue(t *testing.T) {
	evidence := requireFreshOrderAuthorization(t)
	key, apiSecret := os.Getenv("BINANCE_TESTNET_KEY"), os.Getenv("BINANCE_TESTNET_SECRET")
	account := strings.TrimSpace(os.Getenv("BINANCE_VENUE_ACCOUNT"))
	if account == "" {
		account = "BINANCE"
	}
	uid, err := secret.Read("BINANCE_VENUE_ACCOUNT_UID")
	if err != nil || strings.TrimSpace(uid) == "" {
		t.Fatal("testnet placement needs its bound BINANCE_VENUE_ACCOUNT_UID")
	}
	price, quantity := strings.TrimSpace(os.Getenv("BINANCE_TESTNET_PROOF_PRICE")), strings.TrimSpace(os.Getenv("BINANCE_TESTNET_PROOF_QUANTITY"))
	if key == "" || apiSecret == "" || price == "" || quantity == "" {
		t.Fatal("BINANCE_TESTNET_KEY/_SECRET and operator-approved BINANCE_TESTNET_PROOF_PRICE/_QUANTITY are required")
	}
	priceValue, priceOK := new(big.Rat).SetString(price)
	quantityValue, quantityOK := new(big.Rat).SetString(quantity)
	if !priceOK || !quantityOK || priceValue.Sign() <= 0 || quantityValue.Sign() <= 0 {
		t.Fatal("proof price and quantity must be exact positive base-10 decimals")
	}
	if os.Getenv("TEST_POSTGRES_URL") == "" {
		t.Fatal("the testnet single-placement proof requires a restricted-role TEST_POSTGRES_URL for its disposable adapter view")
	}
	open := backing.VenueStore(t, "../../migrations")
	view := open()
	orderID := orderid.Mint()
	st := &orderpb.OrderState{
		OrderId: orderID, Venue: "BINANCE", VenueAccountId: account, InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
		OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, TimeInForce: orderpb.TimeInForce_TIME_IN_FORCE_GTC,
		OrderedQuantity: bdec(quantity), LimitPrice: bdec(price),
	}
	placementCtx, expirePlacement := context.WithCancel(t.Context())
	defer expirePlacement()
	baseClient := newExchangeHTTPClient(0)
	next := baseClient.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	loss := &loseAcceptedPlacementTransport{next: next, cancel: expirePlacement}
	client := &http.Client{Transport: loss, Timeout: 10 * time.Second}
	rest := newBinanceREST(restConfig{
		BaseURL: "https://testnet.binance.vision", APIKey: key, APISecret: apiSecret,
		Bucket: newWeightBucket(1200, time.Minute, nil), HTTPClient: client,
	})
	venue := NewBinanceVenue(BinanceConfig{
		MIC: "BINANCE", Account: account, Symbols: StaticSymbolMap{"BTC-USD": "BTCUSDT"}, REST: rest,
	})

	proof, err := venue.verifyAccount(placementCtx, strings.TrimSpace(uid), slog.Default())
	if err != nil || !proof.Verified {
		t.Fatal("testnet account verification failed")
	}

	venue.durableOrders = view
	adapter := server.NewWithDispatchEvidence(venue, view, view, proof, halt.OpenGate(nil), slog.Default())
	defer func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if _, err := adapter.CancelOrder(cleanupCtx, &venuepb.CancelOrderRequest{State: st}); err != nil {
			t.Errorf("CRITICAL: cleanup could not cancel testnet order %s: %v", orderID, err)
		}
	}()

	request := &venuepb.ExecuteRequest{State: st}
	if ack, err := adapter.Execute(placementCtx, request); err != nil {
		t.Fatalf("ambiguous placement was not recovered by client-order-id query: %v", err)
	} else if len(ack.GetFills()) != 0 {
		t.Fatalf("proof order unexpectedly traded during ambiguous recovery: fills=%d", len(ack.GetFills()))
	}
	observeCtx, stopObserve := context.WithTimeout(t.Context(), 30*time.Second)
	defer stopObserve()
	first, err := rest.queryOrder(observeCtx, "BTCUSDT", orderID)
	if err != nil {
		t.Fatalf("query accepted proof order: %v", err)
	}
	if first.ClientOrderID != orderID || first.OrderID == 0 || first.Status != "NEW" {
		t.Fatalf("accepted order is not the intended resting order: client_match=%t venue_id_present=%t status=%q",
			first.ClientOrderID == orderID, first.OrderID != 0, first.Status)
	}

	reopened := open()
	replay := server.NewWithDispatchEvidence(venue, reopened, reopened, proof, halt.OpenGate(nil), slog.Default())
	if _, err := replay.Execute(observeCtx, request); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("recorded placement must refuse a duplicate Execute: %v", err)
	}
	recovered, err := replay.QueryOrder(observeCtx, &venuepb.QueryOrderRequest{State: st})
	if err != nil || recovered.GetState() != venuepb.OrderViewState_ORDER_VIEW_STATE_WORKING ||
		recovered.GetExecutedQuantity() == nil || recovered.GetExecutedQuantity().GetCoefficient() != 0 || len(recovered.GetFills()) != 0 {
		t.Fatalf("recorded placement did not reconcile to a resting order: %v %v", recovered.GetState(), err)
	}
	second, err := rest.queryOrder(observeCtx, "BTCUSDT", orderID)
	if err != nil {
		t.Fatalf("query after duplicate Execute: %v", err)
	}
	if second.OrderID != first.OrderID || second.ClientOrderID != orderID {
		t.Fatalf("duplicate produced a different exchange order: same_venue_id=%t client_match=%t",
			second.OrderID == first.OrderID, second.ClientOrderID == orderID)
	}
	if got := loss.posts.Load(); got != 1 {
		t.Fatalf("placement requests=%d, want exactly one", got)
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
