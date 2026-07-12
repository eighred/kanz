// Package transport turns SPIRE-issued workload identities (SEC-01a) into
// mutual-TLS configuration for the Kanz mesh. It is the single seam every
// gRPC client/server and any future TLS transport uses to present its own
// X509-SVID and verify + authorize the peer's — so identity policy lives in
// one place rather than being re-derived at each call site (the PRED-06/07
// inference path, the API gateway, etc.).
//
// # How identity flows
//
// The SPIRE agent (SEC-01a) writes a rotating X509-SVID to the Workload API
// socket the SPIFFE CSI driver mounts into each pod. NewSource keeps a live,
// auto-rotating handle to it; go-spiffe re-fetches at ~half the 1h SVID TTL,
// so a long-lived server or client transparently swaps in fresh certs with no
// reload code here. The same Source supplies both the local SVID (what we
// present) and the trust bundle (how we verify peers).
//
// # Authorization, deny-by-default
//
// Authentication (valid SVID, chains to the trust bundle) is necessary but not
// sufficient — a peer must also be *authorized*. AuthorizeMesh accepts any
// workload in the kanz.internal trust domain (the intra-mesh default);
// AuthorizeServices narrows to an explicit allow-list of SPIFFE IDs for a
// server that should only ever talk to a known, small set of callers. There is
// no "authorize any trust domain" helper on purpose — that would defeat the
// point of a private trust domain.
//
// # Why go-spiffe rather than hand-rolled TLS
//
// SVID verification is security-critical and subtle (URI-SAN SPIFFE ID
// extraction, bundle rotation, the InsecureSkipVerify-plus-VerifyPeerCertificate
// dance clients must do to skip hostname checks while still verifying the
// chain). go-spiffe is the canonical implementation that matches the SPIRE
// control plane we deploy; reimplementing it would be more code and a bigger
// attack surface for no benefit. It is a focused security dependency, not bloat.
package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// TrustDomain is the Kanz SPIFFE trust domain (SEC-01a). Every workload
// identity is spiffe://kanz.internal/ns/<namespace>/sa/<service-account>.
const TrustDomain = "kanz.internal"

// kanzTrustDomain is the parsed TrustDomain, computed once. The const is fixed
// and valid, so a parse failure here is a programming error, not a runtime one.
var kanzTrustDomain = mustTrustDomain(TrustDomain)

func mustTrustDomain(s string) spiffeid.TrustDomain {
	td, err := spiffeid.TrustDomainFromString(s)
	if err != nil {
		panic("transport: invalid trust domain " + s + ": " + err.Error())
	}
	return td
}

// Source supplies the workload's own X509-SVID and the trust bundle used to
// verify peers. *workloadapi.X509Source satisfies it (auto-rotating); tests
// inject a static implementation. It is the intersection of go-spiffe's
// x509svid.Source and x509bundle.Source so the mTLS helpers take a single arg.
type Source interface {
	GetX509SVID() (*x509svid.SVID, error)
	GetX509BundleForTrustDomain(td spiffeid.TrustDomain) (*x509bundle.Bundle, error)
}

// InitialSVIDTimeout bounds the wait for the FIRST SVID. It does not bound
// rotation: go-spiffe's watcher runs on its own context.Background()-derived
// context, so the deadline here applies only to the initial fetch.
const InitialSVIDTimeout = 30 * time.Second

// NewSource connects to the SPIFFE Workload API and returns an auto-rotating
// X509 source. It blocks until the first SVID arrives, so a caller that gets a
// Source already holds a usable identity. socket is the agent socket address
// (the SEC-01a CSI mount, e.g. "unix:///run/spiffe/spire-agent.sock"); empty ⇒
// go-spiffe reads the SPIFFE_ENDPOINT_SOCKET env var. The caller must Close the
// returned source on shutdown.
//
// # It waits, but it does not wait forever
//
// This used to block on the CALLER'S context — which for every service on this
// platform is the root context, cancelled only by SIGTERM. So a workload whose
// SPIRE agent was not there simply STOPPED, silently, inside its own startup:
//
//	no error. no log line. no crash. no restart. Running, 0/1 Ready, forever.
//
// Found by deploying the OMS to a cluster with no SPIRE agent (EXEC-M10): the pod
// emitted exactly one line — "oms listening" — and then nothing, for as long as it
// was left alone. Nothing in the platform could tell an operator why, because the
// process never got far enough to say anything. And this is NOT an exotic state: a
// SPIRE agent DaemonSet that has not scheduled yet, an agent that crashed, a socket
// that failed to mount — all produce it, on every SPIFFE-enabled service.
//
// So the initial fetch is bounded. On timeout the caller gets a real error naming
// the socket, the service exits loudly, and the kubelet restarts it with backoff —
// which is the correct shape for "a dependency is not up yet": visible, diagnosable,
// and self-healing once SPIRE arrives. A pod that crash-loops with a clear reason is
// strictly better than a pod that hangs with none.
func NewSource(ctx context.Context, socket string) (*workloadapi.X509Source, error) {
	var opts []workloadapi.X509SourceOption
	if socket != "" {
		opts = append(opts, workloadapi.WithClientOptions(workloadapi.WithAddr(socket)))
	}
	// WithTimeout respects an earlier parent deadline, so a caller that wants a
	// shorter wait still gets it.
	ctx, cancel := context.WithTimeout(ctx, InitialSVIDTimeout)
	defer cancel()

	src, err := workloadapi.NewX509Source(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("transport: no SVID from the SPIFFE workload API at %s within %s "+
			"(is the SPIRE agent running, and is its socket mounted into this pod?): %w",
			socketDesc(socket), InitialSVIDTimeout, err)
	}
	return src, nil
}

