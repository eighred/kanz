package binance

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCommissionRateRejectsUnboundedExchangeNumber(t *testing.T) {
	for _, raw := range []string{"1e2000000000", `"1/2"`, `"-0.1"`, `null`, `"` + strings.Repeat("9", 401) + `"`} {
		var rate commissionRate
		if err := json.Unmarshal([]byte(raw), &rate); err == nil {
			t.Fatalf("accepted unbounded commission rate %q", raw[:min(len(raw), 32)])
		}
	}
}

func TestBinanceCapitalEvidenceReadsSignedCurrentCommissionAndExchangeSettlement(t *testing.T) {
	quote := "USDT"
	commission := `{"symbol":"BTCUSDT","standardCommission":{"maker":"0.001","taker":"0.002","buyer":"0","seller":"0"},"specialCommission":{"maker":"0","taker":"0","buyer":"0","seller":"0"},"taxCommission":{"maker":0.00000112,"taker":0.00000114,"buyer":0,"seller":0},"discount":{"discountAsset":"BNB"}}`
	responseStatus := http.StatusOK
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/exchangeInfo", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("symbol") != "BTCUSDT" {
			t.Error("catalogue queried the wrong symbol")
		}
		mu.Lock()
		body := `{"symbols":[{"symbol":"BTCUSDT","baseAsset":"BTC","quoteAsset":"` + quote + `","status":"TRADING"}]}`
		mu.Unlock()
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/api/v3/account/commission", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		sig := q.Get("signature")
		q.Del("signature")
		mac := hmac.New(sha256.New, []byte("secret"))
		fmt.Fprint(mac, q.Encode())
		if r.Header.Get("X-MBX-APIKEY") != "key" || sig != hex.EncodeToString(mac.Sum(nil)) || q.Get("symbol") != "BTCUSDT" {
			t.Error("current commission request was not signed and symbol scoped")
		}
		mu.Lock()
		body, status := commission, responseStatus
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	rest := newBinanceREST(restConfig{BaseURL: srv.URL, APIKey: "key", APISecret: "secret", Bucket: newWeightBucket(1200, time.Minute, nil)})
	v := NewBinanceVenue(BinanceConfig{Account: "account", Symbols: StaticSymbolMap{"BTC-USDT": "BTCUSDT"}, REST: rest})
	evidence, err := v.ReadCapitalEvidence(context.Background(), "BTC-USDT", "account")
	if err != nil {
		t.Fatal(err)
	}
	if evidence.GetQuoteAsset() != "USDT" || evidence.GetSettlementSourceVersion().GetContentSha256() == "" || evidence.GetCurrentFee().GetSourceVersion().GetContentSha256() == "" {
		t.Fatalf("incomplete exchange evidence: %+v", evidence)
	}
	catalogue := `{"symbols":[{"symbol":"BTCUSDT","baseAsset":"BTC","quoteAsset":"USDT","status":"TRADING"}]}`
	if got := evidence.GetSettlementSourceVersion(); got.GetSource() != "binance:/api/v3/exchangeInfo" || got.GetContentSha256() != fmt.Sprintf("%x", sha256.Sum256([]byte(catalogue))) || got.GetObservedAtUnixMillis() <= 0 {
		t.Fatal("catalogue provenance mismatch", got)
	}
	if got := evidence.GetCurrentFee().GetSourceVersion(); got.GetSource() != "binance:/api/v3/account/commission" || got.GetContentSha256() != fmt.Sprintf("%x", sha256.Sum256([]byte(commission))) || got.GetObservedAtUnixMillis() <= 0 {
		t.Fatal("commission provenance mismatch", got)
	}
	if len(evidence.GetCurrentFee().GetComponents()) != 3 || evidence.GetCurrentFee().GetFeeCurrencyMode() != "UNKNOWN_FROM_COMMISSION_ENDPOINT" {
		t.Fatalf("collapsed fee components or invented currency: %+v", evidence.GetCurrentFee())
	}
	if got := evidence.GetCurrentFee().GetComponents()[2].GetMakerRate(); got != "0.00000112" {
		t.Fatalf("JSON number tax rate was not retained exactly: %q", got)
	}
	fullRate := commission
	for _, tc := range []struct {
		name         string
		status, size int
		ok           bool
	}{
		{"non-success source", http.StatusBadRequest, 0, false}, {"complete source exactly at limit", http.StatusOK, 1 << 20, true}, {"discarded source suffix", http.StatusOK, (1 << 20) + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := fullRate
			if tc.size > 0 {
				data += strings.Repeat(" ", tc.size-len(data))
			}
			mu.Lock()
			commission = data
			responseStatus = tc.status
			mu.Unlock()
			got, err := v.ReadCapitalEvidence(t.Context(), "BTC-USDT", "account")
			if tc.ok {
				if err != nil || got.GetCurrentFee().GetSourceVersion().GetContentSha256() != fmt.Sprintf("%x", sha256.Sum256([]byte(data))) {
					t.Fatalf("full source at limit: %v", err)
				}
			} else if err == nil || got != nil {
				t.Fatal("unverified source became capital evidence")
			}
		})
	}
	mu.Lock()
	commission = fullRate
	responseStatus = http.StatusOK
	mu.Unlock()
	mu.Lock()
	original := commission
	commission = strings.Replace(commission, "BTCUSDT", "ETHUSDT", 1)
	mu.Unlock()
	if result, err := v.ReadCapitalEvidence(t.Context(), "BTC-USDT", "account"); err == nil || result != nil {
		t.Fatal("wrong commission symbol became financial evidence")
	}
	mu.Lock()
	commission = original
	quote = "USD"
	mu.Unlock()
	if _, err := v.ReadCapitalEvidence(context.Background(), "BTC-USDT", "account"); err == nil || !strings.Contains(err.Error(), "configured as") {
		t.Fatalf("exchange settlement mismatch accepted: %v", err)
	}
	if _, err := v.ReadCapitalEvidence(context.Background(), "BTC-USDT", "other-account"); err == nil {
		t.Fatal("read another account's financial evidence")
	}
	mu.Lock()
	quote = "USDT"
	commission = strings.Replace(commission, `"taker":"0.002"`, `"taker":"unknown"`, 1)
	mu.Unlock()
	if _, err := v.ReadCapitalEvidence(context.Background(), "BTC-USDT", "account"); err == nil {
		t.Fatal("accepted malformed current fee evidence")
	}
}
