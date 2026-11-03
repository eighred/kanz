package okx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/venueadapter/exchangeauth"
)

func TestOKXCapitalEvidenceBindsCurrentFeeToAccountCurrencyMode(t *testing.T) {
	quote := "USDT"
	feeType := "1"
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/public/instruments", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("instId") != "BTC-USDT" || r.URL.Query().Get("instType") != "SPOT" {
			t.Error("instrument catalogue queried the wrong pair or regime")
		}
		mu.Lock()
		body := `{"code":"0","data":[{"instId":"BTC-USDT","instType":"SPOT","baseCcy":"BTC","quoteCcy":"` + quote + `","state":"live","groupId":"1"}]}`
		mu.Unlock()
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/api/v5/account/trade-fee", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("OK-ACCESS-SIGN") == "" || r.Header.Get("x-simulated-trading") != "1" || r.URL.Query().Get("instId") != "BTC-USDT" {
			t.Error("current fee request lost account signing, demo mode or symbol")
		}
		_, _ = w.Write([]byte(`{"code":"0","data":[{"feeGroup":[{"groupId":"2","maker":"-0.003","taker":"-0.004"},{"groupId":"1","maker":"-0.0008","taker":"-0.001"}],"maker":"-0.5","taker":"-0.5"}]}`))
	})
	mux.HandleFunc("/api/v5/account/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("OK-ACCESS-SIGN") == "" || r.Header.Get("x-simulated-trading") != "1" {
			t.Error("account fee currency request lost signing or demo mode")
		}
		mu.Lock()
		body := `{"code":"0","data":[{"feeType":"` + feeType + `"}]}`
		mu.Unlock()
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	rest := newOKXREST(okxRestConfig{BaseURL: srv.URL, APIKey: "key", APISecret: "secret", Passphrase: "pass", Mode: exchangeauth.OKXDemo,
		Buckets: newOKXBuckets(NewWeightBucket(60, 2*time.Second, nil), nil)})
	v := &OKXVenue{mic: "OKX", account: "account", symbols: StaticSymbolMap{"BTC-USDT": "BTC-USDT"}, rest: rest}
	evidence, err := v.ReadCapitalEvidence(context.Background(), "BTC-USDT", "account")
	if err != nil {
		t.Fatal(err)
	}
	if evidence.GetQuoteAsset() != "USDT" || evidence.GetCurrentFee().GetFeeCurrencyMode() != "1" || evidence.GetCurrentFee().GetComponents()[0].GetTakerRate() != "-0.001" {
		t.Fatalf("missing current account fee terms: %+v", evidence)
	}
	if evidence.GetSettlementSourceVersion().GetContentSha256() == "" || evidence.GetCurrentFee().GetSourceVersion().GetContentSha256() == "" {
		t.Fatal("exchange response provenance missing")
	}
	mu.Lock()
	feeType = ""
	mu.Unlock()
	if _, err := v.ReadCapitalEvidence(context.Background(), "BTC-USDT", "account"); err == nil {
		t.Fatal("accepted missing account fee currency mode")
	}
	mu.Lock()
	feeType = "1"
	quote = "USD"
	mu.Unlock()
	if _, err := v.ReadCapitalEvidence(context.Background(), "BTC-USDT", "account"); err == nil || !strings.Contains(err.Error(), "configured as") {
		t.Fatalf("accepted exchange settlement mismatch: %v", err)
	}
}
