// Package testidentity mints ephemeral synthetic workload identities for tests.
// It never reads a workload socket, key file, external issuer or environment.
package testidentity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

type Authority struct {
	cert   *x509.Certificate
	key    *ecdsa.PrivateKey
	domain spiffeid.TrustDomain
}

func NewAuthority(domain spiffeid.TrustDomain) (*Authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "synthetic-kanz-test-authority"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, key.Public(), key)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &Authority{cert: parsed, key: key, domain: domain}, nil
}

// Source implements the existing transport.Source interface, including rotation
// of the leaf under one test authority. Returned credentials are synthetic.
type Source struct {
	svid   atomic.Pointer[x509svid.SVID]
	bundle *x509bundle.Bundle
	domain spiffeid.TrustDomain
}

func (a *Authority) Issue(id spiffeid.ID, notBefore, notAfter time.Time) (*Source, error) {
	if id.TrustDomain() != a.domain {
		return nil, errors.New("test identity domain differs")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	cert := &x509.Certificate{SerialNumber: serial, NotBefore: notBefore, NotAfter: notAfter,
		URIs: []*url.URL{id.URL()}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, cert, a.cert, key.Public(), a.key)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	s := &Source{bundle: x509bundle.FromX509Authorities(a.domain, []*x509.Certificate{a.cert}), domain: a.domain}
	s.svid.Store(&x509svid.SVID{ID: id, Certificates: []*x509.Certificate{parsed}, PrivateKey: key})
	return s, nil
}

func (s *Source) GetX509SVID() (*x509svid.SVID, error) {
	v := s.svid.Load()
	if v == nil {
		return nil, errors.New("synthetic identity unavailable")
	}
	return v, nil
}

func (s *Source) GetX509BundleForTrustDomain(td spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	if td != s.domain {
		return nil, errors.New("synthetic trust domain unavailable")
	}
	return s.bundle, nil
}

// ReplaceLeaf simulates workload rotation without modifying the trust bundle.
// A nil replacement simulates temporary identity unavailability.
func (s *Source) ReplaceLeaf(other *Source) {
	if other == nil {
		s.svid.Store(nil)
		return
	}
	s.svid.Store(other.svid.Load())
}
