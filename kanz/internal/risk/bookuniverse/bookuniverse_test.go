package bookuniverse

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/state"
)

var bookAsOf = time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

// storeWith builds a store from portfolio id → instrument ids. Restore is the
// exported single-threaded install path (the bootstrap seam), so the tests need
// no bus and no proto envelopes.
func storeWith(t *testing.T, book map[v1.PortfolioID][]domain.InstrumentID) *state.Store {
	t.Helper()
	s := state.NewStore()
	for id, instruments := range book {
		p := domain.NewPortfolio(id, "USD")
		for _, ins := range instruments {
			p.SetPosition(domain.Position{InstrumentID: ins, AsOf: bookAsOf})
		}
		if err := s.Restore(p, nil); err != nil {
			t.Fatalf("Restore %s: %v", id, err)
		}
	}
	return s
}

func mustUniverse(t *testing.T, s *state.Store, opts ...Option) []string {
	t.Helper()
	fn, err := FromStore(s, opts...)
	if err != nil {
		t.Fatalf("FromStore: %v", err)
	}
	out, err := fn(context.Background(), bookAsOf)
	if err != nil {
		t.Fatalf("universe: %v", err)
	}
	return out
}

func TestInstrumentIDsAreDeduplicatedAcrossPortfoliosAndSorted(t *testing.T) {
	// MSFT is held in three portfolios and must appear ONCE — Fit would turn a
	// repeat into a duplicate, perfectly correlated column.
	s := storeWith(t, map[v1.PortfolioID][]domain.InstrumentID{
		"P-1": {"MSFT", "AAPL", "ZM"},
		"P-2": {"MSFT", "TSLA"},
		"P-3": {"MSFT", "AAPL", "BRK.B"},
	})

	got := mustUniverse(t, s)
	want := []string{"AAPL", "BRK.B", "MSFT", "TSLA", "ZM"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("universe: got %v want %v (deduplicated and lexicographically sorted)", got, want)
	}
}

func TestAnEmptyStoreRefusesRatherThanReturningAnEmptyUniverse(t *testing.T) {
	cases := []struct {
		name  string
		book  map[v1.PortfolioID][]domain.InstrumentID
		ports string // the portfolio count the message must carry
	}{
		{
			name:  "nothing has arrived from the bus yet",
			book:  nil,
			ports: "0 portfolio(s)",
		},
		{
			name:  "portfolios exist and hold nothing",
			book:  map[v1.PortfolioID][]domain.InstrumentID{"P-1": nil, "P-2": nil},
			ports: "2 portfolio(s)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fn, err := FromStore(storeWith(t, tc.book))
			if err != nil {
				t.Fatalf("FromStore: %v", err)
			}
			got, err := fn(context.Background(), bookAsOf)
			if !errors.Is(err, ErrEmptyBook) {
				t.Fatalf("an empty book must refuse with ErrEmptyBook, got (%v, %v) — an empty "+
					"slice with a nil error says the firm holds nothing, which must not look the "+
					"same as nothing having been loaded", got, err)
			}
			if got != nil {
				t.Errorf("a refusal must return no universe, got %v", got)
			}
			// The two causes above have the same empty universe and different
			// remedies, so the count that separates them has to reach the message.
			if !strings.Contains(err.Error(), tc.ports) {
				t.Errorf("error %q must name the portfolio count (%s)", err, tc.ports)
			}
		})
	}
}

func TestAPortfolioWithNoPositionsContributesNothing(t *testing.T) {
	s := storeWith(t, map[v1.PortfolioID][]domain.InstrumentID{
		"P-CASH":  nil, // funded, unallocated — a real state, not an error
		"P-EMPTY": nil,
		"P-LONG":  {"AAPL", "MSFT", "NVDA", "TSLA"},
	})

	got := mustUniverse(t, s)
	want := []string{"AAPL", "MSFT", "NVDA", "TSLA"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("universe: got %v want %v — an empty portfolio must add no id of its own", got, want)
	}
}

