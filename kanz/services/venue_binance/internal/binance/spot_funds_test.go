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

	"github.com/eighred/kanz/internal/dec"
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
	const schedule = `{"symbol":"BTCUSDT","standardCommission":{"taker":"0.001","buyer":"0.0002","seller":"0"},"taxCommission":{"taker":"0.00001","buyer":"0","seller":"0.00003"},"specialCommission":{"taker":"0","buyer":"0","seller":"0.0001"},"discount":{"enabledForAccount":false,"enabledForSymbol":true}}`
	commission := schedule
	feeCalls := 0
	v.rest.httpc.Transport = diagnosticRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		query := r.URL.Query()
		sig := query.Get("signature")
		query.Del("signature")
		mac := hmac.New(sha256.New, []byte("s"))
		mac.Write([]byte(query.Encode()))
		if r.Method != "GET" || r.URL.Host != "testnet.binance.vision" ||
			r.Header.Get("X-MBX-APIKEY") != "k" || sig != hex.EncodeToString(mac.Sum(nil)) {
			t.Fatal("expected signed spot-testnet account read")
		}
		payload := body
		switch r.URL.Path {
		case "/api/v3/account":
		case "/api/v3/account/commission":
			if query.Get("symbol") != "BTCUSDT" {
				t.Fatal("incorrect commission symbol")
			}
			feeCalls++
			payload = commission
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
	if err != nil || got.GetAvailableBase() == nil || got.GetAvailableBase().Coefficient != 0 ||
		got.GetAvailableQuote().GetCoefficient() != 900719925474099301 || got.AvailableQuote.Exponent != -2 ||
		got.ReceivedAt.AsTime().Before(got.RequestStartedAt.AsTime()) {
		t.Fatalf("incorrect available funds: %v, %v", got, err)
	}
	if feeCalls != 0 || got.TakerSchedule != nil {
		t.Fatal("unsolicited private schedule query")
	}
	req.IncludeTakerSchedule = true
	got, err = read()
	fees := got.GetTakerSchedule()
	if err != nil || fees.GetBuy().GetCurrency() != "BTC" || fees.GetSell().GetCurrency() != "USDT" ||
		dec.Cmp(fees.Buy.Bps, bdec("12.1")) != 0 || dec.Cmp(fees.Sell.Bps, bdec("11.4")) != 0 ||
		fees.RequestStartedAt.AsTime().Before(got.ReceivedAt.AsTime()) || fees.ReceivedAt.AsTime().Before(fees.RequestStartedAt.AsTime()) {
		t.Fatalf("wrong signed fee schedule or clocks: %v, %v", got, err)
	}
	for _, bad := range []string{strings.Replace(schedule, "false", "true", 1), strings.Replace(schedule, "BTCUSDT", "ETHUSDT", 1),
		strings.Replace(schedule, "taxCommission", "missing", 1), strings.Replace(schedule, "enabledForSymbol", "missing", 1)} {
		commission = bad
		got, err = read()
		if err != nil || got.GetAvailableQuote() == nil || got.TakerSchedule != nil {
			t.Fatal("uncertain fee rate accepted or funds discarded")
		}
	}
	beforeBadFunds := feeCalls
	for _, bad := range []string{
		strings.Replace(body, `"uid":42`, `"uid":43`, 1),
		strings.Replace(body, `"canTrade":true`, `"canTrade":false`, 1),
		strings.Replace(body, `"free":"0"`, `"free":""`, 1),
		strings.Replace(body, `"asset":"BTC"`, `"asset":"USDT"`, 1),
		strings.Replace(body, `"free":"0"`, `"free":"-1"`, 1),
	} {
		body = bad
		if resp, err := read(); resp != nil || err == nil || feeCalls != beforeBadFunds {
			t.Fatal("invalid account funds were returned")
		}
	}
	prior := calls
	v.rest.baseURL = "https://api.binance.com"
	if resp, err := read(); resp != nil || err == nil || prior != calls {
		t.Fatal("demo reader contacted a live venue")
	}
}
