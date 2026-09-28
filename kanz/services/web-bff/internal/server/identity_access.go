package server

import (
	"net/http"
	"net/url"
	"strings"
)

func (s *Server) handleIdentityAccess(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.currentSession(r)
	if !ok {
		writeJSON(w, 401, map[string]string{"error": "not authenticated"})
		return
	}
	path := "/permissions"
	if r.URL.Path != "/auth/permissions" {
		path = "/users"
		if r.Method == http.MethodGet {
			path += "?after=" + url.QueryEscape(r.URL.Query().Get("after"))
		} else {
			// Escape the subject as one segment. Never relay a caller-controlled path.
			action := "access"
			if strings.HasSuffix(r.Pattern, "/disable") {
				action = "disable"
			}
			if strings.HasSuffix(r.Pattern, "/enable") {
				action = "enable"
			}
			path += "/" + url.PathEscape(r.PathValue("subject")) + "/" + action
		}
	}
	body, ok := s.readProxyBody(w, r)
	if !ok {
		return
	}
	resp, err := s.identity.Administration(r.Context(), r.Method, path, sess.AccessToken, body)
	if err != nil {
		s.fail(w, 502, "the identity service is unavailable", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}
