package scaffold

// The generated skeleton is a real, buildable HTTP service: config + probes +
// metrics + graceful shutdown, the shape every Kanz service shares. A new
// service fills in its own handlers/consumers on top. Bus/DB wiring is left out
// deliberately — it is service-specific, and a scaffold that guesses wrong is
// worse than a clean base (see services/audit or services/lineage for the
// bus-consumer pattern to copy in).

const mainTmpl = `// {{.Name}} binary entrypoint. TODO: describe what this service does.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/services/{{.Name}}/internal/config"
	"github.com/eighred/kanz/services/{{.Name}}/internal/server"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Default().Error("config load failed", "err", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})
	obs, err := observability.New(ctx, observability.Config{
		ServiceName:    cfg.Source,
		ServiceVersion: version(),
		OTLPEndpoint:   cfg.OTLPEndpoint,
		SampleRatio:    1,
	}, base)
	if err != nil {
		slog.Default().Error("observability init failed", "err", err)
		os.Exit(2)
	}
	logger := obs.Logger
	slog.SetDefault(logger)
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = obs.Shutdown(shutCtx)
	}()

	readiness := &server.Readiness{}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(readiness, logger, server.WithMetrics(obs.MetricsHandler())),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("{{.Name}} listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			stop()
		}
	}()

	// TODO: start the service's real work here (a bus consumer, a worker pool).
	readiness.Set(true)
	<-ctx.Done()
	readiness.Set(false)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}
}

// version is the service version stamped on telemetry. Hardcoded until the build
// injects a git SHA / semver (CICD-01a/c ldflags).
func version() string { return "dev" }
`

const configTmpl = `package config

import (
	"log/slog"
	"os"
	"strings"
)

// Config is the {{.Name}} runtime configuration, sourced from the environment so
// it composes with the CSI/Vault secret mounts (SEC-01d).
type Config struct {
	Listen   string
	LogLevel slog.Level
	// Source is the service identity stamped on telemetry.
	Source string
	// OTLPEndpoint is the OTel collector for span export (OBS-01).
	OTLPEndpoint string
}

func Load() (Config, error) {
	return Config{
		Listen:       envOr("{{.EnvPrefix}}_LISTEN", ":8090"),
		LogLevel:     parseLevel(envOr("{{.EnvPrefix}}_LOG_LEVEL", "info")),
		Source:       envOr("{{.EnvPrefix}}_SOURCE", "{{.Name}}"),
		OTLPEndpoint: os.Getenv("{{.EnvPrefix}}_OTLP_ENDPOINT"),
	}, nil
}

func envOr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
`

const serverTmpl = `package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
)

// Readiness gates traffic: starts NOT ready, flips once the service is wired;
// shutdown clears it.
type Readiness struct{ ready atomic.Bool }

func (r *Readiness) Set(ready bool) { r.ready.Store(ready) }
func (r *Readiness) Ready() bool    { return r.ready.Load() }

type Server struct {
	readiness *Readiness
	metrics   http.Handler
	mux       *http.ServeMux
}

type Option func(*Server)

// WithMetrics mounts a Prometheus /metrics handler (OBS-01a).
func WithMetrics(h http.Handler) Option { return func(s *Server) { s.metrics = h } }

func New(readiness *Readiness, _ *slog.Logger, opts ...Option) *Server {
	s := &Server{readiness: readiness, mux: http.NewServeMux()}
	for _, opt := range opts {
		opt(s)
	}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	if s.metrics != nil {
		s.mux.Handle("GET /metrics", s.metrics)
	}
	// TODO: register the service's API routes here.
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if !s.readiness.Ready() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
`

const readmeTmpl = `# {{.Name}} service

TODO: describe what {{.Name}} does.

Scaffolded by ` + "`tools/scaffold`" + ` (DEVX-01b) following the EVT-16a layout:
` + "`cmd/{{.Name}}/`" + ` (entrypoint) + ` + "`internal/`" + ` (per-service packages).

## Run

` + "```sh" + `
go run ./services/{{.Name}}/cmd/{{.Name}}
` + "```" + `

Config (env): ` + "`{{.EnvPrefix}}_LISTEN`" + ` (` + "`:8090`" + `),
` + "`{{.EnvPrefix}}_LOG_LEVEL`" + `, ` + "`{{.EnvPrefix}}_SOURCE`" + `,
` + "`{{.EnvPrefix}}_OTLP_ENDPOINT`" + `.

## Next steps

- Add the service's real work in ` + "`cmd/{{.Name}}/main.go`" + ` (a bus consumer,
  a worker pool — see ` + "`services/audit`" + ` or ` + "`services/lineage`" + ` for the
  bus-consumer pattern).
- Register API routes in ` + "`internal/server`" + `.
- Add a Dockerfile (copy ` + "`services/risk-engine/Dockerfile`" + `) and wire the
  service into ` + "`infra/deploy`" + ` + the ` + "`infra/gitops`" + ` ApplicationSet.
`
