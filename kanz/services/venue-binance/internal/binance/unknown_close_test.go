package binance

import (
	"context"
	"errors"
	"fmt"
	"github.com/eighred/kanz/internal/execution"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCloseQueryFailuresRetainOwnershipAndRecover(t *testing.T) {
	for _, fault := range []string{"401", "403", "429", "500", "503", "timeout", "malformed", "empty", "missing", "working", "wrong_order", "invalid_quantity", "unknown_status"} {
		t.Run(fault, func(t *testing.T) {
			f := newFakeBinance(t)
			capture := &reconCapture{}
			reg := NewCloseRegistry()
			trackedFlatten(reg, "o1", time.Hour)
			r := healReconOverBinance(f, capture, reg)
			now := time.Now()
			r.now = func() time.Time { return now }
			var recovered atomic.Bool
			var gets, posts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodGet {
					posts.Add(1)
					w.WriteHeader(500)
					return
				}
				gets.Add(1)
				body := `{"clientOrderId":"o1","status":"CANCELED","executedQty":"0"}`
				if !recovered.Load() {
					switch fault {
					case "401":
						w.WriteHeader(401)
					case "403":
						w.WriteHeader(403)
					case "429":
						w.WriteHeader(429)
					case "500":
						w.WriteHeader(500)
					case "503":
						w.WriteHeader(503)
					case "timeout":
						<-req.Context().Done()
						return
					case "malformed":
						body = "{"
					case "empty":
						body = "{}"
					case "missing":
						body = `{"code":-2013,"msg":"missing"}`
					case "working":
						body = `{"clientOrderId":"o1","status":"NEW","executedQty":"0"}`
					case "wrong_order":
						body = strings.ReplaceAll(body, "o1", "other")
					case "invalid_quantity":
						body = strings.ReplaceAll(body, `executedQty":"0"`, `executedQty":"bad"`)
					case "unknown_status":
						body = strings.ReplaceAll(body, `CANCELED`, `future_status`)
					}
				}
				_, _ = fmt.Fprint(w, body)
			}))
			defer srv.Close()
			r.rest.baseURL = srv.URL
			r.rest.httpc = &http.Client{Timeout: 100 * time.Millisecond}
			err := r.HealClosures(context.Background())
			if err == nil || reg.Len() != 1 || posts.Load() != 0 || len(capture.events) != 0 {
				t.Fatalf("unknown became terminal: err=%v pending=%d posts=%d events=%d", err, reg.Len(), posts.Load(), len(capture.events))
			}
			if err := r.HealClosures(context.Background()); err != nil {
				t.Fatal(err)
			}
			if gets.Load() != 1 {
				t.Fatal("retry ignored backoff")
			}
			recovered.Store(true)
			now = now.Add(time.Minute)
			if err := r.HealClosures(context.Background()); err != nil {
				t.Fatal(err)
			}
			if reg.Len() != 0 || posts.Load() != 0 || len(capture.events) != 1 {
				t.Fatalf("recovery lost evidence: pending=%d posts=%d events=%d", reg.Len(), posts.Load(), len(capture.events))
			}
		})
	}
}

func TestReconciliationReportsMixedCoverage(t *testing.T) {
	f := newFakeBinance(t)
	capture := &reconCapture{}
	r := reconOver(f, capture, staticOrders{
		&orderpb.OrderState{OrderId: "bad", InstrumentId: "BTC-USD", OrderedQuantity: bdec("1")},
		&orderpb.OrderState{OrderId: "o1", InstrumentId: "BTC-USD", OrderedQuantity: bdec("1")},
	}, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.RawQuery, "bad") {
			w.WriteHeader(503)
			return
		}
		_, _ = fmt.Fprint(w, `{"clientOrderId":"o1","status":"CANCELED","executedQty":"0"}`)
	}))
	defer srv.Close()
	r.rest.baseURL = srv.URL
	err := r.Reconcile(context.Background())
	var pass *execution.ReconcilePassError
	var failedOrder *execution.OrderObservationError
	if !errors.As(err, &failedOrder) || failedOrder.OrderID != "bad" {
		t.Fatalf("failed order identity missing: %v", err)
	}
	if !errors.As(err, &pass) || pass.Total != 2 || pass.Checked != 2 || pass.Failed != 1 || len(capture.events) != 1 {
		t.Fatalf("partial coverage erased: err=%v events=%d", err, len(capture.events))
	}
	capture.events = nil
	reg := NewCloseRegistry()
	for _, id := range []string{"bad", "o1"} {
		if err := reg.Track(context.Background(), CloseIntent{OrderID: id, InstrumentID: "BTC-USD", RequestedAt: time.Now().Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	r.closes = reg
	err = r.HealClosures(context.Background())
	if !errors.As(err, &pass) || pass.Total != 2 || pass.Checked != 2 || pass.Failed != 1 || len(capture.events) != 1 || reg.Len() != 1 {
		t.Fatalf("mixed close coverage lost: err=%v events=%d pending=%d", err, len(capture.events), reg.Len())
	}

}
