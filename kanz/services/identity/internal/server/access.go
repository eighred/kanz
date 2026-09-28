package server

import (
	"errors"
	"net/http"

	"github.com/eighred/kanz/internal/identity"
)

func administration(c *identity.Claims) identity.Administration {
	return identity.Administration{Subject: c.Subject, Tenant: c.Tenant, IssuedAt: c.IssuedAt, SessionEpoch: c.SessionEpoch}
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	c, ok := s.operator(w, r)
	if !ok {
		return
	}
	users, err := s.provisioning.Store.UsersFor(r.Context(), administration(c), r.URL.Query().Get("after"))
	if err != nil {
		s.accessError(w, err)
		return
	}
	next := ""
	if len(users) == identity.UserPageSize {
		next = users[len(users)-1].Subject
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"users": users, "next_cursor": next})
}

func (s *Server) setAccess(w http.ResponseWriter, r *http.Request) {
	c, ok := s.operator(w, r)
	if !ok {
		return
	}
	var req struct {
		Revision   *int64   `json:"revision"`
		Roles      []string `json:"roles"`
		Portfolios []string `json:"portfolios"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Revision == nil || *req.Revision < 0 {
		writeErr(w, 400, "a nonnegative revision is required")
		return
	}
	result, err := s.provisioning.Store.SetAccess(r.Context(), administration(c), r.PathValue("subject"), *req.Revision, req.Roles, req.Portfolios, s.now().UTC())
	if err != nil {
		s.accessError(w, err)
		return
	}
	writeJSON(w, 200, result)
}

func (s *Server) accessError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, identity.ErrAdminAuthority):
		writeErr(w, 401, "the token was rejected")
	case errors.Is(err, identity.ErrUserNotFound):
		writeErr(w, 404, "no such account")
	case errors.Is(err, identity.ErrAccessConflict), errors.Is(err, identity.ErrLastAdmin):
		writeErr(w, 409, err.Error())
	case errors.Is(err, identity.ErrInvalidAccess), errors.Is(err, identity.ErrAdminRoleCombination):
		writeErr(w, 400, err.Error())
	default:
		s.logger.Error("identity access operation failed", "err", err)
		writeErr(w, 503, "account access is unavailable")
	}
}

// Permissions reports identity authority only. Gateway capabilities are served
// by the gateway from its enforcement policy, never inferred from role names.
func (s *Server) permissions(w http.ResponseWriter, r *http.Request) {
	raw, ok := bearer(r)
	if !ok {
		writeErr(w, 401, "a bearer token is required")
		return
	}
	c, err := s.provisioning.Verifier.Verify(raw, s.now().UTC())
	if err != nil {
		writeErr(w, 401, "the token was rejected")
		return
	}
	u, err := s.provisioning.Store.UserBySubject(r.Context(), c.Subject)
	if err != nil && !errors.Is(err, identity.ErrUserNotFound) {
		writeErr(w, 503, "account access is unavailable")
		return
	}
	a := administration(c)
	if !a.Current(u) {
		writeErr(w, 401, "the token was rejected")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	admin := a.Allows(u) && c.HasRole(s.provisioning.AdminRole) && identity.ValidateAdminRoles(c.Roles) == nil
	writeJSON(w, 200, map[string]any{"subject": u.Subject, "tenant": u.Tenant, "identity_admin": admin})
}
