// This local proof input is a public CA for the canonical bootstrap's in-pod
// Kubernetes CA path. It is independent of the later owned TokenReview server.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"time"
)

func publicFixtureCA() error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "runtime bootstrap public dummy CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	if err := certificate.CheckSignatureFrom(certificate); err != nil {
		return err
	}
	// The signing key stays in memory: only the public certificate is encoded.
	encoded := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if len(encoded) == 0 || len(encoded) > 16<<10 {
		return fmt.Errorf("public fixture certificate exceeded its bound")
	}
	_, err = os.Stdout.Write(encoded)
	return err
}

func main() {
	if publicFixtureCA() != nil {
		_, _ = fmt.Fprintln(os.Stderr, "public fixture CA generation failed")
		os.Exit(1)
	}
}
