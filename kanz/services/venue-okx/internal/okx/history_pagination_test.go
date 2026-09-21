package okx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/venueadapter/exchangeauth"
)

// Protocol simulation: the cursor is billId, which deliberately differs from tradeId.
func TestHistoryPaginationUsesBillIDAndRefusesPartialAnswers(t *testing.T) {
	for _, mode := range []string{"complete", "repeated", "wrong-order", "missing-cursor", "failure", "bounded"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				q := r.URL.Query()
				if r.Method != http.MethodGet || r.URL.Path != "/api/v5/trade/fills-history" || q.Get("instId") != "BTC-USDT" || q.Get("ordId") != "42" || q.Get("limit") != "100" {
					t.Error("unexpected request", r.URL)
				}
				wantCursor := ""
				if calls > 1 {
					wantCursor = strconv.Itoa(1001 - (calls-1)*100)
				}
				if q.Get("after") != wantCursor {
					t.Error("wrong cursor", q.Get("after"), wantCursor)
				}
				if calls == 2 && mode == "failure" {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
				n := 100
				if calls == 2 && mode != "bounded" {
					n = 1
				}
				rows := make([]okxFill, n)
				for i := range rows {
					rows[i] = okxFill{InstID: "BTC-USDT", OrdID: "42", TradeID: strconv.Itoa(i), BillID: strconv.Itoa(1000 - (calls-1)*100 - i)}
				}
				if calls == 2 && mode == "repeated" {
					rows[0].BillID = "901"
				}
				if mode == "wrong-order" {
					rows[0].OrdID = "43"
				}
				if mode == "missing-cursor" {
					rows[0].BillID = ""
				}
				if err := json.NewEncoder(w).Encode(struct {
					Code string    `json:"code"`
					Data []okxFill `json:"data"`
				}{"0", rows}); err != nil {
					t.Error(err)
				}
			}))
			defer srv.Close()
			client := newOKXREST(okxRestConfig{BaseURL: srv.URL, APIKey: "test", APISecret: "test", Passphrase: "test", Mode: exchangeauth.OKXDemo, Buckets: newOKXBuckets(NewWeightBucket(60, 2*time.Second, nil), nil)})
			rows, err := client.fillsHistory(context.Background(), "BTC-USDT", "42")
			if mode == "complete" {
				if err != nil || len(rows) != 101 || calls != 2 {
					t.Fatalf("rows=%d calls=%d err=%v", len(rows), calls, err)
				}
			} else if err == nil || len(rows) != 0 || calls > 8 {
				t.Fatalf("partial history escaped: rows=%d calls=%d err=%v", len(rows), calls, err)
			}
		})
	}
}
