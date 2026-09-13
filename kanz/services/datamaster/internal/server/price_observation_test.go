package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/services/datamaster/internal/feed"
	"github.com/eighred/kanz/services/datamaster/internal/pricing"
	"github.com/eighred/kanz/services/datamaster/internal/store"
)

func TestPriceObservationNeverTouchesExceptionStore(t *testing.T) {
	// A nil store makes any dependency on persistence fail, even if a duplicate
	// INSERT would have left the queue unchanged and hidden the unsafe GET.
	s := New(&Readiness{}, nil, testTenant, nil, nil, testFeeds(), WithClock(func() time.Time { return now }))
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for _, id := range []string{"INST1", "UNKNOWN"} {
				r := httptest.NewRecorder()
				s.ServeHTTP(r, asTenant(http.MethodGet, "/v1/prices/"+id, testTenant))
				if r.Code != http.StatusOK {
					t.Errorf("read status: %d", r.Code)
				}
			}
		})
	}
	wg.Wait()
}

func TestPriceObservationExactAndMissing(t *testing.T) {
	const exact = "12345678901.12345678"
	for _, tc := range []struct {
		name       string
		candidates []pricing.Candidate
		hasPrice   bool
		chosen     string
		stale      int
	}{
		{"exact", []pricing.Candidate{{InstrumentID: "X", Source: "test", Price: dec.Rat(exact), AsOf: now}}, true, exact, 0},
		{"median", []pricing.Candidate{{InstrumentID: "X", Source: "a", Price: dec.Rat("1.00000001"), AsOf: now}, {InstrumentID: "X", Source: "b", Price: dec.Rat("1.00000002"), AsOf: now}}, true, "1.000000015", 0},
		{"zero", []pricing.Candidate{{InstrumentID: "X", Source: "test", Price: dec.Rat("0"), AsOf: now}}, true, "0", 0},
		{"missing", nil, false, "", 0},
		{"stale", []pricing.Candidate{{InstrumentID: "X", Source: "test", Price: dec.Rat(exact), AsOf: now.Add(-48 * time.Hour)}}, false, "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			queue := store.NewQueueStore(nil, nil)
			s := New(&Readiness{}, nil, testTenant, nil, queue, []feed.VendorFeed{feed.SimFeed{Name: "test", Candidates: tc.candidates}}, WithClock(func() time.Time { return now }))
			r := get(t, s, "/v1/prices/X")
			var out struct {
				HasPrice        bool    `json:"has_price"`
				Chosen          *string `json:"chosen"`
				ObservationOnly bool    `json:"observation_only"`
				Stale           int     `json:"stale_candidates"`
				ObservedAt      string  `json:"observed_at"`
			}
			if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if r.Code != 200 || out.HasPrice != tc.hasPrice || !out.ObservationOnly || out.Stale != tc.stale || out.ObservedAt != now.Format(time.RFC3339Nano) {
				t.Fatalf("invalid observation: %+v", out)
			}
			if tc.hasPrice {
				if out.Chosen == nil || *out.Chosen != tc.chosen {
					t.Fatalf("lost exact price: %v", out.Chosen)
				}
			} else if out.Chosen != nil {
				t.Fatal("missing price became a price")
			}
			open, err := queue.Open(context.Background())
			if err != nil || len(open) != 0 {
				t.Fatalf("observation wrote exceptions: %v %v", open, err)
			}
		})
	}
}
