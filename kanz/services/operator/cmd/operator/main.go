// operator binary entrypoint. Reads the Kubernetes node inventory via the
// in-cluster ServiceAccount (a get/list-nodes ClusterRole) and serves it over
// operator.v1 gRPC for the universe TUI. AddNode (node provisioning) is enabled
// only when OPERATOR_PROVISIONER_IMAGE is set; otherwise the server stays
// read-only and AddNode returns Unimplemented. SetVenueKeys/ListVenueKeys (S4a)
// are enabled only when OPERATOR_SECRET_BACKEND selects a backend ("kube" or
// "vault"); otherwise SetVenueKeys returns Unimplemented.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/internal/venueadapter/exchangeauth"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/operator/internal/config"
	"github.com/eighred/kanz/services/operator/internal/estate"
	"github.com/eighred/kanz/services/operator/internal/grpcsrv"
	"github.com/eighred/kanz/services/operator/internal/nodeops"
	"github.com/eighred/kanz/services/operator/internal/provision"
	"github.com/eighred/kanz/services/operator/internal/secrets"
	"github.com/eighred/kanz/services/operator/internal/venueproof"
)

func main() {
	// The lifecycle lives in run() because os.Exit skips defers: every defer
	// run() registers fires before this line. The non-zero code is what makes a
	// fatal halt distinguishable from a graceful SIGTERM — both otherwise exit 0
	// with reason "Completed" in the pod's termination record (#266).
	// 2 = startup failure, 1 = run loop died after startup, 0 = clean shutdown.
	os.Exit(run())
}

func run() int {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config load failed", "err", err)
		return 2
	}

	// In-cluster REST config: the pod's ServiceAccount token + the API server
	// CA, mounted by Kubernetes. The ClusterRole (Task 5) scopes it to
	// get/list nodes.
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		logger.Error("in-cluster config failed (operator runs as a pod)", "err", err)
		return 2
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		logger.Error("kubernetes client init failed", "err", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Records WHY we are stopping so the exit code can say so. Every site that
	// calls fatal.Raise below already brought the process down with stop(); the
	// only thing added is that the reason survives to the exit status (#266).
	fatal := lifecycle.NewFatal(stop)

	// Telemetry (#61). The operator ran with NO metrics surface at all: it is the
	// control plane the TUI drives — it provisions nodes and writes venue keys —
	// and nothing about its health was observable beyond a 200 from /healthz.
	// OTLPEndpoint is deliberately empty: spans are still created and trace
	// context still propagates, and startup never blocks on a collector being
	// reachable, which a control plane must not do.
	obs, err := observability.New(ctx, observability.Config{
		ServiceName:    "operator",
		ServiceVersion: version.String(),
		SampleRatio:    1,
	}, slog.NewJSONHandler(os.Stdout, nil))
	if err != nil {
		logger.Error("telemetry init failed", "err", err)
		return 2
	}
	defer func() {
		if err := obs.Shutdown(context.Background()); err != nil {
			logger.Error("telemetry shutdown failed", "err", err)
		}
	}()

	// Health server: liveness/readiness targets for the Deployment probes, and
	// the Prometheus surface. /metrics rides the HEALTH port, not the gRPC one —
	// a scrape against gRPC cannot succeed, and operator-deploy.yaml's
	// prometheus.io/port names 8091 for exactly this reason.
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.Handle("GET /metrics", obs.MetricsHandler())
	healthSrv := httpserver.New(cfg.HealthListen, mux, httpserver.Standard())
	go func() {
		logger.Info("operator health listening", "addr", cfg.HealthListen)
		if err := healthSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("health server failed", "err", err)
			fatal.Raise(err)
		}
	}()

	// gRPC server: mutually authenticated, restricted to the configured callers
	// (OPS-M2a). Built BEFORE the listener so a misconfigured control plane fails
	// without having bound the port — a socket that accepts and then refuses every
	// caller is harder to diagnose than one that was never opened.
	srvOpt, allowed, err := controlPlaneServerOption(ctx, cfg.SPIFFESocket, cfg.AllowedClients)
	if err != nil {
		logger.Error("control-plane access is not configured; refusing to serve", "err", err)
		return 2
	}
	lis, err := net.Listen("tcp", cfg.GRPCListen)
	if err != nil {
		logger.Error("grpc listen failed", "addr", cfg.GRPCListen, "err", err)
		return 2
	}
	grpcSrv := grpc.NewServer(srvOpt)
	logger.Info("control plane authenticated — mTLS, callers restricted",
		"authorized_clients", transport.ServiceIDStrings(allowed))
	reader := estate.NewK8s(cs)
	var srv *grpcsrv.Server
	if cfg.ProvisionerImage != "" {
		prov := provision.New(cs, provision.Config{
			Namespace:        namespaceOr("kanz-operator"),
			ProvisionerImage: cfg.ProvisionerImage,
			K3sServerURL:     cfg.K3sServerURL,
			K3sToken:         cfg.K3sToken,
			ImagePullPolicy:  cfg.ProvisionerImagePullPolicy,
		})
		srv = grpcsrv.NewWithProvisioner(reader, prov)
		logger.Info("node provisioning enabled", "image", cfg.ProvisionerImage)
	} else {
		srv = grpcsrv.New(reader)
		logger.Warn("no OPERATOR_PROVISIONER_IMAGE — AddNode disabled (read-only)")
	}
	srv = srv.WithNodeOps(nodeops.New(cs, logger))

	switch cfg.SecretBackend {
	case "kube":
		srv = srv.WithSecrets(secrets.NewKubeStore(cs, cfg.VenueSecretNamespace))
		logger.Info("venue-key backend: kubernetes secrets", "namespace", cfg.VenueSecretNamespace)
	case "vault":
		vs, verr := secrets.NewVaultStore(cfg.VaultAddr, cfg.VaultToken)
		if verr != nil {
			logger.Error("vault venue-key backend init failed", "err", verr)
			return 2
		}
		srv = srv.WithSecrets(vs)
		logger.Info("venue-key backend: vault", "addr", cfg.VaultAddr)
	default:
		logger.Warn("no OPERATOR_SECRET_BACKEND — SetVenueKeys disabled")
	}

	if cfg.VenueProof == "require" {
		srv = srv.WithVenueProof(venueproof.New(cfg.VenueBaseURLs, exchangeauth.OKXTradingMode(cfg.OKXTradingMode), execution.NewExchangeHTTPClient(5*time.Minute)))
		logger.Info("venue-key pre-write proof: require", "venues", provenVenues(cfg.VenueBaseURLs))
	} else {
		logger.Info("venue-key pre-write proof: off")
	}

	srv.Register(grpcSrv)
	go func() {
		logger.Info("operator gRPC listening", "addr", cfg.GRPCListen)
		if err := grpcSrv.Serve(lis); err != nil {
			logger.Error("grpc server failed", "err", err)
			fatal.Raise(err)
		}
	}()

	<-ctx.Done()
	logger.Info("operator shutting down")
	grpcSrv.GracefulStop()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = healthSrv.Shutdown(shutCtx)

	return fatal.Code()
}

// provenVenues returns the sorted venue ids proof is configured for — ids
// only, never the base URLs, so a log line can never carry anything that
// resembles a deployment secret.
func provenVenues(baseURLs map[string]string) []string {
	out := make([]string, 0, len(baseURLs))
	for v := range baseURLs {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// namespaceOr returns the pod's namespace (downward API file) or a default. The
// operator provisions in its own namespace.
func namespaceOr(def string) string {
	if b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(b)); ns != "" {
			return ns
		}
	}
	return def
}
