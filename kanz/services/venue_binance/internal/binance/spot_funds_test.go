package binance

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/venueadapter/server"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
)

func TestDemoSpotFundsProjectsOnlyFreeAmountsFromMatchingSignedAccount(t *testing.T) {
	v := venueOverFake(newFakeBinance(t))
	v.account = "portfolio-binance"
	v.symbols = StaticSymbolMap{"BTC-USDT": "BTCUSDT"}
	v.rest.baseURL = "https://testnet.binance.vision"
	body := `{"uid":42,"accountType":"SPOT","canTrade":true,"balances":[{"asset":"BTC","free":"0","locked":"7"},{"asset":"USDT","free":"9007199254740993.01","locked":"20"}]}`
	calls := 0
	v.rest.httpc.Transport = diagnosticRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		query := r.URL.Query()
		sig := query.Get("signature")
		query.Del("signature")
		mac := hmac.New(sha256.New, []byte("s"))
		mac.Write([]byte(query.Encode()))
		if r.Method != "GET" || r.URL.Host != "testnet.binance.vision" || r.URL.Path != "/api/v3/account" ||
			r.Header.Get("X-MBX-APIKEY") != "k" || sig != hex.EncodeToString(mac.Sum(nil)) {
			t.Fatal("expected signed spot-testnet account read")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	s := server.New(v, nil, nil, execution.AccountProof{Verified: true, ExchangeAccountID: "42"}, nil, nil).WithDemoSpotFunds("pilot")
	req := &venuepb.ReadDemoSpotFundsRequest{TenantId: "pilot", VenueAccountId: v.account, Mic: v.MIC(), InstrumentId: "BTC-USDT"}
	read := func() (*venuepb.ReadDemoSpotFundsResponse, error) {
		return s.ReadDemoSpotFunds(context.Background(), req)
	}
	got, err := read()
	if err != nil || got.GetAvailableBase() == nil || got.GetAvailableBase().Coefficient != 0 ||
		got.GetAvailableQuote().GetCoefficient() != 900719925474099301 || got.AvailableQuote.Exponent != -2 ||
		got.ReceivedAt.AsTime().Before(got.RequestStartedAt.AsTime()) {
		t.Fatalf("incorrect available funds: %v, %v", got, err)
	}
	for _, bad := range []string{
		strings.Replace(body, `"uid":42`, `"uid":43`, 1),
		strings.Replace(body, `"canTrade":true`, `"canTrade":false`, 1),
		strings.Replace(body, `"free":"0"`, `"free":""`, 1),
		strings.Replace(body, `"asset":"BTC"`, `"asset":"USDT"`, 1),
		strings.Replace(body, `"free":"0"`, `"free":"-1"`, 1),
	} {
		body = bad
		if resp, err := read(); resp != nil || err == nil {
			t.Fatal("invalid account funds were returned")
		}
	}
	prior := calls
	v.rest.baseURL = "https://api.binance.com"
	if resp, err := read(); resp != nil || err == nil || prior != calls {
		t.Fatal("demo reader contacted a live venue")
	}
}
