package server

import "net/http"

// Inventory derives its owner exclusively from the opaque current cookie.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.sessions.List(r.Context(), sessionID(r))
	if err != nil {
		s.sessionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": rows})
}
func (s *Server) handleRevokeSession(w http.ResponseWriter, r *http.Request) {
	id := sessionID(r)
	// Revoke rechecks the actor and target atomically, including current-session revocation.
	if err := s.sessions.Revoke(r.Context(), id, r.PathValue("id")); err != nil {
		s.sessionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"revoked": true})
}
