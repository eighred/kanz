package persist

// The keyset pagination behind LoadEach, proven WITHOUT a database (#674).
//
// postgres_test.go is gated on TEST_POSTGRES_URL and skips silently without one,
// so anything proven only there is unproven on most machines. The risky half of
// pagination is not the SQL — it is the LOOP: an off-by-one that skips a record,
// a repeat that restores one twice, or a cursor that fails to advance and spins
// forever. None of that needs Postgres to exercise, and all of it is what a
// restore must never get wrong.
//
// The store's own doc makes the same split: "The DB-free idempotent-replay
// property is proven in the app package's bootstrap_test.go; these prove the
// durable contract that underpins it."

import (
	"context"
	"errors"
	"fmt"
	"testing"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
)

// fakePages is a page source over a fixed, ordered estate.
type fakePages struct {
	ids []string
	// calls records the (after, limit) of every page request, so a test can
	// assert the walk is bounded rather than assuming it.
	calls []pageCall
	// short truncates the FIRST page to this many rows, standing in for a
	// database returning fewer rows than asked for without being at the end.
	short int
}

type pageCall struct {
	after v1.PortfolioID
	limit int
}

func (f *fakePages) fetch(_ context.Context, after v1.PortfolioID, limit int) ([]PortfolioRecord, error) {
	f.calls = append(f.calls, pageCall{after: after, limit: limit})
	start := 0
	if after != "" {
		for i, id := range f.ids {
			if id == string(after) {
				start = i + 1
				break
			}
		}
	}
	end := start + limit
	if len(f.calls) == 1 && f.short > 0 && start+f.short < end {
		end = start + f.short
	}
	if end > len(f.ids) {
		end = len(f.ids)
	}
	out := make([]PortfolioRecord, 0, end-start)
	for _, id := range f.ids[start:end] {
		out = append(out, PortfolioRecord{ID: v1.PortfolioID(id)})
	}
	return out, nil
}

func ids(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		// Fixed width so lexical order matches numeric order, as portfolio_id
		// ordering in the query is lexical.
		out = append(out, fmt.Sprintf("pf-%05d", i))
	}
	return out
}

// EVERY RECORD, EXACTLY ONCE, IN ORDER. A restore that skips one leaves a
// portfolio this replica must answer for answering from nothing; a restore that
// repeats one applies its state twice.
func TestPaginationVisitsEveryRecordExactlyOnce(t *testing.T) {
	for _, tc := range []struct{ total, page int }{
		{total: 0, page: 4},
		{total: 1, page: 4},
		{total: 4, page: 4},   // exactly one full page, then an empty one
		{total: 5, page: 4},   // a full page then a partial
		{total: 100, page: 7}, // many pages, ragged tail
		{total: 9, page: 1},   // degenerate page size
	} {
		t.Run(fmt.Sprintf("total=%d/page=%d", tc.total, tc.page), func(t *testing.T) {
			src := &fakePages{ids: ids(tc.total)}
			var seen []string
			err := paginateRecords(context.Background(), src.fetch, tc.page, func(rec PortfolioRecord) error {
				seen = append(seen, string(rec.ID))
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(seen) != tc.total {
				t.Fatalf("emitted %d record(s), want %d", len(seen), tc.total)
			}
			for i, got := range seen {
				if want := src.ids[i]; got != want {
					t.Fatalf("record %d = %q, want %q — the walk is out of order or skipping", i, got, want)
				}
			}
		})
	}
}

// EVERY QUERY IS BOUNDED. The whole point is that no single read can be
// proportional to the estate; a limit that fails to reach the driver is the
// original defect wearing a loop.
func TestEveryPageRequestCarriesTheLimit(t *testing.T) {
	src := &fakePages{ids: ids(50)}
	if err := paginateRecords(context.Background(), src.fetch, 8, func(PortfolioRecord) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(src.calls) == 0 {
		t.Fatal("no page was requested")
	}
	for i, c := range src.calls {
		if c.limit != 8 {
			t.Fatalf("page request %d asked for limit %d, want 8", i, c.limit)
		}
	}
	// 50 records at 8 per page is 6 full pages, then a partial of 2, then one
	// EMPTY page that ends the walk: 8 requests. The walk deliberately does not
	// stop on the partial — see paginateRecords on why a filtering page source
	// would make that silently truncate the restore.
	if len(src.calls) != 8 {
		t.Fatalf("made %d page requests, want 8 (6 full + 1 partial + 1 empty) — fewer means "+
			"records were skipped, more means the cursor is not advancing", len(src.calls))
	}
}

// A SHORT PAGE IS NOT THE END, when the store simply returned fewer rows than
// asked for. Treating it as the end would silently truncate the restore, which
// is the same failure as a missing WHERE in the opposite direction.
func TestAShortFirstPageDoesNotEndTheWalk(t *testing.T) {
	src := &fakePages{ids: ids(20), short: 3}
	var seen int
	err := paginateRecords(context.Background(), src.fetch, 8, func(PortfolioRecord) error {
		seen++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != 20 {
		t.Fatalf("restored %d of 20 records — a short page ended the walk early", seen)
	}
}

// A CURSOR THAT DOES NOT ADVANCE MUST FAIL, NOT SPIN. A store that keeps
// answering with the same last id would otherwise hang the boot forever, holding
// readiness down with no error and no log — the failure mode that is hardest to
// diagnose because nothing is reported at all.
func TestANonAdvancingCursorIsRefusedRatherThanLooping(t *testing.T) {
	same := PortfolioRecord{ID: "pf-stuck"}
	stuck := func(context.Context, v1.PortfolioID, int) ([]PortfolioRecord, error) {
		return []PortfolioRecord{same, same}, nil
	}
	err := paginateRecords(context.Background(), stuck, 2, func(PortfolioRecord) error { return nil })
	if err == nil {
		t.Fatal("a store whose cursor never advances did not error — this loops forever in production")
	}
	if !errors.Is(err, ErrCursorStalled) {
		t.Fatalf("err = %v, want ErrCursorStalled", err)
	}
}

// THE EMITTER'S ERROR STOPS THE WALK. Bootstrap aborts on any restore failure
// other than a foreign shard, and continuing to page after it has given up would
// do work whose result nobody reads.
func TestAnEmitterErrorStopsTheWalkImmediately(t *testing.T) {
	src := &fakePages{ids: ids(100)}
	boom := errors.New("restore refused")
	var seen int
	err := paginateRecords(context.Background(), src.fetch, 8, func(PortfolioRecord) error {
		seen++
		if seen == 3 {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the emitter's error", err)
	}
	if seen != 3 {
		t.Fatalf("emitted %d records after the failure — the walk did not stop", seen)
	}
	if len(src.calls) != 1 {
		t.Fatalf("requested %d pages after an emitter failure on page one, want 1", len(src.calls))
	}
}

// A CANCELLED CONTEXT STOPS THE WALK. A restore is the slowest thing this
// process does at boot; a shutdown signal during it must not be ignored until
// the whole estate has been paged.
func TestACancelledContextStopsTheWalk(t *testing.T) {
	src := &fakePages{ids: ids(100)}
	ctx, cancel := context.WithCancel(context.Background())
	var seen int
	err := paginateRecords(ctx, src.fetch, 8, func(PortfolioRecord) error {
		seen++
		if seen == 2 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
