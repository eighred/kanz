package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/accounting/internal/cashmove"
)

type cashMovementRequest struct {
	MovementID     string  `json:"movement_id"`
	Kind           string  `json:"kind"`
	Amount         string  `json:"amount"`
	Currency       string  `json:"currency"`
	Effective      string  `json:"effective"`
	SourceRef      string  `json:"source_ref"`
	VenueAccountID *string `json:"venue_account_id"`
	Reason         string  `json:"reason"`
	ReviewedDigest string  `json:"reviewed_digest"`
}

func (s *Server) cashPrincipal(w http.ResponseWriter, r *http.Request) (*auth.Principal, bool) {
	if !s.callerOwnsThisInstance(w, r) {
		return nil, false
	}
	p, ok := auth.PrincipalFromHeaders(r.Header)
	if !ok || !auth.PortfolioEntitled(p.Portfolios, r.PathValue("id")) {
		http.Error(w, "portfolio not found", 404)
		return nil, false
	}
	return p, true
}

func (s *Server) readCashCommand(w http.ResponseWriter, r *http.Request) (cashmove.Command, string, bool) {
	p, ok := s.cashPrincipal(w, r)
	if !ok {
		return cashmove.Command{}, "", false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	var body cashMovementRequest
	var extra any
	if d.Decode(&body) != nil || d.Decode(&extra) != io.EOF || body.VenueAccountID == nil {
		http.Error(w, "explicit cash command terms required", 400)
		return cashmove.Command{}, "", false
	}
	kind, err := parseKind(body.Kind)
	if err != nil {
		http.Error(w, "invalid cash kind", 400)
		return cashmove.Command{}, "", false
	}
	amount, err := dec.Exact(body.Amount).Rat()
	if err != nil {
		http.Error(w, "invalid exact amount", 400)
		return cashmove.Command{}, "", false
	}
	effective, err := time.Parse(time.RFC3339Nano, body.Effective)
	if err != nil {
		http.Error(w, "explicit effective time required", 400)
		return cashmove.Command{}, "", false
	}
	// An empty account is an explicit declaration of no exchange-account leg.
	// Non-empty accounts must be assigned to this portfolio by configuration;
	// journal contents cannot grant permission to move money into an account.
	if *body.VenueAccountID != "" && (s.custodyScope == nil || !s.custodyScope.CashAccountAllowed(r.PathValue("id"), *body.VenueAccountID)) {
		http.Error(w, "account not found", 404)
		return cashmove.Command{}, "", false
	}
	c := cashmove.Command{Tenant: s.tenant, Actor: p.Subject, Reason: body.Reason, Movement: cashmove.CashMovement{MovementID: body.MovementID, PortfolioID: r.PathValue("id"), Kind: kind, Amount: amount, Currency: body.Currency, Effective: effective, SourceRef: body.SourceRef, VenueAccountID: *body.VenueAccountID}}
	if _, err = cashmove.ReviewDigest(c); err != nil {
		http.Error(w, "invalid cash command terms", 400)
		return cashmove.Command{}, "", false
	}
	return c, body.ReviewedDigest, true
}

func (s *Server) previewCashMovement(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.readCashCommand(w, r)
	if !ok {
		return
	}
	digest, err := cashmove.ReviewDigest(c)
	if err != nil {
		http.Error(w, "invalid cash command terms", 400)
		return
	}
	writeJSON(w, 200, map[string]string{"reviewed_digest": digest, "actor": c.Actor, "status": "review_required"})
}

func (s *Server) handleCashMovement(w http.ResponseWriter, r *http.Request) {
	c, reviewed, ok := s.readCashCommand(w, r)
	if !ok {
		return
	}
	receipt, err := s.cashCommands.Accept(r.Context(), c, reviewed)
	if err != nil {
		cashCommandError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, receipt)
}

func (s *Server) cashMovementStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.cashPrincipal(w, r); !ok {
		return
	}
	receipt, err := s.cashCommands.Status(r.Context(), s.tenant, r.PathValue("id"), r.PathValue("movement"))
	if err != nil {
		cashCommandError(w, err)
		return
	}
	writeJSON(w, 200, receipt)
}

func cashCommandError(w http.ResponseWriter, err error) {
	status := 503
	switch {
	case errors.Is(err, cashmove.ErrCommandInput):
		status = 400
	case errors.Is(err, cashmove.ErrCommandConflict):
		status = 409
	case errors.Is(err, cashmove.ErrCommandNotFound):
		status = 404
	}
	http.Error(w, "cash command refused", status)
}
