package replay

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/segmentio/kafka-go/sasl/plain"
)

func TestReaderTransportPreservesAuthentication(t *testing.T) {
	config := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: "fixture.invalid"}
	sasl := plain.Mechanism{Username: "fixture", Password: "synthetic"}
	r, err := NewReader(Config{Brokers: []string{"127.0.0.1:1"}, Topic: "fixture", Range: Range{EndOffset: ptrInt64(1)}, TLSConfig: config, SASL: sasl, PartitionDialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.dialer.TLS != config || r.dialer.SASLMechanism != sasl || r.dialer.Timeout != time.Second {
		t.Fatal("reader authentication or timeout was lost")
	}
}

func TestReaderTLSFailureDoesNotFallbackOrHangClose(t *testing.T) {
	peer := httptest.NewTLSServer(http.NotFoundHandler())
	defer peer.Close()
	r, err := NewReader(Config{Brokers: []string{strings.TrimPrefix(peer.URL, "https://")}, Topic: "fixture",
		Range: Range{EndOffset: ptrInt64(1)}, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, ServerName: "untrusted.invalid"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = r.Next(ctx)
	var certificate *tls.CertificateVerificationError
	if !errors.As(err, &certificate) {
		t.Fatalf("expected actual certificate refusal, got %T", err)
	}
	requireClosed(t, r)
}
