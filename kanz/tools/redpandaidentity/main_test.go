package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/segmentio/kafka-go"
	"io"
	"net/url"
	"testing"
)

func TestSubjectRequiresCanonicalCompleteIdentity(t *testing.T) {
	id := "spiffe://kanz.internal/ns/kanz-services/sa/proof-a"
	u, _ := url.Parse(id)
	h := sha256.Sum256([]byte(id))
	value := hex.EncodeToString(h[:16])
	// Literal pinned SPIRE shape: default C/O, first DNS as CN, composer UID.
	cert := &x509.Certificate{URIs: []*url.URL{u}, DNSNames: []string{"client-a-abcde"}, Subject: pkix.Name{
		Country: []string{"US"}, Organization: []string{"SPIRE"}, CommonName: "client-a-abcde",
		Names: []pkix.AttributeTypeAndValue{
			{Type: asn1.ObjectIdentifier{2, 5, 4, 6}, Value: "US"},
			{Type: asn1.ObjectIdentifier{2, 5, 4, 10}, Value: "SPIRE"},
			{Type: asn1.ObjectIdentifier{2, 5, 4, 3}, Value: "client-a-abcde"},
			{Type: asn1.ObjectIdentifier{2, 5, 4, 45}, Value: value},
		}}}
	want := "CN=client-a-abcde,O=SPIRE,C=US,2.5.4.45=#1320" + hex.EncodeToString([]byte(value))
	if got, err := subject(cert, id); err != nil || got != want {
		t.Fatalf("DN=%q error=%v", got, err)
	}
	for _, mutate := range []func(*x509.Certificate){
		func(c *x509.Certificate) { c.URIs = nil },
		func(c *x509.Certificate) { c.URIs = append(c.URIs, u) },
		func(c *x509.Certificate) {
			v, _ := url.Parse("spiffe://other/ns/kanz-services/sa/proof-a")
			c.URIs = []*url.URL{v}
		},
		func(c *x509.Certificate) { c.Subject.Names[0].Value = "wrong" },
		func(c *x509.Certificate) { c.Subject.Names[3].Value = "wrong-hash" },
		func(c *x509.Certificate) { c.Subject.Names[0] = c.Subject.Names[3] },
		func(c *x509.Certificate) { c.Subject.Names[0].Type = asn1.ObjectIdentifier{2, 5, 4, 11} },
		func(c *x509.Certificate) { c.Subject.Names = append(c.Subject.Names, c.Subject.Names[0]) },
		func(c *x509.Certificate) { c.DNSNames = nil },
		func(c *x509.Certificate) { c.DNSNames = []string{"other"} },
		func(c *x509.Certificate) { c.Subject.CommonName = "other" },
		func(c *x509.Certificate) {
			c.Subject.CommonName = "bad,CN=injected"
			c.DNSNames = []string{c.Subject.CommonName}
			c.Subject.Names[2].Value = c.Subject.CommonName
		},
	} {
		clone := *cert
		clone.URIs = append([]*url.URL(nil), cert.URIs...)
		clone.DNSNames = append([]string(nil), cert.DNSNames...)
		clone.Subject.Names = append([]pkix.AttributeTypeAndValue(nil), cert.Subject.Names...)
		mutate(&clone)
		if _, err := subject(&clone, id); err == nil {
			t.Fatal("accepted changed identity boundary")
		}
	}
}

func TestFailureClassDoesNotExposeErrorText(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("sensitive: %w", kafka.ClusterAuthorizationFailed), "kafka-31"},
		{&tls.CertificateVerificationError{Err: errors.New("sensitive certificate")}, "tls-certificate"},
		{context.DeadlineExceeded, "timeout"},
		{io.EOF, "eof"},
		{errors.New("private arbitrary error"), "other"},
	} {
		if got := failureClass(tc.err); got != tc.want {
			t.Fatalf("classification=%q want=%q", got, tc.want)
		}
	}
}
