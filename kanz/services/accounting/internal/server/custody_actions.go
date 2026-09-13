package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/accounting/internal/custody"
)

func (s *Server) handleAssignBreak(w http.ResponseWriter, r *http.Request) {
	s.custodyAction(w, r, "claim")
}
func (s *Server) handleExplainBreak(w http.ResponseWriter, r *http.Request) {
	s.custodyAction(w, r, "explain")
}
func (s *Server) handleCustodyAction(w http.ResponseWriter, r *http.Request) {
	s.custodyAction(w, r, "")
}

func (s *Server) custodyAction(w http.ResponseWriter, r *http.Request, legacyAction string) {
	if !s.callerOwnsThisInstance(w, r) {
		return
	}
	principal, ok := auth.PrincipalFromHeaders(r.Header)
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "authenticated subject required"})
		return
	}
	actor := principal.Subject
	var body struct {
		RequestID        string `json:"request_id"`
		ExpectedRevision string `json:"expected_revision"`
		Action           string `json:"action"`
		Assignee         string `json:"assignee"`
		Explanation      string `json:"explanation"`
		ReviewContract   string `json:"review_contract"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid custody action"})
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeJSON(w, 400, map[string]string{"error": "invalid custody action"})
		return
	}
	if legacyAction != "" {
		if body.Action != "" && body.Action != legacyAction {
			writeJSON(w, 400, map[string]string{"error": "conflicting custody action"})
			return
		}
		body.Action = legacyAction
	}
	if body.ReviewContract != "" && body.ReviewContract != "exact-v1" {
		writeJSON(w, 400, map[string]string{"error": "unsupported custody review contract"})
		return
	}
	if body.Assignee != "" && (body.Action != "claim" || body.Assignee != actor) {
		writeJSON(w, 400, map[string]string{"error": "assignment is an authenticated self-claim"})
		return
	}
	revision, err := strconv.ParseInt(body.ExpectedRevision, 10, 64)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "expected revision required"})
		return
	}
	evidence, err := s.breaks.ApplyAction(r.Context(), custody.Action{Tenant: s.tenant, Actor: actor, RequestID: body.RequestID, BreakID: r.PathValue("id"), Kind: body.Action, Explanation: body.Explanation, ExpectedRevision: revision})
	if err != nil {
		status := http.StatusInternalServerError
		message := "custody action could not be recorded"
		switch {
		case errors.Is(err, custody.ErrInvalidAction):
			status = 400
			message = "invalid custody action"
		case errors.Is(err, custody.ErrNoBreak):
			status = 404
			message = "no such break"
		case errors.Is(err, custody.ErrStaleRevision), errors.Is(err, custody.ErrActionConflict), errors.Is(err, custody.ErrIllegalTransition):
			status = 409
			message = "custody state or request conflicts; reload before reviewing another action"
		case errors.Is(err, custody.ErrDurableActionStore):
			status = 503
			message = "durable custody actions unavailable"
		case errors.Is(err, custody.ErrUnverifiedPrecision):
			status = 503
			message = "exact custody values unavailable; replay source data and reconcile"
		}
		writeJSON(w, status, map[string]string{"error": message})
		return
	}
	writeJSON(w, http.StatusOK, evidence)
}
