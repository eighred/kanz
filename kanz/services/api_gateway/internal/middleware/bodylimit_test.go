package middleware

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/gatewaysig"
)

// A BODY IS BOUNDED BEFORE ANYTHING AUTHENTICATES THE CALLER (#887).
//
// Signing buffers the whole body and hashes it, and it runs before Auth. With no
// bound an unauthenticated caller chose how much of the gateway's heap it
// allocated per request — on the sole entry point for POST /v1/orders, whose
// failure mode is an OOM kill reported by nothing until the pod dies.

// ranHandler records whether the handler behind the middleware was reached. The
// issue asks specifically for this: a refusal that still ran the handler would
// have allocated and hashed the body anyway.
type ranHandler struct{ ran bool }

func (h *ranHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.ran = true
	w.WriteHeader(http.StatusOK)
}

func TestAnOversizedBodyIsRefusedWithoutReachingTheHandler(t *testing.T) {
	const max = 1024
	behind := &ranHandler{}
	h := BodyLimit(max)(behind)

	r := httptest.NewRequest(http.MethodPost, "/v1/orders", bytes.NewReader(make([]byte, max*4)))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rr.Code)
	}
	if behind.ran {
		t.Fatal("the handler behind BodyLimit RAN for an oversized body — the point of the " +
			"Content-Length check is that the heap is never allocated and the HMAC is never " +
			"computed, so refusing after the fact is not the control this needs")
	}
}

// THE CONTENT-LENGTH CHECK MUST NOT READ THE BODY. Asserted by handing it a
// reader that fails the test if anything pulls from it: an implementation that
// bounded the body only with MaxBytesReader would have to read `max` bytes
// before it could report the overrun, which is exactly the allocation being
// prevented.
type poisonReader struct{ t *testing.T }

func (p poisonReader) Read([]byte) (int, error) {
	p.t.Error("BodyLimit READ the body of a request whose Content-Length already exceeded the " +
		"ceiling. The header is the only signal available before allocating, and skipping it " +
		"means the process still buffers the whole payload it is about to refuse.")
	return 0, io.EOF
}

func TestAnOversizedContentLengthIsRefusedWithoutReadingAByte(t *testing.T) {
	// THE HANDLER BEHIND MUST READ, or this asserts nothing. The first version put
	// a non-reading handler here: with the Content-Length check removed the
	// request simply passed through, nothing ever pulled from poisonReader, and
	// the mutation SURVIVED a test whose whole name is about not reading. This
	// stands in for Signing, which buffers the entire body and hashes it.
	reader := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
	})
	h := BodyLimit(1024)(reader)

	r := httptest.NewRequest(http.MethodPost, "/v1/orders", poisonReader{t})
	r.ContentLength = 1 << 30
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rr.Code)
	}
}

// A CHUNKED BODY CARRIES NO CONTENT-LENGTH, so the header check cannot see it
// and the MaxBytesReader is the only thing standing there. ContentLength is -1
// in that case, which must not read as "smaller than max".
func TestABodyThatLiesAboutItsLengthIsStillBounded(t *testing.T) {
	const max = 1024
	var got error
	h := BodyLimit(max)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, got = io.ReadAll(r.Body)
	}))

	r := httptest.NewRequest(http.MethodPost, "/v1/orders", bytes.NewReader(make([]byte, max*4)))
	r.ContentLength = -1 // chunked: the header promises nothing
	h.ServeHTTP(httptest.NewRecorder(), r)

	var tooLarge *http.MaxBytesError
	if got == nil {
		t.Fatal("a chunked body four times the ceiling read to completion — Content-Length is " +
			"absent on a chunked request and a caller may simply lie about it, so the reader " +
			"is the only bound left")
	}
	if !errors.As(got, &tooLarge) {
		t.Fatalf("read error = %v, want *http.MaxBytesError", got)
	}
}

func TestABodyWithinTheLimitPassesThroughUntouched(t *testing.T) {
	const max = 1024
	payload := []byte(strings.Repeat("x", max/2))
	var seen []byte
	h := BodyLimit(max)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen, _ = io.ReadAll(r.Body)
	}))

	r := httptest.NewRequest(http.MethodPost, "/v1/orders", bytes.NewReader(payload))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if !bytes.Equal(seen, payload) {
		t.Fatalf("handler saw %d bytes, want %d — the limiter is corrupting or truncating a "+
			"legitimate body", len(seen), len(payload))
	}
}

// A NON-POSITIVE MAX IS THE CEILING, NOT "UNLIMITED". Zero is what an unset
// config reads as, and a bound that disables itself on a misconfiguration is the
// defect rather than a lenient default.
func TestAZeroLimitFallsBackToTheCeilingRatherThanDisabling(t *testing.T) {
	behind := &ranHandler{}
	h := BodyLimit(0)(behind)

	over := httptest.NewRequest(http.MethodPost, "/v1/orders", bytes.NewReader(nil))
	over.ContentLength = MaxRequestBody + 1
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, over)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 — BodyLimit(0) disabled the bound", rr.Code)
	}
	if behind.ran {
		t.Fatal("the handler ran under BodyLimit(0) for an oversized body")
	}

	// THE ARM THAT ACTUALLY DISTINGUISHES THE FALLBACK, and the first version of
	// this test lacked it. Dropping `max = MaxRequestBody` leaves max at 0, so
	// `r.ContentLength > 0` refuses EVERY body — stricter, not unlimited — and the
	// refusal above then passed for the wrong reason. What the fallback really
	// decides is whether a LEGITIMATE body still gets through, which is the
	// difference between a sane default and a total outage on an unset config.
	behind.ran = false
	under := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader("{}"))
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, under)

	if rr.Code != http.StatusOK || !behind.ran {
		t.Fatalf("a 2-byte body under BodyLimit(0) ⇒ %d (handler ran: %v), want 200. "+
			"max stayed 0 rather than falling back to MaxRequestBody, so every request "+
			"carrying a body is refused — an unset config becomes a total outage on /v1 "+
			"rather than the documented ceiling.", rr.Code, behind.ran)
	}
}

// AN OVERSIZED BODY IS 413, NOT 401 — the issue's second explicit requirement.
//
// Signing verifies an HMAC over the buffered body. Before #887 it discarded the
// read error, so a body cut short by the reader produced a TRUNCATED prefix that
// failed Verify and came back as "invalid request signature". The caller is then
// sent to check its signing key for a request that was simply too big, and the
// two conditions become indistinguishable.
func TestAnOversizedBodyIsNotReportedAsASignatureFailure(t *testing.T) {
	const (
		secret = "s3cr3t"
		max    = 1024
	)
	behind := &ranHandler{}
	// The chain as wired: BodyLimit outside Signing.
	h := BodyLimit(max)(Signing(secret)(behind))

	payload := make([]byte, max*4)
	r := httptest.NewRequest(http.MethodPost, "/v1/orders", bytes.NewReader(payload))
	r.ContentLength = -1 // chunked, so only the reader can catch it
	// A VALID signature over the real payload, so nothing here is a genuine
	// signature failure — the only thing wrong with this request is its size.
	r.Header.Set(gatewaysig.Header, gatewaysig.Sign([]byte(secret), http.MethodPost, "/v1/orders", payload))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if rr.Code == http.StatusUnauthorized {
		t.Fatal("an oversized body was reported as 401. \"Too large\" and \"unsigned\" are " +
			"different answers: a caller told 401 goes and checks its signing key, and never " +
			"discovers the request was refused for its size")
	}
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rr.Code)
	}
	if behind.ran {
		t.Fatal("the handler ran for an oversized body")
	}
}
