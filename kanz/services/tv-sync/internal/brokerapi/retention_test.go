package brokerapi

// WHAT THE BROKER API SAYS WHEN THE HISTORY IS A WINDOW (#809).
//
// tv-sync now bounds the folded history it keeps in memory. Two things must reach
// the client, and both of them are about not letting a short answer read as a
// complete one:
//
//  1. A LIST IS A WINDOW AND SAYS SO. Orders and executions older than the
//     retention window are not resident; a client counting rows without knowing
//     that reports "this fund placed four orders" for a fund that placed four
//     thousand.
//  2. AN AS-OF READ BEHIND THE WINDOW IS 410, NOT 200 AND NOT 404. The account
//     exists and the question is well formed; this process simply no longer holds
//     the history to answer it. A 200 carrying a fold of the surviving window
//     would be the worst of the three — a plausible position and P&L for a book
//     the fund never had.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/services/tv-sync/internal/projection"
)

type tickingClock struct{ t time.Time }

func (c *tickingClock) now() time.Time { return c.t }

// windowedHandler folds a day of fills through a one-hour window and evicts, so
// every read below is answered by a projection that has genuinely forgotten
// something.
func windowedHandler(t *testing.T) (*http.ServeMux, *projection.Projection, *tickingClock) {
	t.Helper()
	clock := &tickingClock{t: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}
	p := projection.New(clock.now, nil, projection.WithRetention(time.Hour))
	mux := http.NewServeMux()
	New(p).Routes(mux)

	for i, id := range []string{"o1", "o2", "o3", "o4"} {
		foldFill(t, p, "acme", "fund-alpha", id, "BTC", orderpb.Side_SIDE_BUY, 1, int64(100+i))
		clock.t = clock.t.Add(2 * time.Hour)
	}
	p.Evict(clock.t)
	return mux, p, clock
}

func TestRESTAListReadDeclaresItsWindow(t *testing.T) {
	mux, p, _ := windowedHandler(t)
	from, ok := p.RetainedFrom("acme", "fund-alpha")
	if !ok || from.IsZero() {
		t.Fatal("the fixture evicted nothing, so there is no window to declare")
	}

	for _, path := range []string{"orders", "executions"} {
		rec := get(t, mux, "/broker/accounts/fund-alpha/"+path, "acme")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", path, rec.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		got, present := body["retainedFrom"]
		if !present {
			t.Errorf("GET %s returned a list with no retainedFrom while the projection has "+
				"forgotten history from before %s. A short list read as a complete one is a wrong "+
				"answer about how a fund traded, not a truncated one (#809)", path, from)
			continue
		}
		if got != from.UTC().Format(time.RFC3339Nano) {
			t.Errorf("GET %s retainedFrom = %v, want %s", path, got, from.UTC().Format(time.RFC3339Nano))
		}
	}
}

// TestRESTPositionsDoNotDeclareAWindow is the other half, and it is not
// cosmetic: positions and state are FOLDS, and what retention evicted was folded
// into the account's baseline on the way out, so those figures are exact. Telling
// a client they are windowed would teach it to distrust the one number that is
// not.
func TestRESTPositionsDoNotDeclareAWindow(t *testing.T) {
	mux, _, _ := windowedHandler(t)
	for _, path := range []string{"positions", "state"} {
		rec := get(t, mux, "/broker/accounts/fund-alpha/"+path, "acme")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", path, rec.Code)
		}
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if _, present := body["retainedFrom"]; present {
			t.Errorf("GET %s declared a retention window. The evicted history is folded into the "+
				"account baseline, so this figure is exact — marking it windowed misreports an exact "+
				"number as a partial one (#809)", path)
		}
	}
}

// TestRESTAnAsOfReadBehindTheWindowIs410.
//
// 404 would say the fund does not exist. 200 would hand a trader a fold of the
// surviving window — a position, an average cost and a realized P&L that look
// correct and describe a book that never existed. 410 says the resource is gone
// from here, and the body names the instant from which this pod can still answer.
func TestRESTAnAsOfReadBehindTheWindowIs410(t *testing.T) {
	mux, p, _ := windowedHandler(t)
	from, _ := p.RetainedFrom("acme", "fund-alpha")
	behind := from.Add(-time.Hour).UTC().Format(time.RFC3339)

	for _, path := range []string{"positions", "state", "orders", "executions"} {
		rec := get(t, mux, "/broker/accounts/fund-alpha/"+path+"?as_of="+behind, "acme")
		if rec.Code != http.StatusGone {
			t.Errorf("GET %s?as_of=%s = %d, want 410. Answering it at all means folding a partial "+
				"history into a number indistinguishable from a correct one (#809)", path, behind, rec.Code)
		}
	}

	// An account that does not exist is still 404, so the two failures stay
	// distinguishable to an operator reading a log.
	if rec := get(t, mux, "/broker/accounts/no-such-fund/positions", "acme"); rec.Code != http.StatusNotFound {
		t.Errorf("a missing account = %d, want 404 — 410 there would say the history aged out of a "+
			"fund that never existed", rec.Code)
	}
	// And a read INSIDE the window is served normally.
	inside := from.Add(time.Minute).UTC().Format(time.RFC3339)
	if rec := get(t, mux, "/broker/accounts/fund-alpha/positions?as_of="+inside, "acme"); rec.Code != http.StatusOK {
		t.Errorf("an as-of read inside the resident window = %d, want 200 — the horizon must bound "+
			"only the questions this pod genuinely cannot answer", rec.Code)
	}
}
