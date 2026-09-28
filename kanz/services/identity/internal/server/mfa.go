package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/eighred/kanz/internal/identity"
	"github.com/go-webauthn/webauthn/webauthn"
)

type MFAStore interface {
	MFAStatus(context.Context, identity.Administration, time.Time) (*identity.MFAStatus, error)
	BeginMFA(context.Context, identity.Administration, identity.Hash, string, string, time.Time, *webauthn.WebAuthn) (*identity.MFACeremony, error)
	FinishMFA(context.Context, *identity.Administration, string, string, []byte, time.Time, *webauthn.WebAuthn) (*identity.User, error)
	RemoveMFA(context.Context, identity.Administration, string, time.Time) (*identity.User, error)
}
type MFAConfig struct {
	Store             MFAStore
	WebAuthn          *webauthn.WebAuthn
	RequirePrivileged bool
}

func WithMFA(c MFAConfig) Option { return func(s *Server) { s.mfa = &c } }

func (s *Server) mfaActor(w http.ResponseWriter, r *http.Request) (*identity.Administration, bool) {
	w.Header().Set("Cache-Control", "no-store")
	raw, ok := bearer(r)
	if !ok {
		writeErr(w, 401, "invalid credentials")
		return nil, false
	}
	c, err := s.provisioning.Verifier.Verify(raw, s.now().UTC())
	if err != nil {
		writeErr(w, 401, "invalid credentials")
		return nil, false
	}
	a := administration(c)
	return &a, true
}
func (s *Server) mfaStatus(w http.ResponseWriter, r *http.Request) {
	a, ok := s.mfaActor(w, r)
	if !ok {
		return
	}
	status, err := s.mfa.Store.MFAStatus(r.Context(), *a, s.now().UTC())
	if err != nil {
		s.mfaError(w, err)
		return
	}
	writeJSON(w, 200, status)
}
func (s *Server) mfaBegin(purpose string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, ok := s.mfaActor(w, r)
		if !ok {
			return
		}
		if !s.allowAttempt(r, a.Subject) {
			writeErr(w, 429, "too many attempts")
			return
		}
		var req struct {
			Name     string `json:"name"`
			Password string `json:"password"`
		}
		if !decodeRecovery(w, r, &req) {
			return
		}
		var previous identity.Hash
		if purpose == "register" {
			u, err := s.store.UserBySubject(r.Context(), a.Subject)
			if err != nil {
				_ = identity.Verify(s.decoyHash, req.Password)
				s.mfaError(w, identity.ErrMFA)
				return
			}
			if identity.Verify(u.Credential, req.Password) != nil {
				s.mfaError(w, identity.ErrMFA)
				return
			}
			previous = u.Credential
		}
		c, err := s.mfa.Store.BeginMFA(r.Context(), *a, previous, purpose, req.Name, s.now().UTC(), s.mfa.WebAuthn)
		if err != nil {
			s.mfaError(w, err)
			return
		}
		writeJSON(w, 200, c)
	}
}
func (s *Server) mfaFinish(purpose string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		var a *identity.Administration
		if purpose != "login" {
			var ok bool
			a, ok = s.mfaActor(w, r)
			if !ok {
				return
			}
		}
		subject := ""
		if a != nil {
			subject = a.Subject
		}
		if !s.allowAttempt(r, subject) {
			writeErr(w, 429, "too many attempts")
			return
		}
		var req struct {
			ID         string          `json:"id"`
			Credential json.RawMessage `json:"credential"`
		}
		if !decodeRecovery(w, r, &req) {
			return
		}
		u, err := s.mfa.Store.FinishMFA(r.Context(), a, req.ID, purpose, req.Credential, s.now().UTC(), s.mfa.WebAuthn)
		if err != nil {
			s.mfaError(w, err)
			return
		}
		s.issue(w, u)
	}
}
func (s *Server) mfaRemove(w http.ResponseWriter, r *http.Request) {
	a, ok := s.mfaActor(w, r)
	if !ok {
		return
	}
	if !s.allowAttempt(r, a.Subject) {
		writeErr(w, 429, "too many attempts")
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if !decodeRecovery(w, r, &req) {
		return
	}
	if len(req.ID) > 2048 {
		writeErr(w, 400, "invalid factor")
		return
	}
	u, err := s.mfa.Store.RemoveMFA(r.Context(), *a, req.ID, s.now().UTC())
	if err != nil {
		s.mfaError(w, err)
		return
	}
	s.issue(w, u)
}
func (s *Server) mfaError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, identity.ErrMFAStepUp):
		writeErr(w, 403, "recent MFA required")
	case errors.Is(err, identity.ErrMFA), errors.Is(err, identity.ErrRecovery):
		writeErr(w, 401, "MFA verification was not accepted")
	case errors.Is(err, identity.ErrMFALastFactor):
		writeErr(w, 409, "retain at least one verified factor")
	default:
		writeErr(w, 503, "MFA service unavailable")
	}
}
