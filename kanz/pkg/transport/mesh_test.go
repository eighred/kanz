package transport

import (
	"context"
	"testing"
)

// A disabled Mesh is the dev path, and every composition root defers Close on it
// unconditionally — so the zero/disabled case must be inert rather than a panic
// waiting for the one service nobody runs locally.
func TestNewMesh_EmptySocketIsDisabledAndInert(t *testing.T) {
	m, err := NewMesh(context.Background(), "")
	if err != nil {
		t.Fatalf("empty socket must not error (it is the dev path): %v", err)
	}
	if m.Enabled() {
		t.Fatal("Enabled() = true for an empty socket")
	}
	if m.Source != nil {
		t.Fatal("Source must be nil when disabled")
	}
	// The load-bearing property: nil Client is what bus.NATSConfig.TLSConfig and
	// http.Transport.TLSClientConfig already read as "no TLS", so a caller needs
	// no branch. If this ever became a non-nil empty *tls.Config, every disabled
	// caller would attempt TLS with no certificate.
	if m.Client != nil {
		t.Fatal("Client must be nil when disabled — consumers read nil as 'no TLS'")
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close on a disabled Mesh: %v", err)
	}
}

func TestMesh_NilIsSafe(t *testing.T) {
	var m *Mesh
	if m.Enabled() {
		t.Fatal("nil Mesh must not report Enabled")
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close on a nil Mesh: %v", err)
	}
}

// A socket that no agent serves must FAIL, not hang — the bounded-wait rule
// NewSource exists to enforce (a pod that crash-loops with a reason beats a pod
// that hangs mute). NewMesh must propagate that rather than degrade to disabled,
// which would silently turn a SPIFFE-configured service into a plaintext one.
func TestNewMesh_UnreachableSocketErrorsRatherThanDisabling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // no agent here; don't wait 30s for the real timeout

	m, err := NewMesh(ctx, "unix:///nonexistent/kanz-mesh-test.sock")
	if err == nil {
		_ = m.Close()
		t.Fatal("an unreachable socket returned no error — a SPIFFE-configured service " +
			"would silently dial plaintext and be refused by the production broker")
	}
}
