package okx

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/venueadapter/exchangeauth"
	"github.com/eighred/kanz/internal/venueadapter/server"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
)

func TestDemoSpotFundsUsesPairCashCapacityAndVerifiesMode(t *testing.T) {
	v := okxVenueOver(newFakeOKX(t))
	v.account = "portfolio-okx"
	v.symbols = StaticSymbolMap{"BTC-USDT": "BTC-USDT"}
	v.rest.baseURL = "https://www.okx.com"
	config := `{"code":"0","data":[{"uid":"42","acctLv":"1"}]}`
	balances := `{"code":"0","data":[{"instId":"BTC-USDT","availBuy":"325.5","availSell":"0.013","cashBal":"99999"}]}`
	balanceCalls := 0
	v.rest.httpc.Transport = diagnosticRoundTrip(func(r *http.Request) (*http.Response, error) {
		mac := hmac.New(sha256.New, []byte("s"))
		mac.Write([]byte(r.Header.Get("OK-ACCESS-TIMESTAMP") + "GET" + r.URL.RequestURI()))
		if r.Method != "GET" || r.URL.Host != "www.okx.com" || r.Header.Get("x-simulated-trading") != "1" ||
			r.Header.Get("OK-ACCESS-SIGN") != base64.StdEncoding.EncodeToString(mac.Sum(nil)) {
			t.Fatal("expected signed OKX demo read")
		}
		payload := config
		switch r.URL.Path {
		case "/api/v5/account/config":
		case "/api/v5/account/max-avail-size":
			if r.URL.RawQuery != "instId=BTC-USDT&tdMode=cash" {
				t.Fatal("unexpected pair or trading mode")
			}
			payload = balances
			balanceCalls++
		default:
			t.Fatal("unexpected venue request")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(payload))}, nil
	})
	s := server.New(v, nil, nil, execution.AccountProof{Verified: true, ExchangeAccountID: "42"}, nil, nil).WithDemoSpotFunds("pilot")
	req := &venuepb.ReadDemoSpotFundsRequest{TenantId: "pilot", VenueAccountId: v.account, Mic: v.MIC(), InstrumentId: "BTC-USDT"}
	read := func() (*venuepb.ReadDemoSpotFundsResponse, error) {
		return s.ReadDemoSpotFunds(context.Background(), req)
	}
	got, err := read()
	if err != nil || got.GetAvailableBase().GetCoefficient() != 13 || got.AvailableBase.Exponent != -3 ||
		got.GetAvailableQuote().GetCoefficient() != 3255 || got.AvailableQuote.Exponent != -1 {
		t.Fatalf("cash capacity in wrong units: %v, %v", got, err)
	}
	for _, bad := range []string{
		strings.Replace(balances, "BTC-USDT", "ETH-USDT", 1),
		strings.Replace(balances, "325.5", "", 1),
		strings.Replace(balances, "0.013", "999999999999999999999999.01", 1),
	} {
		balances = bad
		if resp, err := read(); resp != nil || err == nil {
			t.Fatal("invalid pair or amount returned")
		}
	}
	prior := balanceCalls
	for _, bad := range []string{strings.Replace(config, "42", "43", 1), strings.Replace(config, `"1"`, `"4"`, 1)} {
		config = bad
		if resp, err := read(); resp != nil || err == nil || balanceCalls != prior {
			t.Fatal("requested funds for a mismatched account or margin mode")
		}
	}
	v.rest.mode = exchangeauth.OKXLive
	if resp, err := read(); resp != nil || err == nil {
		t.Fatal("accepted live trading mode")
	}
}
