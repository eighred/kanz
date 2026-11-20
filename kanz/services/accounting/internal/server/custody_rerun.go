package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/custody"
)

type ManualCustodyRerun func(context.Context, custody.Subject, string, string) (custody.Run, error)

func WithCustodyReruns(role string, compare ManualCustodyRerun) Option {
	return func(s *Server) { s.rerunRole = role; s.rerun = compare }
}

type rerunReceipt struct {
	RequestID       string    `json:"request_id"`
	Actor           string    `json:"actor"`
	RunID           string    `json:"run_id"`
	PortfolioID     string    `json:"portfolio_id"`
	CustodianID     string    `json:"custodian_id"`
	BusinessDate    string    `json:"business_date"`
	Outcome         string    `json:"outcome"`
	StatementID     string    `json:"statement_id,omitempty"`
	CompletedAt     time.Time `json:"completed_at"`
	ScopeMode       string    `json:"scope_mode,omitempty"`
	JournalPosition string    `json:"journal_position,omitempty"`
}

func (s *Server) handleCustodyRerun(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	portfolio := r.PathValue("id")
	principal, ok := s.portfolioReader(w, r, portfolio)
	if !ok {
		return
	}
	if !principal.HasRole(s.rerunRole) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "custody rerun grant required"})
		return
	}
	var request struct {
		RequestID    string `json:"request_id"`
		CustodianID  string `json:"custodian_id"`
		BusinessDate string `json:"business_date"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(new(json.RawMessage)) != io.EOF {
		writeJSON(w, 400, map[string]string{"error": "invalid custody rerun request"})
		return
	}
	day, err := time.Parse(time.DateOnly, request.BusinessDate)
	if err != nil || day.IsZero() || !day.Before(custody.BusinessDay(time.Now())) {
		writeJSON(w, 400, map[string]string{"error": "a completed UTC business date is required"})
		return
	}
	if !slices.Contains(s.custodyScope.Custodians(portfolio), request.CustodianID) {
		writeJSON(w, 404, map[string]string{"error": notFoundBody})
		return
	}
	subject := custody.Subject{PortfolioID: portfolio, CustodianID: request.CustodianID, BusinessDate: day}
	run, err := s.rerun(bus.WithTenantID(r.Context(), principal.Tenant), subject, request.RequestID, principal.Subject)
	if err != nil {
		switch {
		case errors.Is(err, custody.ErrInvalidManualRequest):
			writeJSON(w, 400, map[string]string{"error": "invalid custody request identity or scope"})
		case errors.Is(err, custody.ErrManualRequestConflict):
			writeJSON(w, 409, map[string]string{"error": "request identity is bound to another comparison"})
		case errors.Is(err, custody.ErrHistoricalLifecycle):
			writeJSON(w, 409, map[string]string{"error": "a newer comparison owns the break lifecycle"})
		case errors.Is(err, custody.ErrPublicationPending):
			writeJSON(w, 503, map[string]string{"error": "completion publication is pending; retry the same request"})
		default:
			writeJSON(w, 503, map[string]string{"error": "comparison unavailable; retry the same request"})
		}
		return
	}
	if run.RequestID != request.RequestID || run.Actor != principal.Subject || run.Subject.PortfolioID != portfolio || run.Subject.CustodianID != request.CustodianID || !run.Subject.BusinessDate.Equal(day) || run.RunID == "" || run.CompletedAt.IsZero() || run.Outcome == custody.OutcomeUnspecified {
		writeJSON(w, 503, map[string]string{"error": "committed comparison receipt unavailable"})
		return
	}
	response := rerunReceipt{RequestID: run.RequestID, Actor: run.Actor, RunID: run.RunID, PortfolioID: run.Subject.PortfolioID, CustodianID: run.Subject.CustodianID, BusinessDate: day.Format(time.DateOnly), Outcome: run.Outcome.String(), StatementID: run.StatementID, CompletedAt: run.CompletedAt.UTC()}
	if run.Basis != nil {
		response.ScopeMode = run.Basis.Mode
		response.JournalPosition = strconv.FormatInt(run.Basis.JournalPosition, 10)
	}
	writeJSON(w, 200, response)
}
