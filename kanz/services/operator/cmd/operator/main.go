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

	"github.com/kanz-eng/kanz/internal/execution"
	"github.com/kanz-eng/kanz/services/operator/internal/config"
	"github.com/kanz-eng/kanz/services/operator/internal/estate"
	"github.com/kanz-eng/kanz/services/operator/internal/grpcsrv"
	"github.com/kanz-eng/kanz/services/operator/internal/nodeops"
	"github.com/kanz-eng/kanz/services/operator/internal/provision"
	"github.com/kanz-eng/kanz/services/operator/internal/secrets"
	"github.com/kanz-eng/kanz/services/operator/internal/venueproof"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config load failed", "err", err)
		os.Exit(2)
	}

	// In-cluster REST config: the pod's ServiceAccount token + the API server
	// CA, mounted by Kubernetes. The ClusterRole (Task 5) scopes it to
	// get/list nodes.
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		logger.Error("in-cluster config failed (operator runs as a pod)", "err", err)
		os.Exit(2)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		logger.Error("kubernetes client init failed", "err", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Health server: liveness/readiness targets for the Deployment probes.
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	healthSrv := &http.Server{Addr: cfg.HealthListen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		logger.Info("operator health listening", "addr", cfg.HealthListen)
		if err := healthSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("health server failed", "err", err)
			stop()
		}
	}()

	// gRPC server: mutually authenticated, restricted to the configured callers
	// (OPS-M2a). Built BEFORE the listener so a misconfigured control plane fails
	// without having bound the port — a socket that accepts and then refuses every
	// caller is harder to diagnose than one that was never opened.
	srvOpt, allowed, err := controlPlaneServerOption(ctx, cfg.SPIFFESocket, cfg.AllowedClients)
	if err != nil {
		logger.Error("control-plane access is not configured; refusing to serve", "err", err)
		os.Exit(2)
	}
	lis, err := net.Listen("tcp", cfg.GRPCListen)
	if err != nil {
		logger.Error("grpc listen failed", "addr", cfg.GRPCListen, "err", err)
		os.Exit(2)
	}
	grpcSrv := grpc.NewServer(srvOpt)
	logger.Info("control plane authenticated — mTLS, callers restricted",
		"authorized_clients", spiffeIDStrings(allowed))
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
			os.Exit(2)
		}
		srv = srv.WithSecrets(vs)
		logger.Info("venue-key backend: vault", "addr", cfg.VaultAddr)
	default:
		logger.Warn("no OPERATOR_SECRET_BACKEND — SetVenueKeys disabled")
	}

	if cfg.VenueProof == "require" {
		srv = srv.WithVenueProof(venueproof.New(cfg.VenueBaseURLs, execution.NewExchangeHTTPClient(5*time.Minute)))
		logger.Info("venue-key pre-write proof: require", "venues", provenVenues(cfg.VenueBaseURLs))
	} else {
		logger.Info("venue-key pre-write proof: off")
	}

	srv.Register(grpcSrv)
	go func() {
		logger.Info("operator gRPC listening", "addr", cfg.GRPCListen)
		if err := grpcSrv.Serve(lis); err != nil {
			logger.Error("grpc server failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("operator shutting down")
	grpcSrv.GracefulStop()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = healthSrv.Shutdown(shutCtx)
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
