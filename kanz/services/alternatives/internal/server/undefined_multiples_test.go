package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eighred/kanz/internal/alternatives"
	"github.com/eighred/kanz/services/alternatives/internal/fund"
)

// THE WIRE CONTRACT: AN UNDEFINED MULTIPLE IS ABSENT, NOT ZERO (#623).
//
// handlePosition wrote tvpi/dpi/rvpi unconditionally while OMITTING irr when it
// could not be solved — two treatments of the same question, five lines apart in
// one response body. A commitment with nothing drawn therefore served
// `"tvpi": 0`, which an investor-facing client reads as a fund that returned
// nothing on the capital it took.
//
// These assert the SERVED BODY rather than the metric function, because the
// substitution that matters happens at the boundary: a caller cannot tell 0 from
// undefined once the key is written, and no amount of care inside
// ComputeMultiples fixes a handler that writes the zero anyway.

func seedUndrawn(t *testing.T, store fund.Store) {
	t.Helper()
	// Committed but never called: the multiples have no denominator.
	e := &alternatives.Event{
		EventID: "u1", CommitmentID: "UNDRAWN", Type: alternatives.EventCommit,
		Amount: bigRat(1000), Date: day(2021, 1, 1),
	}
	if err := store.Append(context.Background(), e); err != nil {
		t.Fatal(err)
	}
}

func positionBody(t *testing.T, s *Server, id string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/commitments/"+id, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("position %s: want 200 got %d (%s)", id, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", id, err)
	}
	return out
}

func TestAnUndrawnCommitmentOmitsTheMultiplesRatherThanServingZero(t *testing.T) {
	s, store := newServer(t)
	seedUndrawn(t, store)

	out := positionBody(t, s, "UNDRAWN")

	for _, key := range []string{"tvpi", "dpi", "rvpi"} {
		if v, present := out[key]; present {
			t.Errorf("%q was served as %v for a commitment with nothing drawn — an undefined "+
				"multiple must be ABSENT, the way irr already is, or a client cannot tell it from "+
				"a fund that lost everything", key, v)
		}
	}
	// The facts that ARE known are still served: this must not become a hole.
	if out["committed"] != "1000.00" {
		t.Fatalf("committed: want 1000.00 got %v", out["committed"])
	}
	if out["called"] != "0.00" {
		t.Fatalf("called: want 0.00 got %v", out["called"])
	}
}

// THE ZERO THAT IS REAL IS STILL SERVED. Capital drawn and nothing returned is a
// total loss and the most consequential figure on this surface; refusing it would
// be the same collapse pointing the other way.
func TestATotalLossServesZeroMultiples(t *testing.T) {
	s, store := newServer(t)
	for _, e := range []*alternatives.Event{
		{EventID: "l1", CommitmentID: "LOSS", Type: alternatives.EventCommit, Amount: bigRat(1000), Date: day(2021, 1, 1)},
		{EventID: "l2", CommitmentID: "LOSS", Type: alternatives.EventCall, Amount: bigRat(400), Date: day(2021, 1, 1)},
	} {
		if err := store.Append(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}

	out := positionBody(t, s, "LOSS")

	for _, key := range []string{"tvpi", "dpi", "rvpi"} {
		v, present := out[key]
		if !present {
			t.Fatalf("%q is missing for a commitment that DREW capital — this is a measured zero "+
				"and omitting it hides a total loss", key)
		}
		if f, ok := v.(float64); !ok || f != 0 {
			t.Fatalf("%q = %v, want 0", key, v)
		}
	}
}

// The ordinary, fully-drawn case is unchanged — the repair must not move a
// figure anyone already reads.
func TestADrawnCommitmentStillServesItsMultiples(t *testing.T) {
	s, store := newServer(t)
	seed(t, store)

	out := positionBody(t, s, "C1")
	if tvpi, ok := out["tvpi"].(float64); !ok || tvpi < 1.49 || tvpi > 1.51 {
		t.Fatalf("tvpi: want ~1.5 got %v", out["tvpi"])
	}
}
