package signingkey

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func writePEM(t *testing.T, name string, block *pem.Block) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func sec1(t *testing.T, curve elliptic.Curve) *pem.Block {
	t.Helper()
	k, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return &pem.Block{Type: "EC PRIVATE KEY", Bytes: der}
}

func TestLoadsBothPEMEncodings(t *testing.T) {
	// SEC1, what `openssl ecparam -genkey` writes.
	if _, err := Load(writePEM(t, "sec1.pem", sec1(t, elliptic.P256())), false, quiet()); err != nil {
		t.Fatalf("SEC1 key refused: %v", err)
	}

	// PKCS#8, what `openssl pkcs8 -topk8` writes. Refusing either encoding makes
	// a correctly generated key look like a corrupt one.
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(writePEM(t, "pkcs8.pem", &pem.Block{Type: "PRIVATE KEY", Bytes: der}), false, quiet()); err != nil {
		t.Fatalf("PKCS#8 key refused: %v", err)
	}
}

// ES256 IS DEFINED OVER P-256 ONLY (RFC 7518 §3.4). A P-384 key signs happily
// while the token still says alg=ES256, so every verifier rejects it — an
// outage that reads as a key-distribution problem rather than a curve mismatch.
func TestAKeyOnTheWrongCurveIsRefused(t *testing.T) {
	_, err := Load(writePEM(t, "p384.pem", sec1(t, elliptic.P384())), false, quiet())
	if err == nil {
		t.Fatal("a P-384 key was accepted for ES256 signing")
	}
	if !strings.Contains(err.Error(), "P-256") {
		t.Fatalf("error = %v, want it to name the required curve", err)
	}
}

// NO KEY AND NO OPT-IN MUST REFUSE TO START. Generating one silently would work
// on a laptop and then invalidate every session on every restart in a
// deployment, surfacing as intermittent 401s rather than as a missing key.
func TestNoKeyWithoutTheOptInRefusesToStart(t *testing.T) {
	_, err := Load("", false, quiet())
	if err == nil {
		t.Fatal("no signing key configured, and the service started anyway")
	}
	for _, want := range []string{"IDENTITY_SIGNING_KEY_FILE", "IDENTITY_ALLOW_EPHEMERAL_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s — an operator cannot act on it: %v", want, err)
		}
	}
}

func TestTheEphemeralOptInWorksAndIsAnnounced(t *testing.T) {
	var sb strings.Builder
	logger := slog.New(slog.NewTextHandler(&sb, nil))

	k, err := Load("", true, logger)
	if err != nil {
		t.Fatalf("ephemeral opt-in refused: %v", err)
	}
	if k == nil || k.Curve != elliptic.P256() {
		t.Fatal("the generated key is not P-256")
	}
	if !strings.Contains(sb.String(), "EPHEMERAL") {
		t.Fatalf("nothing was logged about the ephemeral key.\n\n"+
			"A throwaway signing key that is not announced is indistinguishable from a "+
			"durable one until a restart drops every session. Log was: %q", sb.String())
	}
}

func TestAMissingOrGarbageFileIsRefused(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.pem"), false, quiet()); err == nil {
		t.Fatal("a missing key file was accepted")
	}
	p := filepath.Join(t.TempDir(), "junk.pem")
	if err := os.WriteFile(p, []byte("this is not a PEM file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p, false, quiet()); err == nil {
		t.Fatal("a non-PEM file was accepted as a signing key")
	}
}
