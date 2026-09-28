package delivery

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	smtpserver "github.com/emersion/go-smtp"
)

// The real SMTP server implements the wire protocol, STARTTLS and SASL. This
// backend is only its terminal mailbox sink; no hand-written protocol double.
type mailboxSink struct {
	messages chan string
	reject   bool
}

func (b *mailboxSink) NewSession(*smtpserver.Conn) (smtpserver.Session, error) {
	return &mailSession{sink: b}, nil
}

type mailSession struct {
	sink          *mailboxSink
	authenticated bool
}

func (s *mailSession) Reset()        {}
func (s *mailSession) Logout() error { return nil }
func (s *mailSession) Mail(string, *smtpserver.MailOptions) error {
	if !s.authenticated {
		return errors.New("authentication required")
	}
	return nil
}
func (s *mailSession) Rcpt(string, *smtpserver.RcptOptions) error { return nil }
func (s *mailSession) Data(r io.Reader) error {
	b, e := io.ReadAll(r)
	if e != nil {
		return e
	}
	if s.sink.reject {
		return errors.New("synthetic refusal containing private content")
	}
	s.sink.messages <- string(b)
	return nil
}
func (s *mailSession) AuthMechanisms() []string { return []string{"PLAIN"} }
func (s *mailSession) Auth(string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(_, user, password string) error {
		if user != "sender" || password != "synthetic-test-only" {
			return errors.New("refused")
		}
		s.authenticated = true
		return nil
	}), nil
}

func smtpService(t *testing.T, mode string, reject bool) (*SMTP, <-chan string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	sink := &mailboxSink{messages: make(chan string, 8), reject: reject}
	srv := smtpserver.NewServer(sink)
	srv.Domain = "localhost"
	srv.TLSConfig = tlsConfig
	srv.MaxMessageBytes = 16384
	srv.ReadTimeout = 5 * time.Second
	srv.WriteTimeout = 5 * time.Second
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if mode == "tls" {
		listener = tls.NewListener(listener, tlsConfig)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close(); <-done })
	client, err := New(Config{Address: listener.Addr().String(), Mode: mode, From: "noreply@example.test", Username: "sender", Password: "synthetic-test-only"})
	if err != nil {
		t.Fatal(err)
	}
	client.tlsConfig.RootCAs = roots
	return client, sink.messages
}

func TestSMTPRealServiceTLSAuthenticationAndData(t *testing.T) {
	for _, mode := range []string{"starttls", "tls"} {
		t.Run(mode, func(t *testing.T) {
			client, messages := smtpService(t, mode, false)
			if err := client.Send(context.Background(), "recipient@example.test", "Verify mailbox", "https://example.test/verify#token=synthetic"); err != nil {
				t.Fatal(err)
			}
			select {
			case message := <-messages:
				if !strings.Contains(message, "#token=synthetic") || !strings.Contains(message, "From: noreply@example.test") {
					t.Fatal("message corrupted")
				}
			case <-time.After(time.Second):
				t.Fatal("SMTP accepted without delivery")
			}
		})
	}
}

func TestSMTPRefusesUntrustedTLSAuthenticationAndDataFailures(t *testing.T) {
	for _, failure := range []string{"tls", "auth", "data"} {
		t.Run(failure, func(t *testing.T) {
			client, _ := smtpService(t, "starttls", failure == "data")
			if failure == "tls" {
				client.tlsConfig.RootCAs = x509.NewCertPool()
			}
			if failure == "auth" {
				client.config.Password = "wrong"
			}
			if err := client.Send(context.Background(), "recipient@example.test", "Verify", "private content"); !errors.Is(err, ErrDelivery) || strings.Contains(err.Error(), "private content") {
				t.Fatal("failure not sanitized")
			}
		})
	}
}

func TestSMTPCancellationInterruptsStalledGreeting(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, e := l.Accept()
		if e == nil {
			defer c.Close()
			_, _ = io.Copy(io.Discard, c)
		}
	}()
	client, err := New(Config{Address: l.Addr().String(), Mode: "starttls", From: "noreply@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err = client.Send(ctx, "recipient@example.test", "Verify", "body"); !errors.Is(err, ErrDelivery) {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SMTP socket leaked after cancellation")
	}
}