// socketDesc names the socket in an error, whether it was configured explicitly or
// left to go-spiffe's env lookup.
func socketDesc(socket string) string {
	if socket == "" {
		return "$SPIFFE_ENDPOINT_SOCKET"
	}
	return socket
}

// ServiceID builds the SPIFFE ID for a Kanz workload — the identity the SEC-01a
// ClusterSPIFFEID issues and that AuthorizeServices allow-lists. Mirrors the
// registration template spiffe://kanz.internal/ns/<ns>/sa/<sa>.
func ServiceID(namespace, serviceAccount string) (spiffeid.ID, error) {
	if namespace == "" || serviceAccount == "" {
		return spiffeid.ID{}, errors.New("transport: namespace and serviceAccount are required")
	}
	return spiffeid.FromSegments(kanzTrustDomain, "ns", namespace, "sa", serviceAccount)
}

// AuthorizeMesh accepts any peer presenting a valid SVID in the Kanz trust
// domain — the default for intra-mesh mTLS where every workload is trusted to
// connect (finer-grained access control is AUTH-01's job, at the application
// layer).
func AuthorizeMesh() tlsconfig.Authorizer {
	return tlsconfig.AuthorizeMemberOf(kanzTrustDomain)
}

// AuthorizeServices accepts only peers whose SPIFFE ID is in the allow-list —
// deny-by-default narrowing for a server that should only ever be called by a
// known set of workloads (e.g. the API gateway accepts only the risk-engine).
func AuthorizeServices(ids ...spiffeid.ID) tlsconfig.Authorizer {
	return tlsconfig.AuthorizeOneOf(ids...)
}

// ServerTLSConfig returns a *tls.Config that presents this workload's SVID and
// requires + verifies + authorizes the client's SVID (mutual TLS). The same
// Source provides both the served cert and the verification bundle.
func ServerTLSConfig(src Source, authz tlsconfig.Authorizer) *tls.Config {
	return tlsconfig.MTLSServerConfig(src, src, authz)
}

// ClientTLSConfig returns a *tls.Config that presents this workload's SVID and
// verifies + authorizes the server's SVID. (go-spiffe sets InsecureSkipVerify
// and performs SPIFFE verification in VerifyPeerCertificate — it skips DNS
// hostname checks, not certificate verification.)
func ClientTLSConfig(src Source, authz tlsconfig.Authorizer) *tls.Config {
	return tlsconfig.MTLSClientConfig(src, src, authz)
}

// ServerCredentials wraps ServerTLSConfig as gRPC transport credentials.
func ServerCredentials(src Source, authz tlsconfig.Authorizer) credentials.TransportCredentials {
	return credentials.NewTLS(ServerTLSConfig(src, authz))
}

// ClientCredentials wraps ClientTLSConfig as gRPC transport credentials.
func ClientCredentials(src Source, authz tlsconfig.Authorizer) credentials.TransportCredentials {
	return credentials.NewTLS(ClientTLSConfig(src, authz))
}

// ServerOption is the grpc.ServerOption form — pass to grpc.NewServer so the
// server speaks mTLS. Convenience over ServerCredentials at the call site.
func ServerOption(src Source, authz tlsconfig.Authorizer) grpc.ServerOption {
	return grpc.Creds(ServerCredentials(src, authz))
}

// ClientDialOption is the grpc.DialOption form — pass to grpc.NewClient so the
// dial speaks mTLS. This is the drop-in replacement for the insecure
// credentials the PRED-07 sync client uses today.
func ClientDialOption(src Source, authz tlsconfig.Authorizer) grpc.DialOption {
	return grpc.WithTransportCredentials(ClientCredentials(src, authz))
}
