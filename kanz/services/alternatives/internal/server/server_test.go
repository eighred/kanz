package server

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/alternatives"
	"github.com/eighred/kanz/services/alternatives/internal/fund"
)

func day(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC) }

func bigRat(n int64) *big.Rat { return big.NewRat(n, 1) }

func newServer(t *testing.T) (*Server, fund.Store) {
	t.Helper()
	store := fund.NewMemoryStore()
	r := &Readiness{}
	r.Set(true)
	return New(r, nil, store), store
}

func seed(t *testing.T, store fund.Store) {
	t.Helper()
	events := []*alternatives.Event{
		{EventID: "e1", CommitmentID: "C1", Type: alternatives.EventCommit, Amount: bigRat(1000), Date: day(2021, 1, 1)},
		{EventID: "e2", CommitmentID: "C1", Type: alternatives.EventCall, Amount: bigRat(400), Date: day(2021, 1, 1)},
		{EventID: "e3", CommitmentID: "C1", Type: alternatives.EventDistribution, Amount: bigRat(100), Date: day(2021, 6, 1)},
		{EventID: "e4", CommitmentID: "C1", Type: alternatives.EventNAVMark, Amount: bigRat(500), Date: day(2022, 1, 1)},
	}
	for _, e := range events {
		if err := store.Append(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHealthAndReady(t *testing.T) {
	s, _ := newServer(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: want 200 got %d", path, rec.Code)
		}
	}
}

func TestPositionEndpoint(t *testing.T) {
	s, store := newServer(t)
	seed(t, store)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/commitments/C1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("position: want 200 got %d (%s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["uncalled"] != "600.00" { // 1000 committed - 400 called
		t.Fatalf("uncalled: want 600.00 got %v", out["uncalled"])
	}
	if out["distributed"] != "100.00" {
		t.Fatalf("distributed: want 100.00 got %v", out["distributed"])
	}
	// TVPI = (100 + 500) / 400 = 1.5.
	if tvpi, ok := out["tvpi"].(float64); !ok || tvpi < 1.49 || tvpi > 1.51 {
		t.Fatalf("tvpi: want ~1.5 got %v", out["tvpi"])
	}
}
