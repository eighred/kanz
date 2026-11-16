package server

import (
	"errors"
	"net/http"

	"github.com/eighred/kanz/internal/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type issuedInvitation struct {
	invitationJSON
	Token string `json:"invite_token"`
	Note  string `json:"note"`
}

func inviteRevision(w http.ResponseWriter, r *http.Request) (int64, bool) {
	var req struct {
		Revision *revisionJSON `json:"revision"`
	}
	if !decodeAdministration(w, r, &req) {
		return 0, false
	}
	if req.Revision == nil || *req.Revision < 0 {
		writeErr(w, 400, "a nonnegative revision is required")
		return 0, false
	}
	return int64(*req.Revision), true
}

func (s *Server) revokeInvite(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	c, ok := s.operator(w, r)
	if !ok {
		return
	}
	revision, ok := inviteRevision(w, r)
	if !ok {
		return
	}
	i, err := s.provisioning.Store.RevokeInvite(r.Context(), administration(c), r.PathValue("id"), revision, s.now().UTC())
	if err != nil {
		s.invitationError(w, err)
		return
	}
	writeJSON(w, 200, invitationResponse(i.Summary(s.now().UTC())))
}

func (s *Server) reissueInvite(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	c, ok := s.operator(w, r)
	if !ok {
		return
	}
	revision, ok := inviteRevision(w, r)
	if !ok {
		return
	}
	raw, hash, err := identity.NewInviteToken()
	if err != nil {
		s.invitationError(w, err)
		return
	}
	i, err := s.provisioning.Store.ReissueInvite(r.Context(), administration(c), r.PathValue("id"), revision, uuid.NewString(), hash, s.provisioning.InviteDomains, s.now().UTC(), s.provisioning.InviteTTL)
	if err != nil {
		s.invitationError(w, err)
		return
	}
	writeJSON(w, 201, issuedInvitation{invitationJSON: invitationResponse(i.Summary(s.now().UTC())), Token: raw, Note: "the original invitation is revoked; this token is shown once and cannot be recovered"})
}

func (s *Server) invitationError(w http.ResponseWriter, err error) {
	var pgerr *pgconn.PgError
	switch {
	case errors.Is(err, identity.ErrAdminAuthority):
		writeErr(w, 401, "the token was rejected")
	case errors.Is(err, identity.ErrInviteNotFound):
		writeErr(w, 404, "no such invitation")
	case errors.Is(err, identity.ErrInviteConflict), errors.As(err, &pgerr) && pgerr.Code == "23505":
		writeErr(w, 409, identity.ErrInviteConflict.Error())
	case errors.Is(err, identity.ErrInvitePolicy):
		writeErr(w, 403, identity.ErrInvitePolicy.Error())
	case errors.Is(err, identity.ErrInvalidAccess), errors.Is(err, identity.ErrAdminRoleCombination):
		writeErr(w, 400, "invitation authority does not satisfy current policy")
	default:
		// Database errors may contain the inserted token hash; log no payload.
		s.logger.Error("invitation lifecycle transaction failed")
		writeErr(w, 503, "invitation change is unavailable")
	}
}
