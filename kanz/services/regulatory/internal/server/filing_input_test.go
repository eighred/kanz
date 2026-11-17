package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/dec"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

func TestFilingInputDistinguishesStructAliasesFromInstrumentMapKeys(t *testing.T) {
	const body = `{"portfolio_id":"pf1","as_of":"2026-06-30T00:00:00Z","Holdings":[{"InstrumentID":"A","MarketValue":9007199254740993,"Carbon":{"Scope1":0,"Scope2":0,"Scope3":0,"Revenue":1,"EVIC":1}},{"InstrumentID":"a","MarketValue":1,"Carbon":{"Scope1":0,"Scope2":0,"Scope3":0,"Revenue":1,"EVIC":1}}],"FossilFuel":{"A":false,"a":true}}`
	var request disclosureRequest
	response := httptest.NewRecorder()
	input, ok := readFilingInput(response, httptest.NewRequest(http.MethodPost, "/v1/filings/tcfd", strings.NewReader(body)), &request, nil, &request)
	if !ok || len(input.holdings) != 2 {
		t.Fatal("case-sensitive instrument-map identities were folded together")
	}
	value, valid := dec.FromProtoChecked(input.holdings[0].MarketValue)
	want, _ := dec.FromProtoChecked(mustSourceDecimal(t, "9007199254740993"))
	if !valid || value.Cmp(want) != 0 || !bytes.Contains(input.manifest, []byte(`"market_value":"9007199254740993e0"`)) {
		t.Fatal("exact source token was reconstructed from a rounded analytic float")
	}
	if err := checkFilingJSON([]byte(`{"GrossAssetValue":"1","grossassetvalue":"2"}`), reflect.TypeFor[formPFRequest]()); err == nil {
		t.Fatal("encoding/json struct alias overwrote a source field")
	}
	if err := checkFilingJSON([]byte(`{"FossilFuel":{"A":false,"A":true}}`), reflect.TypeFor[disclosureRequest]()); err == nil {
		t.Fatal("exact duplicate instrument-map key accepted")
	}
}

func mustSourceDecimal(t *testing.T, text string) *commonpb.Decimal {
	t.Helper()
	value, ok := dec.ParseProtoExact(text)
	if !ok {
		t.Fatal("invalid literal exact decimal")
	}
	return value
}

func TestFilingInputRejectsUnknownUnicodeTrailingAndShapeOverflow(t *testing.T) {
	for _, raw := range []string{
		`{"portfolio_id":"\ud800"}`,
		`{"portfolio_id":"pf1"} {}`,
		`{"VaR":null}`,
		strings.Repeat("[", 25) + "0" + strings.Repeat("]", 25),
		"[" + strings.Repeat("0,", 50000) + "0]",
	} {
		if err := checkFilingJSON([]byte(raw), reflect.TypeFor[formPFRequest]()); err == nil {
			t.Fatal("ambiguous or unbounded JSON accepted")
		}
	}
	if err := checkFilingJSON([]byte(`{"portfolio_id":"pf1","as_of":"2026-06-30T00:00:00Z"}`), reflect.TypeFor[formPFRequest]()); err != nil {
		t.Fatal("ordinary known-field JSON refused")
	}
}
