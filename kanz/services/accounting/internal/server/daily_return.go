package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/return_basis"
	"google.golang.org/protobuf/encoding/protojson"
)

func (s *Server) dailyReturn(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	portfolio := r.PathValue("id")
	principal, ok := s.portfolioReader(w, r, portfolio)
	if !ok {
		return
	}
	q := r.URL.Query()
	day, dateErr := time.Parse(time.DateOnly, q.Get("close_date"))
	horizon, timeErr := time.Parse(time.RFC3339Nano, q.Get("as_of"))
	ccy := q.Get("currency")
	if len(q) != 3 || len(q["as_of"]) != 1 || len(q["close_date"]) != 1 || len(q["currency"]) != 1 ||
		dateErr != nil || timeErr != nil || horizon.Before(day) || horizon.Nanosecond()%1000 != 0 ||
		horizon.After(time.Now()) || len(ccy) != 3 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a closed day, currency and explicit knowledge time are required"})
		return
	}
	report, err := s.returnBasis.ReadObserved(bus.WithTenantID(r.Context(), principal.Tenant), portfolio, ccy, q.Get("close_date"), horizon)
	if errors.Is(err, returnbasis.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": notFoundBody})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "daily return unavailable"})
		return
	}
	data, err := (protojson.MarshalOptions{UseProtoNames: true, EmitDefaultValues: true}).Marshal(report)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "daily return unavailable"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}
