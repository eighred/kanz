package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/tv-sync/internal/brokerapi"
	"github.com/eighred/kanz/services/tv-sync/internal/projection"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestServingGenerationRejectsOldLeasesAfterRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := NewReadiness(ctx)
	if _, ok := r.AcquireStream(); ok {
		t.Fatal("unready lease")
	}
	r.Set(true)
	lease, ok := r.AcquireStream()
	if !ok {
		t.Fatal("ready lease refused")
	}
	r.FoldResult("a", "one", true)
	select {
	case <-lease.Done():
	default:
		t.Fatal("failure did not revoke established generation")
	}
	r.FoldResult("a", "one", false)
	r.Set(true)
	called := false
	if err := lease.Write(func() error { called = true; return nil }); err == nil || called {
		t.Fatal("old stream revived after recovery")
	}
	next, ok := r.AcquireStream()
	if !ok {
		t.Fatal("new generation refused")
	}
	if err := next.Write(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-next.Done():
	case <-time.After(time.Second):
		t.Fatal("process cancellation did not revoke stream")
	}
}
func TestServingRevocationOrdersBoundedInflightWrite(t *testing.T) {
	r := NewReadiness(context.Background())
	r.Set(true)
	lease, _ := r.AcquireStream()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	first, second := make(chan error, 1), make(chan error, 1)
	var late atomic.Bool
	go func() { first <- lease.Write(func() error { close(entered); <-release; return nil }) }()
	<-entered
	revoked := make(chan struct{})
	go func() { r.Set(false); close(revoked) }()
	deadline := time.After(time.Second)
	for r.Ready() {
		select {
		case <-deadline:
			t.Fatal("readiness not revoked before waiting for write")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	go func() { second <- lease.Write(func() error { late.Store(true); return nil }) }()
	unblock()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	select {
	case <-revoked:
	case <-time.After(time.Second):
		t.Fatal("revocation stuck")
	}
	if err := <-second; err == nil || late.Load() {
		t.Fatal("new write crossed revocation")
	}
}
func streamProjection(t *testing.T) *projection.Projection {
	t.Helper()
	p := projection.New(time.Now, nil)
	seedStream(t, p, "seed")
	return p
}
func seedStream(t *testing.T, p *projection.Projection, id string) {
	t.Helper()
	b, err := proto.Marshal(&orderpb.OrderAccepted{State: &orderpb.OrderState{OrderId: id, PortfolioId: "fund", InstrumentId: "BTC", Side: orderpb.Side_SIDE_BUY, OrderedQuantity: &commonpb.Decimal{Coefficient: 1}, Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW, AsOf: timestamppb.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Handle(context.Background(), &envelopepb.Envelope{TenantId: "tenant", EventType: "order.order.accepted", EventId: id}, b); err != nil {
		t.Fatal(err)
	}
}
func openStream(t *testing.T, ctx context.Context, client *http.Client, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, "GET", url+"/broker/accounts/fund/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	auth.SetPrincipalHeaders(req.Header, "fixture", "tenant", nil)
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
func TestEstablishedStreamClosesOnFailureAndRequiresNewGeneration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p := streamProjection(t)
	r := NewReadiness(ctx)
	r.Set(true)
	srv := httptest.NewServer(New(r, brokerapi.New(p)))
	defer srv.Close()
	response := openStream(t, ctx, srv.Client(), srv.URL)
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("ready stream refused", response.StatusCode)
	}
	closed := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(response.Body); closed <- b }()
	r.FoldResult("tenant", "failed", true)
	select {
	case b := <-closed:
		if len(b) != 0 {
			t.Fatalf("revoked stream emitted %d bytes", len(b))
		}
	case <-time.After(time.Second):
		t.Fatal("established stream did not close without another delivery")
	}
	seedStream(t, p, "after-failure")
	denied := openStream(t, ctx, srv.Client(), srv.URL)
	_ = denied.Body.Close()
	if denied.StatusCode != 503 {
		t.Fatal("failed generation admitted a new stream")
	}
	r.FoldResult("tenant", "failed", false)
	next := openStream(t, ctx, srv.Client(), srv.URL)
	defer next.Body.Close()
	if next.StatusCode != 200 {
		t.Fatal("recovered generation refused new stream")
	}
	cancel()
}

type heldAcquire struct {
	r                *Readiness
	entered, release chan struct{}
}

func (g heldAcquire) AcquireStream() (brokerapi.StreamLease, bool) {
	lease, ok := g.r.AcquireStream()
	close(g.entered)
	<-g.release
	return lease, ok
}
func TestStreamHeaderAdmissionCannotRaceRevocation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p := streamProjection(t)
	r := NewReadiness(ctx)
	r.Set(true)
	broker := brokerapi.New(p)
	s := New(r, broker)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	broker.SetStreamGate(heldAcquire{r, entered, release})
	srv := httptest.NewServer(s)
	defer srv.Close()
	got := make(chan int, 1)
	go func() {
		request, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/broker/accounts/fund/stream", nil)
		auth.SetPrincipalHeaders(request.Header, "fixture", "tenant", nil)
		resp, err := srv.Client().Do(request)
		if err != nil {
			got <- 0
			return
		}
		defer resp.Body.Close()
		got <- resp.StatusCode
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("lease not acquired")
	}
	r.Set(false)
	r.Set(true)
	unblock()
	select {
	case status := <-got:
		if status != 503 {
			t.Fatalf("stale header lease status%d", status)
		}
	case <-ctx.Done():
		t.Fatal("header race did not finish")
	}
}
