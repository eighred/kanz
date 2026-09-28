package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/eighred/kanz/internal/identity"
)

type RecoveryStore interface {
	MailboxStatus(context.Context, identity.Administration, time.Time) (*identity.MailboxStatus, error)
	EnrollMailbox(context.Context, identity.Administration, identity.Hash, string, time.Time) error
	RequestRecovery(context.Context, string, time.Time) error
	ConsumeChallenge(context.Context, string, string, identity.Hash, time.Time) error
}

func (s *Server) mailboxStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	raw, ok := bearer(r)
	if !ok {
		writeErr(w, 401, "invalid credentials")
		return
	}
	c, err := s.provisioning.Verifier.Verify(raw, s.now().UTC())
	if err != nil {
		writeErr(w, 401, "invalid credentials")
		return
	}
	status, err := s.recovery.MailboxStatus(r.Context(), administration(c), s.now().UTC())
	if errors.Is(err, identity.ErrCredentialMismatch) || errors.Is(err, identity.ErrRecovery) {
		writeErr(w, 401, "invalid credentials")
		return
	}
	if err != nil {
		writeErr(w, 503, "mailbox status unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

func WithRecovery(store RecoveryStore) Option { return func(s *Server) { s.recovery = store } }

func decodeRecovery(w http.ResponseWriter, r *http.Request, dst any) bool {
	w.Header().Set("Cache-Control", "no-store")
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		writeErr(w, 400, "malformed request")
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		writeErr(w, 400, "malformed request")
		return false
	}
	return true
}

func (s *Server) enrollMailbox(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	raw, ok := bearer(r)
	if !ok {
		writeErr(w, 401, "invalid credentials")
		return
	}
	c, err := s.provisioning.Verifier.Verify(raw, s.now().UTC())
	if err != nil {
		writeErr(w, 401, "invalid credentials")
		return
	}
	if !s.allowAttempt(r, c.Subject) {
		writeErr(w, 429, "too many attempts")
		return
	}
	var req struct {
		Address    string `json:"address"`
		Credential string `json:"credential"`
	}
	if !decodeRecovery(w, r, &req) {
		return
	}
	u, err := s.store.UserBySubject(r.Context(), c.Subject)
	if err != nil && !errors.Is(err, identity.ErrUserNotFound) {
		writeErr(w, 503, "mailbox verification unavailable")
		return
	}
	hash := s.decoyHash
	if u != nil {
		hash = u.Credential
	}
	verified := identity.Verify(hash, req.Credential) == nil
	actor := administration(c)
	if u != nil && u.MFA.Required && !c.MFA.Recent(s.now().UTC()) {
		writeErr(w, 403, "recent MFA required")
		return
	}
	if !verified || !actor.Current(u) {
		writeErr(w, 401, "invalid credentials")
		return
	}
	if s.provisioning.InviteDomains.Check(req.Address) != nil {
		writeErr(w, 400, "mailbox does not satisfy the configured domain policy")
		return
	}
	err = s.recovery.EnrollMailbox(r.Context(), actor, u.Credential, req.Address, s.now().UTC())
	if errors.Is(err, identity.ErrRecoveryCooldown) {
		writeErr(w, 429, "wait before requesting another message")
		return
	}
	if errors.Is(err, identity.ErrCredentialMismatch) {
		writeErr(w, 401, "invalid credentials")
		return
	}
	if errors.Is(err, identity.ErrRecovery) {
		writeErr(w, 400, "invalid mailbox")
		return
	}
	if err != nil {
		writeErr(w, 503, "mailbox verification unavailable")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) requestRecovery(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Subject string `json:"subject"`
	}
	if !decodeRecovery(w, r, &req) {
		return
	}
	if len(req.Subject) == 0 || len(req.Subject) > 256 {
		writeErr(w, 400, "invalid request")
		return
	}
	if !s.allowAttempt(r, req.Subject) {
		writeErr(w, 429, "too many attempts")
		return
	}
	if err := s.recovery.RequestRecovery(r.Context(), req.Subject, s.now().UTC()); err != nil {
		writeErr(w, 503, "recovery unavailable")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) verifyMailbox(w http.ResponseWriter, r *http.Request) {
	s.consumeRecovery(w, r, "verify")
}
func (s *Server) resetCredential(w http.ResponseWriter, r *http.Request) {
	s.consumeRecovery(w, r, "recover")
}
func (s *Server) consumeRecovery(w http.ResponseWriter, r *http.Request, purpose string) {
	var req struct {
		Token      string `json:"token"`
		Credential string `json:"credential"`
	}
	if !decodeRecovery(w, r, &req) {
		return
	}
	// Shared per-source bound; random tokens must not allocate a limiter key.
	if !s.allowAttempt(r, "") {
		writeErr(w, 429, "too many attempts")
		return
	}
	if len(req.Token) != 43 {
		writeErr(w, 401, "invalid or expired link")
		return
	}
	var replacement identity.Hash
	if purpose == "recover" {
		if err := identity.ValidateCredential(req.Credential); err != nil {
			writeErr(w, 400, "password must contain 12 to 1024 characters")
			return
		}
		var err error
		replacement, err = identity.HashCredential(req.Credential)
		if err != nil {
			writeErr(w, 503, "recovery unavailable")
			return
		}
	} else if req.Credential != "" {
		writeErr(w, 400, "unexpected credential")
		return
	}
	err := s.recovery.ConsumeChallenge(r.Context(), req.Token, purpose, replacement, s.now().UTC())
	if errors.Is(err, identity.ErrRecovery) {
		writeErr(w, 401, "invalid or expired link")
		return
	}
	if err != nil {
		writeErr(w, 503, "recovery unavailable")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
