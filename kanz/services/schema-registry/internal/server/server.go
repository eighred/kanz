package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/kanz-eng/kanz/services/schema-registry/internal/storage"
)

// maxDescriptorBytes caps an inbound FileDescriptorSet subset. 8 MiB is well
// above any realistic single-message subset (typical: a few KiB).
const maxDescriptorBytes = 8 << 20

type Server struct {
	store  storage.Storage
	logger *slog.Logger
	mux    *http.ServeMux
}

func New(store storage.Storage, logger *slog.Logger) *Server {
	s := &Server{store: store, logger: logger, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	s.mux.HandleFunc("POST /schemas/{schema_id}", s.handleRegister)
	s.mux.HandleFunc("GET /schemas/{ref}", s.handleResolve)
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		s.logger.Warn("readyz: storage ping failed", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "storage_unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleRegister ingests one (schema_id, descriptor) and returns the assigned
// payload_schema_ref. Body is the raw FileDescriptorSet subset for the schema.
// 200 = idempotent re-register (no version bump); 201 = new version.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	schemaID := r.PathValue("schema_id")
	if schemaID == "" {
		http.Error(w, "missing schema_id", http.StatusBadRequest)
		return
	}
	// `:` is the schema-ref delimiter — disallow it in raw schema-ids so
	// resolve URLs stay unambiguous (EVT-16c).
	if strings.ContainsRune(schemaID, ':') {
		http.Error(w, "schema_id must not contain ':'", http.StatusBadRequest)
		return
	}
	sourceTag := r.Header.Get("X-Source-Tag")
	if sourceTag == "" {
		http.Error(w, "missing X-Source-Tag header", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxDescriptorBytes))
	if err != nil {
		http.Error(w, "descriptor too large or unreadable", http.StatusBadRequest)
		return
	}
	if len(body) == 0 {
		http.Error(w, "empty descriptor", http.StatusBadRequest)
		return
	}

	ref, created, err := s.store.Register(r.Context(), schemaID, body, sourceTag)
	if err != nil {
		s.logger.Error("register failed", "schema_id", schemaID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, map[string]any{"ref": ref.String(), "created": created})
}

// handleResolve answers `resolve(payload_schema_ref)`. Body is the raw stored
// descriptor; metadata is in headers so consumers don't pay base64 overhead.
// (schema_id, version) is immutable once published, so the response is
// freely cacheable forever.
func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	ref, err := storage.ParseRef(r.PathValue("ref"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	schema, err := s.store.Get(r.Context(), ref)
	if errors.Is(err, storage.ErrNotFound) {
		http.Error(w, "schema not found", http.StatusNotFound)
		return
	}
	if err != nil {
		s.logger.Error("resolve failed", "ref", ref.String(), "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("ETag", `"`+schema.Fingerprint+`"`)
	h.Set("X-Schema-Fingerprint", schema.Fingerprint)
	h.Set("X-Schema-Source-Tag", schema.SourceTag)
	h.Set("X-Schema-Created-At", schema.CreatedAt.UTC().Format(time.RFC3339))
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(schema.Descriptor)
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
