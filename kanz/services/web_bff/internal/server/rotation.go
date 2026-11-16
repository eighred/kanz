package server

import "net/http"

func (s *Server) handleCredentialRotation(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.currentSession(w, r)
	if !ok {
		return
	}
	body, ok := s.readProxyBody(w, r)
	if !ok {
		return
	}
	resp, err := s.identity.RotateCredential(r.Context(), sess.AccessToken, body, s.clientIP.Resolve(r))
	if err != nil {
		s.fail(w, 502, "the identity service is unavailable", err)
		return
	}
	if resp.Status == http.StatusNoContent {
		if err := s.sessions.Delete(r.Context(), sessionID(r)); err != nil {
			s.sessionError(w, err)
			return
		}
		s.clearSessionCookie(w)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Never relay an upstream body from a credential operation. These fixed
	// messages also prevent a future driver error from echoing credential data.
	status, message := resp.Status, "credential change is unavailable"
	switch status {
	case 403:
		message = "Recent MFA required. Verify your security key on the Authentication page."
	case 401:
		message = "invalid credentials"
	case 400:
		message = "use a different password of 12 to 1024 characters"
	case 429:
		message = "too many attempts, try again shortly"
	case 413:
		message = "request body too large"
	case 408:
		message = "request body timed out"
	default:
		status = 502
	}
	writeJSON(w, status, map[string]string{"error": message})
}