func TestTheMinimumUniverseIsEnforcedAtTheBoundary(t *testing.T) {
	// DefaultMinInstruments is DefaultStatFactors+1; below it the PCA's kept
	// components span the whole return space and every specific variance is zero.
	all := []domain.InstrumentID{"AAPL", "MSFT", "NVDA", "TSLA", "XOM"}

	cases := []struct {
		name    string
		held    []domain.InstrumentID
		min     []Option
		wantErr bool
	}{
		{
			name:    "one below the default floor is refused",
			held:    all[:DefaultMinInstruments-1],
			wantErr: true,
		},
		{
			name:    "exactly the default floor is accepted",
			held:    all[:DefaultMinInstruments],
			wantErr: false,
		},
		{
			name:    "one below a raised floor is refused",
			held:    all[:DefaultMinInstruments],
			min:     []Option{WithMinInstruments(DefaultMinInstruments + 1)},
			wantErr: true,
		},
		{
			name:    "exactly a raised floor is accepted",
			held:    all[:DefaultMinInstruments+1],
			min:     []Option{WithMinInstruments(DefaultMinInstruments + 1)},
			wantErr: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := storeWith(t, map[v1.PortfolioID][]domain.InstrumentID{"P-1": tc.held})
			fn, err := FromStore(s, tc.min...)
			if err != nil {
				t.Fatalf("FromStore: %v", err)
			}
			got, err := fn(context.Background(), bookAsOf)
			switch {
			case tc.wantErr && !errors.Is(err, ErrUniverseTooSmall):
				t.Fatalf("a universe of %d must refuse with ErrUniverseTooSmall, got (%v, %v)",
					len(tc.held), got, err)
			case !tc.wantErr && err != nil:
				t.Fatalf("a universe of %d clears the floor and must be returned, got %v",
					len(tc.held), err)
			case !tc.wantErr && len(got) != len(tc.held):
				t.Fatalf("universe: got %d ids want %d", len(got), len(tc.held))
			}
		})
	}
}

func TestAnUnusableFloorIsRefusedAtConfigTimeNotCallTime(t *testing.T) {
	// LiveModelProvider.Model discards this seam's error, so a floor of 0 that
	// only failed at call time would present as factor measures reporting zero
	// forever with nothing naming the cause.
	if _, err := FromStore(state.NewStore(), WithMinInstruments(0)); err == nil {
		t.Error("a minimum of 0 must be refused by FromStore")
	}
	if _, err := FromStore(nil); err == nil {
		t.Error("a nil store must be refused by FromStore")
	}
}

func TestALookaheadObserverFiresWhenAsOfPredatesTheBook(t *testing.T) {
	s := storeWith(t, map[v1.PortfolioID][]domain.InstrumentID{
		"P-1": {"AAPL", "MSFT", "NVDA", "TSLA"},
	})

	var fired int
	var gotAsOf, gotNewest time.Time
	fn, err := FromStore(s, WithLookaheadObserver(func(asOf, newest time.Time) {
		fired++
		gotAsOf, gotNewest = asOf, newest
	}))
	if err != nil {
		t.Fatalf("FromStore: %v", err)
	}

	// A live recompute asks about the present: the universe IS point-in-time and
	// the hook must stay quiet, or the counter is noise nobody will trust.
	if _, err := fn(context.Background(), bookAsOf.Add(time.Hour)); err != nil {
		t.Fatalf("universe: %v", err)
	}
	if fired != 0 {
		t.Fatalf("an asOf at or after the book's own state is not lookahead; hook fired %d time(s)", fired)
	}

	// A backtest asks about a month ago and gets today's holdings.
	past := bookAsOf.AddDate(0, -1, 0)
	if _, err := fn(context.Background(), past); err != nil {
		t.Fatalf("universe: %v", err)
	}
	if fired != 1 {
		t.Fatalf("an asOf older than the book's state must fire the observer once, got %d", fired)
	}
	if !gotAsOf.Equal(past) || !gotNewest.Equal(bookAsOf) {
		t.Errorf("observer args: got (%v, %v) want (%v, %v)", gotAsOf, gotNewest, past, bookAsOf)
	}
}
