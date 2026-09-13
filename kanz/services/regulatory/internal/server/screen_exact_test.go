package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/dec"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
)

func TestVersionedESGRequiresExplicitPolicyAndTime(t *testing.T) {
	s := screenServer(t, tobaccoClassifier())
	input := map[string]any{"portfolio_id": "test", "base_currency": "USD", "as_of": "2026-01-01T00:00:00Z", "excluded_sectors": []string{"TOBACCO"}, "positions": []map[string]any{{"instrument_id": "BAT", "quantity": "0.000000000001", "market_value": "0.000000000001"}}}
	rec := post(t, s, "/v2/screening/esg", input)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || body["status"] != "COMPLIANCE_STATUS_BREACH" || body["mandate_version"] != "1" || body["evaluated_at"] != input["as_of"] || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("invalid exact contract: %d %s", rec.Code, rec.Body.String())
	}
	for _, key := range []string{"as_of", "base_currency", "excluded_sectors"} {
		saved := input[key]
		delete(input, key)
		if rec := post(t, s, "/v2/screening/esg", input); rec.Code != 400 {
			t.Fatalf("missing %s accepted: %s", key, rec.Body.String())
		}
		input[key] = saved
	}
}

func TestESGExactInputPreservation(t *testing.T) {
	for _, text := range []string{"0.000000000001", "1000000000000", "9007199254740993", "0"} {
		book, err := (screeningRequest{Positions: []screeningPosition{{InstrumentID: "TEST", Quantity: text, MarketValue: text}}}).book()
		if err != nil {
			t.Fatal(err)
		}
		p := book.Positions[0]
		if dec.FromProto(p.Quantity).Cmp(dec.Rat(text)) != 0 || dec.FromProto(p.MarketValue.Amount).Cmp(dec.Rat(text)) != 0 {
			t.Fatalf("changed %s", text)
		}
	}
}

func TestESGTinyExcludedHoldingDoesNotDisappear(t *testing.T) {
	s := screenServer(t, tobaccoClassifier())
	rec := post(t, s, "/v1/screening/esg", map[string]any{
		"portfolio_id": "test", "base_currency": "USD",
		"positions":        []map[string]any{{"instrument_id": "BAT", "quantity": "0.000000000001", "market_value": "0.000000000001"}},
		"excluded_sectors": []string{"TOBACCO"},
	})
	if rec.Code != http.StatusOK || decodeResult(t, rec.Body.String()).GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("tiny excluded holding lost: %d %s", rec.Code, rec.Body.String())
	}
}

func TestESGUnsupportedValuesRefuse(t *testing.T) {
	s := screenServer(t, tobaccoClassifier())
	for _, field := range []string{"quantity", "market_value"} {
		for _, text := range []string{"1/3", "1e999999999", "9223372036854775808", strings.Repeat("9", 401)} {
			p := map[string]any{"instrument_id": "BAT", "quantity": "1", "market_value": "1"}
			p[field] = text
			rec := post(t, s, "/v1/screening/esg", map[string]any{"portfolio_id": "test", "positions": []map[string]any{p}})
			if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), text) {
				t.Fatalf("unsupported value not safely refused: %d %s", rec.Code, rec.Body.String())
			}
		}
	}
}
