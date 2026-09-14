package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/requestbody"
	"github.com/eighred/kanz/services/web-bff/internal/session"
)

type poisonReadCloser struct{ t *testing.T }

func (p poisonReadCloser) Read([]byte) (int, error) {
	p.t.Fatal("BFF read a body whose Content-Length already exceeded the public ceiling")
	return 0, io.EOF
}

func (poisonReadCloser) Close() error { return nil }

type countingReadCloser struct {
	reader io.Reader
	read   int
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read += n
	return n, err
}

func (*countingReadCloser) Close() error { return nil }

func TestProxyRejectsDeclaredOversizeBodyWithoutReadingOrForwarding(t *testing.T) {
	forwarded := 0
	gw := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		forwarded++
	}))
	t.Cleanup(gw.Close)
	srv := bffWithSigning(t, gw.URL, "s3cr3t")

	r := httptest.NewRequest(http.MethodPost, "/api/v1/orders", nil)
	r.Body = poisonReadCloser{t: t}
	r.ContentLength = requestbody.MaxBytes + 1
	rec := proxyRequestAs(t, srv, r)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if forwarded != 0 {
		t.Fatalf("gateway received %d requests, want none", forwarded)
	}
}

func TestProxyBoundsUnknownLengthBodyAndReturns413(t *testing.T) {
	forwarded := 0
	gw := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		forwarded++
	}))
	t.Cleanup(gw.Close)
	srv := bffWithSigning(t, gw.URL, "s3cr3t")

	stream := &countingReadCloser{reader: bytes.NewReader(make([]byte, requestbody.MaxBytes*4))}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/orders", nil)
	r.Body = stream
	r.ContentLength = -1
	rec := proxyRequestAs(t, srv, r)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if forwarded != 0 {
		t.Fatalf("gateway received %d requests, want none", forwarded)
	}
	if stream.read > requestbody.MaxBytes+1 {
		t.Fatalf("BFF read %d bytes, want at most %d", stream.read, requestbody.MaxBytes+1)
	}
}

func TestProxyForwardsAndSignsBodyExactlyAtTheLimit(t *testing.T) {
	gw := &signingGateway{secret: []byte("s3cr3t")}
	gwSrv := gw.start(t)
	srv := bffWithSigning(t, gwSrv.URL, "s3cr3t")

	payload := bytes.Repeat([]byte("x"), requestbody.MaxBytes)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/orders", bytes.NewReader(payload))
	rec := proxyRequestAs(t, srv, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if !gw.ok {
		t.Fatal("gateway did not accept the signature for an exact-limit body")
	}
	if gw.sawBody != len(payload) {
		t.Fatalf("gateway received %d bytes, want the exact-limit payload of %d", gw.sawBody, len(payload))
	}
}

func proxyRequestAs(t *testing.T, srv *Server, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	id, err := srv.sessions.Create(session.Session{
		AccessToken: "the-server-held-token",
		Subject:     "user:fiona",
		Tenant:      "acme",
		Expiry:      time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("session create: %v", err)
	}
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: id})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)
	return rec
}
