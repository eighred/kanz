package transport

import (
	"context"
	"crypto/tls"
	"io"
)

// Mesh is a process's workload identity plus the mTLS materials derived from it,
// built once at the composition root and shared by everything in the process that
// speaks mTLS — the NATS spine, a gRPC listener, an HTTP client.
//
// # Why this exists (SEC-M3)
//
// infra/nats/nats.yaml requires a client SVID (`tls { verify: true,
// verify_and_map: true }`). Every bus.DialNATS call site in this repository
// passed {URL, Name} and left TLSConfig nil — a PLAINTEXT client the production
// broker refuses at the handshake — so the platform could not connect to its own
// spine as deployed. Wiring that correctly is the same eleven lines in every
// composition root (socket ⇒ Source ⇒ ClientTLSConfig, plus the nil-handling and
// the Close), and thirteen copies of eleven lines is thirteen chances to get the
// nil case subtly wrong. So it is promoted here, on the second-consumer rule.
//
// # It lives in transport, not bus, on purpose
//
// bus is the GENERIC transport and must not depend on the identity layer — the
// same no-inversion rule that kept quality flags out of bus.Producer. bus takes a
// *tls.Config, which is stdlib; this package knows what SPIFFE is. Handing
// Mesh.Client to bus.NATSConfig.TLSConfig keeps the dependency pointing one way.
//
// # The zero value is DISABLED, and that is not a security hole
//
// An empty socket yields a disabled Mesh whose Client is nil, which DialNATS
// reads as plaintext. That is correct for dev (dev/docker-compose.yml runs a
// plaintext broker) and it is NOT a silent fallback in production: the real
// broker refuses a plaintext client at the handshake, at startup, loudly. The
// enforcement lives at the thing being protected rather than in a flag every
// caller must remember — so this deliberately has no AllowPlaintext knob, which
// would only duplicate an enforcement that already works.
type Mesh struct {
	// Source is the workload identity: nil when disabled. Pass it to
	// ServerTLSConfig/ServerOption for a listener that requires a peer SVID.
	Source Source
	// Client is the mTLS client config: nil when disabled, which every consumer
	// (bus.NATSConfig.TLSConfig, http.Transport.TLSClientConfig) already reads
	// as "no TLS". A nil *tls.Config is the disabled case expressed in the type
	// the consumers already take, so callers need no branch.
	Client *tls.Config

	closer io.Closer
}

// NewMesh builds the process identity from a SPIFFE workload-API socket (the
// SEC-01a CSI mount, e.g. "unix:///run/spiffe/spire-agent.sock").
//
// An empty socket returns a DISABLED Mesh and no error — the dev/local path. A
// non-empty socket that cannot be reached is a real error: NewSource bounds the
// initial fetch and names the socket, so a missing SPIRE agent crash-loops the
// pod visibly instead of hanging it mute.
//
// The caller owns Close.
func NewMesh(ctx context.Context, socket string) (*Mesh, error) {
	if socket == "" {
		return &Mesh{}, nil
	}
	src, err := NewSource(ctx, socket)
	if err != nil {
		return nil, err
	}
	return &Mesh{
		Source: src,
		Client: ClientTLSConfig(src, AuthorizeMesh()),
		closer: src,
	}, nil
}

// Enabled reports whether this process has a workload identity. Nil-safe.
func (m *Mesh) Enabled() bool { return m != nil && m.Source != nil }

// Close releases the underlying X509Source (which runs a rotation watcher).
// Nil-safe and safe on a disabled Mesh, so a composition root can defer it
// unconditionally.
func (m *Mesh) Close() error {
	if m == nil || m.closer == nil {
		return nil
	}
	return m.closer.Close()
}
