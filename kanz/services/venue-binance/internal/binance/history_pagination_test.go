package binance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// These are exchange-protocol simulations over HTTP, not live venue proofs.
func TestHistoryPaginationStartsAtOldestAndRefusesPartialAnswers(t *testing.T) {
	for _, mode := range []string{"complete", "repeated", "wrong-order", "failure", "bounded"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				q := r.URL.Query()
				if r.Method != http.MethodGet || r.URL.Path != "/api/v3/myTrades" || q.Get("symbol") != "BTCUSDT" || q.Get("orderId") != "42" || q.Get("limit") != "1000" {
					t.Error("unexpected request", r.URL)
				}
				from, err := strconv.ParseInt(q.Get("fromId"), 10, 64)
				if err != nil || from != int64((calls-1)*1000) {
					t.Error("wrong cursor", q.Get("fromId"))
				}
				if calls == 2 && mode == "failure" {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
				n := 1000
				if calls == 2 && mode != "bounded" {
					n = 1
				}
				rows := make([]tradeEntry, n)
				for i := range rows {
					rows[i] = tradeEntry{Symbol: "BTCUSDT", OrderID: 42, ID: from + int64(i)}
				}
				if calls == 2 && mode == "repeated" {
					rows[0].ID = 999
				}
				if mode == "wrong-order" {
					rows[0].OrderID = 43
				}
				if err := json.NewEncoder(w).Encode(rows); err != nil {
					t.Error(err)
				}
			}))
			defer srv.Close()
			client := newBinanceREST(restConfig{BaseURL: srv.URL, APIKey: "test", APISecret: "test", Bucket: newWeightBucket(1200, time.Minute, nil)})
			rows, err := client.myTrades(context.Background(), "BTCUSDT", 42)
			if mode == "complete" {
				if err != nil || len(rows) != 1001 || calls != 2 {
					t.Fatalf("rows=%d calls=%d err=%v", len(rows), calls, err)
				}
			} else if err == nil || len(rows) != 0 || calls > 8 {
				t.Fatalf("partial history escaped: rows=%d calls=%d err=%v", len(rows), calls, err)
			}
		})
	}
}
