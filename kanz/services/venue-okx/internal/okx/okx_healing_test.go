package okx

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"

	"github.com/kanz-eng/kanz/internal/dec"
)

// A 401/403 at the venue auth boundary — the signature of a production key run
// from an egress IP outside the exchange allowlist — surfaces as the typed,
// non-retryable ErrEgressDenied so the connector alerts and stops.
func TestTransport_EgressDeniedClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	bucket := NewWeightBucket(60, time.Minute, nil)
	rest := newOKXREST(okxRestConfig{BaseURL: srv.URL, APIKey: "k", APISecret: "s", Passphrase: "p", Bucket: bucket})

	_, err := rest.queryOrder(context.Background(), "BTC-USDT", "o1")
	if !errors.Is(err, ErrEgressDenied) {
		t.Fatalf("err = %v, want ErrEgressDenied (handshake denial must be typed)", err)
	}
}

func healReconOver(f *fakeOKX, cap *okxCapture, reg PendingCloses) *OKXReconciler {
	bucket := NewWeightBucket(60, time.Minute, nil)
	rest := newOKXREST(okxRestConfig{BaseURL: f.srv.URL, APIKey: "k", APISecret: "s", Passphrase: "p", Bucket: bucket})
	return newOKXReconciler(OKXReconcilerConfig{
		REST: rest, Symbols: StaticSymbolMap{"BTC-USD": "BTC-USDT"},
		Closes: reg, CloseTimeout: 1500 * time.Millisecond,
		Pub: cap, Venue: "OKX", Tenant: "fund-alpha",
	})
}

func trackedClose(reg *CloseRegistry, id string, age time.Duration) {
	reg.Track(CloseIntent{
		OrderID: id, InstrumentID: "BTC-USD",
		SweepSide: orderpb.Side_SIDE_SELL, Leaves: big.NewRat(1, 1),
		RequestedAt: time.Now().Add(-age).UTC(),
	})
}

// A close the venue confirms terminal on query heals to the venue truth — no
// sweep, registry drained.
func TestHeal_ConfirmedTerminalNoSweep(t *testing.T) {
	f := newFakeOKX(t)
	f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"9","clOrdId":"o1","state":"canceled","accFillSz":"0"}]}`
	cap := &okxCapture{}
	reg := NewCloseRegistry()
	trackedClose(reg, "o1", 2*time.Second) // past the 1500ms timeout

	if err := healReconOver(f, cap, reg).HealClosures(context.Background()); err != nil {
		t.Fatalf("HealClosures: %v", err)
	}
	if f.posts != 0 {
		t.Fatalf("posts = %d, want 0 (confirmed terminal must not sweep)", f.posts)
	}
	if reg.Len() != 0 {
		t.Fatal("close not resolved after healing")
	}
	var healed *orderpb.StateHealed
	for _, e := range cap.events {
		if h, ok := e.Payload.(*orderpb.StateHealed); ok {
			healed = h
		}
	}
	if healed == nil || healed.GetState().GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatalf("want StateHealed CANCELLED (venue truth), got %+v", healed)
	}
}

// A close the venue still reports working (stuck) is force-cleared and the
// residual swept via an aggressive market order; StateHealed → CANCELLED.
func TestHeal_StuckForceClearsAndSweeps(t *testing.T) {
	f := newFakeOKX(t)
	f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"9","clOrdId":"o1","state":"live","accFillSz":"0"}]}`
	f.placeBody = `{"code":"0","msg":"","data":[{"ordId":"sweep1","clOrdId":"heal-o1","sCode":"0","sMsg":""}]}`
	f.balanceBody = `{"code":"0","msg":"","data":[{"details":[{"ccy":"BTC","cashBal":"0"}]}]}`
	cap := &okxCapture{}
	reg := NewCloseRegistry()
	trackedClose(reg, "o1", 2*time.Second)

	if err := healReconOver(f, cap, reg).HealClosures(context.Background()); err != nil {
		t.Fatalf("HealClosures: %v", err)
	}
	if f.posts != 1 {
		t.Fatalf("posts = %d, want 1 (a sweep market order)", f.posts)
	}
	if f.sawPostCl != "heal-o1" {
		t.Fatalf("sweep clOrdId = %q, want heal-o1 (idempotent)", f.sawPostCl)
	}
	if reg.Len() != 0 {
		t.Fatal("stuck close not resolved")
	}
	var healed *orderpb.StateHealed
	for _, e := range cap.events {
		if h, ok := e.Payload.(*orderpb.StateHealed); ok {
			healed = h
		}
	}
	if healed == nil || healed.GetState().GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatal("stuck close must force-clear to CANCELLED")
	}
}

// An unresponsive venue (query returns an error code) still force-clears + sweeps
// — the ledger must never freeze.
func TestHeal_UnresponsiveVenueStillClears(t *testing.T) {
	f := newFakeOKX(t)
	f.queryBody = `{"code":"51000","msg":"unavailable","data":[]}` // queryOrder → error
	f.placeBody = `{"code":"0","msg":"","data":[{"ordId":"sweep1","clOrdId":"heal-o1","sCode":"0","sMsg":""}]}`
	f.balanceBody = `{"code":"0","msg":"","data":[{"details":[]}]}`
	cap := &okxCapture{}
	reg := NewCloseRegistry()
	trackedClose(reg, "o1", 2*time.Second)

	if err := healReconOver(f, cap, reg).HealClosures(context.Background()); err != nil {
		t.Fatalf("HealClosures: %v", err)
	}
	if f.posts != 1 {
		t.Fatalf("posts = %d, want 1 (sweep despite unresponsive query)", f.posts)
	}
	if reg.Len() != 0 {
		t.Fatal("close must resolve even when the venue is unresponsive")
	}
}

// A close still within the timeout window is left alone.
func TestHeal_NotYetDueLeftAlone(t *testing.T) {
	f := newFakeOKX(t)
	cap := &okxCapture{}
	reg := NewCloseRegistry()
	trackedClose(reg, "o1", 500*time.Millisecond) // < 1500ms

	if err := healReconOver(f, cap, reg).HealClosures(context.Background()); err != nil {
		t.Fatalf("HealClosures: %v", err)
	}
	if f.posts != 0 || len(cap.events) != 0 {
		t.Fatalf("a not-yet-due close was acted on: posts=%d events=%d", f.posts, len(cap.events))
	}
	if reg.Len() != 1 {
		t.Fatal("a not-yet-due close must remain tracked")
	}
}

func TestCloseRegistry_TrackPreservesRequestedAt(t *testing.T) {
	reg := NewCloseRegistry()
	t0 := time.Now().Add(-2 * time.Second).UTC()
	reg.Track(CloseIntent{OrderID: "o1", InstrumentID: "BTC-USD", RequestedAt: t0})
	// A re-track (retry) must not reset the timeout clock.
	reg.Track(CloseIntent{OrderID: "o1", InstrumentID: "BTC-USD", Leaves: big.NewRat(2, 1)})
	due := reg.DueCloses(time.Now(), 1500*time.Millisecond)
	if len(due) != 1 {
		t.Fatalf("due = %d, want 1 (original RequestedAt preserved)", len(due))
	}
	if got := dec.FromProto(dec.ToProto(due[0].Leaves)); got.Cmp(big.NewRat(2, 1)) != 0 {
		t.Fatalf("re-track should update Leaves, got %s", got.RatString())
	}
}
