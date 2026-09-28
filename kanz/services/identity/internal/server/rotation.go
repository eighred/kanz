package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/eighred/kanz/internal/identity"
)

func (s *Server) rotateCredential(w http.ResponseWriter, r *http.Request) {
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
		Current     string `json:"current_credential"`
		Replacement string `json:"new_credential"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err = dec.Decode(&req); err != nil {
		writeErr(w, 400, "malformed request")
		return
	}
	if err = dec.Decode(new(any)); err != io.EOF {
		writeErr(w, 400, "malformed request")
		return
	}
	u, err := s.store.UserBySubject(r.Context(), c.Subject)
	if err != nil && !errors.Is(err, identity.ErrUserNotFound) {
		writeErr(w, 503, "credential change is unavailable")
		return
	}
	hash := s.decoyHash
	if u != nil {
		hash = u.Credential
	}
	verified := identity.Verify(hash, req.Current) == nil
	actor := administration(c)
	if u != nil && u.MFA.Required && !c.MFA.Recent(s.now().UTC()) {
		writeErr(w, 403, "recent MFA required")
		return
	}
	if !verified || !actor.Current(u) {
		writeErr(w, 401, "invalid credentials")
		return
	}
	if err = identity.ValidateCredential(req.Replacement); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.Current == req.Replacement {
		writeErr(w, 400, "choose a different credential")
		return
	}
	replacement, err := identity.HashCredential(req.Replacement)
	if err != nil {
		writeErr(w, 503, "credential change is unavailable")
		return
	}
	err = s.provisioning.Store.RotateCredential(r.Context(), actor, u.Credential, replacement, s.now().UTC())
	if errors.Is(err, identity.ErrCredentialMismatch) {
		writeErr(w, 401, "invalid credentials")
		return
	}
	if err != nil {
		// Driver errors can carry query parameters. Never log a credential write error.
		s.logger.Error("credential rotation transaction failed", "subject", c.Subject)
		writeErr(w, 503, "credential change is unavailable")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
