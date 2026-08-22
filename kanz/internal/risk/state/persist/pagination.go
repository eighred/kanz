package persist

import (
	"context"
	"errors"
	"fmt"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
)

// Keyset pagination for the bootstrap restore (#674).
//
// # What this replaced
//
// LoadAll ran three unbounded table scans — `FROM positions ORDER BY
// portfolio_id, instrument_id` and `FROM applied_keys` with no WHERE and no
// LIMIT — grouped the whole result in memory, and returned every record as one
// slice. Startup time and peak memory both scaled with total estate size, which
// makes RECOVERY TIME A FUNCTION OF HOW SUCCESSFUL THE PLATFORM IS: the worst
// possible coupling for the component whose job is to come back after a failure.
// It was correct, and it was an availability limit wearing a performance limit's
// clothes.
//
// It was worse on a sharded replica. Bootstrap's own comment says LoadAll
// "returns every portfolio in the tenant's database, which on a SHARDED replica
// is mostly other replicas' portfolios" — so the majority of what was
// materialised was decoded, held, and then discarded by ErrNotOwned.
//
// # Why keyset and not OFFSET
//
// OFFSET makes the database walk and discard the rows it skips, so page N costs
// O(N·pageSize) and the whole walk is quadratic — the same coupling to estate
// size, moved from this process into Postgres. A keyset cursor carries the last
// id and the index does the seek, so every page costs the same.
//
// # Why the loop is here and not inline in the Postgres store
//
// The store's tests are gated on TEST_POSTGRES_URL and skip silently without
// one, and the risky half of pagination is not the SQL — it is the loop. An
// off-by-one skips a portfolio this replica must answer for; a repeat restores
// one twice; a cursor that fails to advance hangs the boot with no error at all.
// Split out, every one of those is provable on a machine with no database, which
// is where this repository does most of its verification. pagination_test.go
// carries that proof.

// ErrCursorStalled is returned when a page source answers with a last id that
// did not advance past the cursor it was given.
//
// IT IS AN ERROR RATHER THAN A BREAK, and the difference matters at boot. Ending
// the walk quietly would truncate the restore and leave this replica serving a
// partial book that it believes is complete — the exact failure the unbounded
// scan could not have. Failing is loud, and a boot that refuses is recoverable in
// a way a boot that silently forgets half the estate is not.
var ErrCursorStalled = errors.New("persist: pagination cursor did not advance")

// pageFunc returns up to limit records whose portfolio id sorts strictly after
// `after`, in ascending id order. An empty `after` starts at the beginning.
type pageFunc func(ctx context.Context, after v1.PortfolioID, limit int) ([]PortfolioRecord, error)

// paginateRecords walks every record through fetch in id order and hands each to
// emit exactly once.
//
// PEAK MEMORY IS ONE PAGE, not one estate, and that is the whole point: the
// caller receives records one at a time and nothing here accumulates them.
//
// limit is a PARAMETER rather than a package var so tests can page at 1 without
// making the production page size mutable at runtime. A const that becomes a var
// "just so tests can shrink it" is how a data race reached main once already.
func paginateRecords(ctx context.Context, fetch pageFunc, limit int, emit func(PortfolioRecord) error) error {
	if limit <= 0 {
		return fmt.Errorf("persist: page limit must be positive, got %d", limit)
	}
	var after v1.PortfolioID
	for {
		// CHECKED BEFORE EACH PAGE, not only between records. A restore is the
		// slowest thing this process does at boot, and a shutdown arriving during
		// it must not wait for the whole estate to be walked.
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := fetch(ctx, after, limit)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for i := range page {
			if err := emit(page[i]); err != nil {
				return err
			}
		}
		next := page[len(page)-1].ID
		if next == after {
			return fmt.Errorf("%w: page source returned %q as its last id after being asked for "+
				"ids beyond %q, so the next page would repeat this one forever", ErrCursorStalled, next, after)
		}
		after = next
		// A SHORT PAGE DOES NOT END THE WALK; ONLY AN EMPTY ONE DOES.
		//
		// For a plain `WHERE id > $1 ORDER BY id LIMIT n` a short page DOES mean
		// exhausted, and stopping there would save exactly one round trip per
		// restore. It is not taken, because the obvious next version of this code
		// breaks it: a sharded replica discards most of what it loads (bootstrap's
		// own comment says LoadAll "is mostly other replicas' portfolios"), so a
		// page source that filters by ownership before returning is the natural
		// optimisation — and under it short pages become routine while the estate
		// is nowhere near exhausted.
		//
		// Stopping on short would then TRUNCATE THE RESTORE SILENTLY, leaving this
		// replica serving a partial book it believes is complete. That is a worse
		// failure than the unbounded scan this replaced, and it would be invisible.
		// One extra query per boot is not a price worth arguing about.
	}
}
