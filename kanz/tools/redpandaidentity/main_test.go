package main

import (
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"net/url"
	"testing"
)

func TestSubjectRequiresCanonicalCompleteIdentity(t *testing.T) {
	id := "spiffe://kanz.internal/ns/kanz-services/sa/proof-a"
	u, _ := url.Parse(id)
	h := sha256.Sum256([]byte(id))
	value := hex.EncodeToString(h[:16])
	cert := &x509.Certificate{URIs: []*url.URL{u}, Subject: pkix.Name{Names: []pkix.AttributeTypeAndValue{{Type: asn1.ObjectIdentifier{2, 5, 4, 45}, Value: value}}}}
	want := "2.5.4.45=#1320" + hex.EncodeToString([]byte(value))
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
		func(c *x509.Certificate) { c.Subject.Names[0].Type = asn1.ObjectIdentifier{2, 5, 4, 3} },
		func(c *x509.Certificate) { c.Subject.Names = append(c.Subject.Names, c.Subject.Names[0]) },
	} {
		clone := *cert
		clone.URIs = append([]*url.URL(nil), cert.URIs...)
		clone.Subject.Names = append([]pkix.AttributeTypeAndValue(nil), cert.Subject.Names...)
		mutate(&clone)
		if _, err := subject(&clone, id); err == nil {
			t.Fatal("accepted changed identity boundary")
		}
	}
}
