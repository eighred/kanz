package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/audit/signer"
)

// THIS SERVICE HOLDS ITS OWN BOUND (#769).
//
// It was the only /v1 surface on the estate with none: decode read the request
// body straight into json.Decode, while mcp caps at 1 MiB and optimization at
// 8 MiB "because an unbounded body drives O(n³) solver work".
//
// It was never a live hole — :8103 is admitted from app: api-gateway only, and
// the gateway answers 413 above its own 1 MiB — but the protection was entirely
// a peer's configuration, and it disappears if the NetworkPolicy widens, a
// second caller is added, or somebody port-forwards for a debug session.
//
// #767 made it worth closing rather than noting: POST /v1/screening/esg takes an
// array of positions whose per-element cost is a big.Rat parse plus a COMP-01
// evaluation, which is the first route here whose work scales with an
// attacker-chosen element count.

func bodyLimitServer(t *testing.T) *Server {
	t.Helper()
	r := &Readiness{}
	r.Set(true)
	return New(r, nil, signer.New(""))
}

// AN OVERSIZED BODY IS 413, and it is refused on EVERY /v1 route rather than
// only the newest one — decode is shared, and a bound that covered one route
// would be a bound the next route silently does not get.
func TestBodyLimit_AnOversizedBodyIsRefusedOnEveryRoute(t *testing.T) {
	s := bodyLimitServer(t)
	// VALID JSON, and oversized. A body of raw bytes fails the syntax check on
	// its FIRST byte, so the decoder never reads far enough to trip the limit and
	// the answer is 400 — which is how the first version of this test passed
	// against a server that had no cap at all. The huge value has to be inside
	// well-formed JSON for the reader to be the thing that stops it.
	huge := []byte(`{"portfolio_id":"` + strings.Repeat("a", maxRequestBytes+1024) + `"}`)

	for _, path := range []string{
		"/v1/filings/frtb", "/v1/filings/formpf", "/v1/filings/aifmd",
		"/v1/filings/tcfd", "/v1/filings/sfdr", "/v1/screening/esg",
		"/v2/screening/esg",
	} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(huge)))
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s answered %d for a %d-byte body, want 413", path, rec.Code, len(huge))
		}
	}
}

// TOO LARGE AND MALFORMED ARE DIFFERENT ANSWERS. 413 says send less, 400 says
// send it correctly; a caller that cannot tell them apart retries the one thing
// that cannot work. json.Decode collapses both unless the limit error is checked
// for by type.
func TestBodyLimit_AMalformedBodyIsStill400(t *testing.T) {
	s := bodyLimitServer(t)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/screening/esg",
		strings.NewReader(`{"portfolio_id": THIS IS NOT JSON`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a malformed body answered %d, want 400 — collapsing it into 413 would tell a "+
			"caller to send less of something that is not too big", rec.Code)
	}
}

// NON-VACUITY: an ordinary body is not refused. Without this the assertions
// above are satisfied by a server that rejects everything.
func TestBodyLimit_AnOrdinaryBodyIsAccepted(t *testing.T) {
	s := bodyLimitServer(t)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/screening/esg",
		strings.NewReader(`{"portfolio_id":"p1","base_currency":"USD"}`)))

	if rec.Code == http.StatusRequestEntityTooLarge || rec.Code == http.StatusBadRequest {
		t.Fatalf("a small well-formed body answered %d: %s", rec.Code, rec.Body.String())
	}
}
