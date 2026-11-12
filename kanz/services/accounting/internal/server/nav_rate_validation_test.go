package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNAVEndpointRefusesNonPositiveRequestFX(t *testing.T) {
	for _, rate := range []string{"0", "-1", "-1/100"} {
		t.Run(rate, func(t *testing.T) {
			s, store := newServer(t)
			seedEUR(t, store)
			body := `{"prices":{"SAP":"120"},"instrument_currency":{"SAP":"EUR"},"fx":{"EUR":"` + rate + `"}}`
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, postV1(http.MethodPost, "/v1/portfolios/PF/nav", strings.NewReader(body)))
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "EUR") {
				t.Fatalf("invalid FX produced status %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}
