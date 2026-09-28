package server

import (
	"encoding/json"
	"net/http"

	"github.com/eighred/kanz/internal/identity"
)

func (s *Server) handleRecovery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	paths := map[string]string{"/auth/mailbox": "/mailbox", "/auth/mailbox/verify": "/mailbox/verify", "/auth/recovery": "/recovery", "/auth/recovery/consume": "/recovery/consume"}
	path, ok := paths[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	bearer := ""
	if path == "/mailbox" {
		sess, found := s.currentSession(r)
		if !found {
			writeJSON(w, 401, map[string]string{"error": "invalid credentials"})
			return
		}
		bearer = sess.AccessToken
	}
	if r.Method == http.MethodGet && path == "/mailbox" {
		resp, err := s.identity.Administration(r.Context(), http.MethodGet, path, bearer, nil)
		if err != nil {
			writeJSON(w, 502, map[string]string{"error": "mailbox status unavailable"})
			return
		}
		if resp.Status == 200 {
			var status identity.MailboxStatus
			if err = json.Unmarshal(resp.Body, &status); err != nil {
				writeJSON(w, 502, map[string]string{"error": "mailbox status unavailable"})
				return
			}
			writeJSON(w, 200, status)
			return
		}
		code := resp.Status
		if code != 401 && code != 404 {
			code = 502
		}
		writeJSON(w, code, map[string]string{"error": "mailbox status unavailable"})
		return
	}
	body, ok := s.readProxyBody(w, r)
	if !ok {
		return
	}
	resp, err := s.identity.Recovery(r.Context(), path, bearer, body, s.clientIP.Resolve(r))
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "recovery unavailable"})
		return
	}
	if resp.Status == http.StatusAccepted || resp.Status == http.StatusNoContent {
		if path == "/recovery/consume" && resp.Status == http.StatusNoContent {
			if c, e := r.Cookie(sessionCookie); e == nil {
				s.sessions.Delete(c.Value)
			}
			s.clearSessionCookie(w)
		}
		if resp.Status == http.StatusAccepted {
			writeJSON(w, resp.Status, map[string]string{"status": "accepted"})
			return
		}
		w.WriteHeader(resp.Status)
		return
	}
	status, message := resp.Status, "recovery unavailable"
	switch status {
	case 400:
		message = "check the address and password requirements"
	case 401:
		message = "credentials or link not accepted"
	case 429:
		message = "too many attempts"
	case 404:
		message = "recovery is not configured"
	case 413:
		message = "request too large"
	case 408:
		message = "request timed out"
	default:
		status = 502
	}
	writeJSON(w, status, map[string]string{"error": message})
}
