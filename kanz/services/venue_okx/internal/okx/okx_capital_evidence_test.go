package okx

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
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
	const rate = `{"code":"0","data":[{"instType":"SPOT","instId":"BTC-USDT","feeGroup":[{"groupId":"2","maker":"-0.003","taker":"-0.004"},{"groupId":"1","maker":"-0.0008","taker":"-0.001"}],"maker":"-0.5","taker":"-0.5"}]}`
	feeResponse := rate
	responseStatus := http.StatusOK
	var mu sync.Mutex
	signed := func(r *http.Request) bool {
		mac := hmac.New(sha256.New, []byte("secret"))
		fmt.Fprint(mac, r.Header.Get("OK-ACCESS-TIMESTAMP")+r.Method+r.URL.RequestURI())
		return r.Header.Get("OK-ACCESS-KEY") == "key" && r.Header.Get("OK-ACCESS-PASSPHRASE") == "pass" && r.Header.Get("OK-ACCESS-SIGN") == base64.StdEncoding.EncodeToString(mac.Sum(nil))
	}
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
		if !signed(r) || r.Header.Get("x-simulated-trading") != "1" || r.URL.Query().Get("instId") != "BTC-USDT" {
			t.Error("current fee request lost account signing, demo mode or symbol")
		}
		mu.Lock()
		body, status := feeResponse, responseStatus
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/api/v5/account/config", func(w http.ResponseWriter, r *http.Request) {
		if !signed(r) || r.Header.Get("x-simulated-trading") != "1" {
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
	const catalogue = `{"code":"0","data":[{"instId":"BTC-USDT","instType":"SPOT","baseCcy":"BTC","quoteCcy":"USDT","state":"live","groupId":"1"}]}`
	const config = `{"code":"0","data":[{"feeType":"1"}]}`
	if got := evidence.GetSettlementSourceVersion(); got.GetSource() != "okx:/api/v5/public/instruments" || got.GetContentSha256() != fmt.Sprintf("%x", sha256.Sum256([]byte(catalogue))) || got.GetObservedAtUnixMillis() <= 0 {
		t.Fatal("catalogue provenance mismatch", got)
	}
	if got := evidence.GetCurrentFee().GetSourceVersion(); got.GetSource() != "okx:/api/v5/account/trade-fee+/api/v5/account/config" || got.GetContentSha256() != fmt.Sprintf("%x", sha256.Sum256([]byte(rate+config))) || got.GetObservedAtUnixMillis() <= 0 {
		t.Fatal("fee and config provenance mismatch", got)
	}
	if result, err := v.ReadCapitalEvidence(t.Context(), "BTC-USDT", "another-account"); err == nil || result != nil {
		t.Fatal("another account obtained financial evidence")
	}
	for _, tc := range []struct {
		name         string
		status, size int
		ok           bool
	}{
		{"non-success source", http.StatusBadRequest, 0, false}, {"complete source exactly at limit", http.StatusOK, 1 << 20, true}, {"discarded source suffix", http.StatusOK, (1 << 20) + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := rate
			if tc.size > 0 {
				data += strings.Repeat(" ", tc.size-len(data))
			}
			mu.Lock()
			feeResponse = data
			responseStatus = tc.status
			mu.Unlock()
			got, err := v.ReadCapitalEvidence(t.Context(), "BTC-USDT", "account")
			if tc.ok {
				if err != nil || got.GetCurrentFee().GetSourceVersion().GetContentSha256() != fmt.Sprintf("%x", sha256.Sum256([]byte(data+config))) {
					t.Fatalf("full source at limit: %v", err)
				}
			} else if err == nil || got != nil {
				t.Fatal("unverified source became capital evidence")
			}
		})
	}
	mu.Lock()
	feeResponse = rate
	responseStatus = http.StatusOK
	mu.Unlock()
	for _, tc := range []struct{ name, from, to string }{
		{"wrong symbol", "BTC-USDT", "ETH-USDT"}, {"wrong instrument family", "SPOT", "SWAP"},
		{"wrong fee group", `"groupId":"1"`, `"groupId":"3"`}, {"duplicate group", `"groupId":"2"`, `"groupId":"1"`},
		{"unbounded fee rate", "-0.0008", "1e9999999"}, {"missing fee rate", "-0.0008", ""}, {"rational outside exchange schema", "-0.001", "1/20"},
		{"unbounded alternate", `"taker":"-0.001"`, `"taker":"-0.001","rpiMaker":"1e9999999"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			feeResponse = strings.Replace(rate, tc.from, tc.to, 1)
			mu.Unlock()
			if result, err := v.ReadCapitalEvidence(t.Context(), "BTC-USDT", "account"); err == nil || result != nil {
				t.Fatal("unbound or unavailable current fees were accepted")
			}
		})
	}
	mu.Lock()
	feeResponse = rate
	mu.Unlock()
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
