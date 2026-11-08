package main

import (
	"context"
	"errors"
	"testing"

	"github.com/eighred/kanz/cmd/kanz-kafka-provision/internal/provision"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

type changingSource struct {
	id  spiffeid.ID
	err error
}

func (s *changingSource) GetX509SVID() (*x509svid.SVID, error) {
	return &x509svid.SVID{ID: s.id}, s.err
}
func (*changingSource) GetX509BundleForTrustDomain(spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	return nil, errors.New("not a TLS fixture")
}

func TestProvisionerIdentityIsCheckedAfterRotation(t *testing.T) {
	s := &changingSource{id: spiffeid.RequireFromString("spiffe://kanz.internal/ns/kanz-messaging/sa/kafka-provisioner")}
	p := provisionerSource{Source: s}
	if _, err := p.GetX509SVID(); err != nil {
		t.Fatal("expected provisioner refused")
	}
	s.id = spiffeid.RequireFromString("spiffe://kanz.internal/ns/kanz-services/sa/archiver-acme")
	if _, err := p.GetX509SVID(); err == nil {
		t.Fatal("changed identity accepted")
	}
	s.id = spiffeid.RequireFromString("spiffe://kanz.internal/ns/kanz-messaging/sa/kafka-provisioner")
	s.err = errors.New("unavailable")
	if _, err := p.GetX509SVID(); err == nil {
		t.Fatal("unavailable identity accepted")
	}
}

func TestInvalidCommandRefusesBeforeWorkloadAPIAccess(t *testing.T) {
	for _, args := range [][]string{{}, {"-mode=offboard"}, {"-mode=apply", "-timeout=241s"}, {"-broker=http://wrong:9094"}, {"-unknown=x"}} {
		_, err := run(context.Background(), args)
		if !errors.Is(err, provision.ErrInput) {
			t.Fatal("invalid command did not fail closed")
		}
	}
}

func TestSameDomainNonBrokerPeerIsNotAnAuthorizedBroker(t *testing.T) {
	for _, peer := range []string{"spiffe://kanz.internal/ns/kanz-messaging/sa/kafka", "spiffe://kanz.internal/ns/kanz-messaging/sa/redpanda"} {
		id, err := brokerIdentity(peer)
		if err != nil || id.String() != peer {
			t.Fatal("canonical broker refused")
		}
	}
	for _, peer := range []string{"spiffe://kanz.internal/ns/kanz-services/sa/oms", "spiffe://kanz.internal/ns/kanz-messaging/sa/kafka-provisioner", "spiffe://other.internal/ns/kanz-messaging/sa/kafka", ""} {
		if _, err := brokerIdentity(peer); !errors.Is(err, provision.ErrInput) {
			t.Fatal("nonbroker peer accepted")
		}
	}
}
