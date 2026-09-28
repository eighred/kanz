package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/services/web-bff/internal/identityclient"
)

func (s *Server) handleMFA(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	path := strings.TrimPrefix(r.URL.Path, "/auth")
	bearer := ""
	if path != "/mfa/login/finish" {
		sess, ok := s.currentSession(w, r)
		if !ok {
			return
		}
		bearer = sess.AccessToken
	}
	var body []byte
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 80<<10)
		var ok bool
		body, ok = s.readProxyBody(w, r)
		if !ok {
			return
		}
	}
	resp, err := s.identity.MFA(r.Context(), r.Method, path, bearer, body, s.clientIP.Resolve(r))
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "MFA unavailable"})
		return
	}
	if resp.Status != 200 {
		status, message := resp.Status, "MFA verification was not accepted"
		switch status {
		case 400, 401:
		case 403:
			message = "Recent MFA required. Verify your security key on the Authentication page."
		case 404:
			message = "MFA is not configured"
		case 409:
			message = "Keep at least one verified factor"
		case 429:
			message = "Too many attempts. Wait before trying again."
		case 413:
			message = "Request too large"
		default:
			status = 502
			message = "MFA unavailable"
		}
		writeJSON(w, status, map[string]string{"error": message})
		return
	}
	if r.Method == http.MethodGet {
		var status identity.MFAStatus
		if json.Unmarshal(resp.Body, &status) != nil {
			writeJSON(w, 502, map[string]string{"error": "MFA status unavailable"})
			return
		}
		writeJSON(w, 200, status)
		return
	}
	if strings.HasSuffix(path, "/begin") {
		ceremony, err := identityclient.ProjectMFACeremony(resp.Body, path == "/mfa/register/begin")
		if err != nil {
			writeJSON(w, 502, map[string]string{"error": "MFA unavailable"})
			return
		}
		writeJSON(w, 200, ceremony)
		return
	}
	var tok identityclient.Token
	if json.Unmarshal(resp.Body, &tok) != nil || tok.Token == "" || tok.Subject == "" || tok.Tenant == "" || tok.MFA != nil {
		writeJSON(w, 502, map[string]string{"error": "MFA unavailable"})
		return
	}
	s.completeLogin(w, r, &tok, nil)
}
