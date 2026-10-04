package binance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBinanceCapitalEvidenceReadsSignedCurrentCommissionAndExchangeSettlement(t *testing.T) {
	quote := "USDT"
	commission := `{"symbol":"BTCUSDT","standardCommission":{"maker":"0.001","taker":"0.002","buyer":"0","seller":"0"},"specialCommission":{"maker":"0","taker":"0","buyer":"0","seller":"0"},"taxCommission":{"maker":0.00000112,"taker":0.00000114,"buyer":0,"seller":0},"discount":{"discountAsset":"BNB"}}`
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
		if r.Header.Get("X-MBX-APIKEY") != "key" || r.URL.Query().Get("signature") == "" || r.URL.Query().Get("symbol") != "BTCUSDT" {
			t.Error("current commission request was not signed and symbol scoped")
		}
		mu.Lock()
		body := commission
		mu.Unlock()
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
	if len(evidence.GetCurrentFee().GetComponents()) != 3 || evidence.GetCurrentFee().GetFeeCurrencyMode() != "UNKNOWN_FROM_COMMISSION_ENDPOINT" {
		t.Fatalf("collapsed fee components or invented currency: %+v", evidence.GetCurrentFee())
	}
	if got := evidence.GetCurrentFee().GetComponents()[2].GetMakerRate(); got != "0.00000112" {
		t.Fatalf("JSON number tax rate was not retained exactly: %q", got)
	}
	mu.Lock()
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
