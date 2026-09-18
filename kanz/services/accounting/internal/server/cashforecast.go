package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

type cashForecaster interface {
	ForecastCash(context.Context, string, string, time.Time, time.Time) (ledger.CashForecast, error)
}

func (s *Server) cashForecast(w http.ResponseWriter, r *http.Request) {
	if !s.callerOwnsThisInstance(w, r) {
		return
	}
	p, ok := auth.PrincipalFromHeaders(r.Header)
	if !ok || !auth.PortfolioEntitled(p.Portfolios, r.PathValue("id")) {
		http.Error(w, "portfolio not found", http.StatusNotFound)
		return
	}
	reader, ok := s.store.(cashForecaster)
	if !ok {
		http.Error(w, "durable forecast unavailable", http.StatusServiceUnavailable)
		return
	}
	asOf, e1 := time.Parse(time.RFC3339Nano, r.URL.Query().Get("as_of"))
	horizon, e2 := time.Parse(time.RFC3339Nano, r.URL.Query().Get("horizon"))
	if e1 != nil || e2 != nil {
		http.Error(w, "as_of and horizon are required RFC3339 timestamps", 400)
		return
	}
	result, err := reader.ForecastCash(r.Context(), r.PathValue("id"), r.URL.Query().Get("currency"), asOf, horizon)
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, ledger.ErrForecastInput) {
			status = http.StatusBadRequest
		}
		if errors.Is(err, ledger.ErrForecastCapacity) {
			status = http.StatusUnprocessableEntity
		}
		http.Error(w, "cash forecast unavailable for the requested bounds", status)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
