package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/accounting/internal/collateralops"
	"google.golang.org/protobuf/encoding/protojson"
)

func WithCollateral(s *collateralops.Store) Option {
	return func(server *Server) { server.collateral = s }
}

func (s *Server) collateralRoutes() {
	if s.collateral == nil {
		return
	}
	s.mux.HandleFunc("POST /v1/portfolios/{id}/collateral/actions", s.collateralAction)
	s.mux.HandleFunc("GET /v1/portfolios/{id}/collateral/{workflow}", s.collateralRead)
	s.mux.HandleFunc("GET /v1/portfolios/{id}/collateral/{workflow}/proof", s.collateralProof)
}

func (s *Server) collateralProof(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.collateralAuthorize(w, r, "", r.PathValue("workflow")); !ok {
		return
	}
	proof, err := s.collateral.Proof(r.Context(), s.tenant, r.PathValue("workflow"))
	if err != nil {
		http.Error(w, "verified allocation proof unavailable", 503)
		return
	}
	writeJSON(w, http.StatusOK, proof)
}
func (s *Server) collateralAuthorize(w http.ResponseWriter, r *http.Request, snapshot, workflow string) (*auth.Principal, bool) {
	if !s.callerOwnsThisInstance(w, r) {
		return nil, false
	}
	p, ok := auth.PrincipalFromHeaders(r.Header)
	if !ok || !auth.PortfolioEntitled(p.Portfolios, r.PathValue("id")) {
		http.Error(w, "portfolio not found", 404)
		return nil, false
	}
	portfolio, err := s.collateral.Portfolio(r.Context(), s.tenant, snapshot, workflow)
	if err != nil || portfolio != r.PathValue("id") {
		http.Error(w, "portfolio not found", 404)
		return nil, false
	}
	return p, true
}
func (s *Server) collateralRead(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.collateralAuthorize(w, r, "", r.PathValue("workflow")); !ok {
		return
	}
	state, err := s.collateral.Load(r.Context(), s.tenant, r.PathValue("workflow"))
	if err != nil {
		http.Error(w, "collateral unavailable", 503)
		return
	}
	blob, err := protojson.Marshal(state)
	if err != nil {
		http.Error(w, "collateral unavailable", 503)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(blob)
}
func (s *Server) collateralAction(w http.ResponseWriter, r *http.Request) {
	if !s.callerOwnsThisInstance(w, r) {
		return
	}
	var body struct {
		RequestID   string `json:"request_id"`
		WorkflowID  string `json:"workflow_id"`
		SnapshotID  string `json:"snapshot_id"`
		Kind        string `json:"action"`
		Revision    string `json:"expected_revision"`
		Explanation string `json:"explanation"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(&body) != nil {
		http.Error(w, "invalid collateral action", 400)
		return
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		http.Error(w, "invalid collateral action", 400)
		return
	}
	revision, err := strconv.ParseInt(body.Revision, 10, 64)
	if err != nil {
		http.Error(w, "expected revision required", 400)
		return
	}
	workflow := body.WorkflowID
	if body.Kind == "propose" {
		workflow = ""
	}
	p, ok := s.collateralAuthorize(w, r, body.SnapshotID, workflow)
	if !ok {
		return
	}
	state, err := s.collateral.Apply(r.Context(), collateralops.Action{Tenant: s.tenant, Actor: p.Subject, RequestID: body.RequestID, WorkflowID: body.WorkflowID, SnapshotID: body.SnapshotID, Kind: body.Kind, ExpectedRevision: revision, Explanation: body.Explanation})
	if err != nil {
		status := 503
		switch {
		case errors.Is(err, collateralops.ErrInput):
			status = 400
		case errors.Is(err, collateralops.ErrNotFound):
			status = 404
		case errors.Is(err, collateralops.ErrConflict), errors.Is(err, collateralops.ErrTransition), errors.Is(err, collateralops.ErrStale), errors.Is(err, collateralops.ErrUnsupported):
			status = 409
		}
		http.Error(w, "collateral action refused; refresh the input and workflow state", status)
		return
	}
	blob, err := protojson.Marshal(state)
	if err != nil {
		http.Error(w, "collateral unavailable", 503)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(blob)
}
