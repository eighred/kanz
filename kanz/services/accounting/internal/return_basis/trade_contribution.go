package returnbasis

import (
	"math/big"
	"time"

	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// TradeCoverage attests the complete dated selection of booked fills.
// Null fills denotes unknown coverage, distinct from an explicit empty set.
type TradeCoverage struct {
	Reference string           `json:"reference"`
	Fills     []TradeReference `json:"fills"`
}

type TradeReference struct {
	EntryID   string    `json:"entry_id"`
	Effective time.Time `json:"effective"`
}

func validTradeCoverage(coverage *TradeCoverage) bool {
	if coverage == nil {
		return true
	}
	if !identifier.MatchString(coverage.Reference) || len(coverage.Fills) > 64 {
		return false
	}
	seen := make(map[string]bool, len(coverage.Fills))
	for _, fill := range coverage.Fills {
		if !identifier.MatchString(fill.EntryID) || seen[fill.EntryID] ||
			fill.Effective.IsZero() || fill.Effective.Location() != time.UTC || fill.Effective.Nanosecond()%1000 != 0 {
			return false
		}
		seen[fill.EntryID] = true
	}
	return true
}

type tradeLegs struct {
	principal map[string]*big.Rat
	cash      big.Rat
	fees      big.Rat
}

func verifyTrades(e Evidence, entries []*ledger.Event, holdings map[string]*big.Rat, start, end, flow time.Time) (tradeLegs, tradeLegs, string) {
	var first, second tradeLegs
	quantity := make(map[string]*big.Rat, len(holdings))
	for id, q := range holdings {
		if q.Sign() < 0 {
			return first, second, "position_activity_unsupported"
		}
		quantity[id] = new(big.Rat).Set(q)
	}
	coverage := e.PositionBasis.TradeCoverage
	if coverage == nil {
		return first, second, ""
	}
	if coverage.Fills == nil {
		return first, second, "trade_coverage_missing"
	}
	expected := make(map[string]time.Time, len(coverage.Fills))
	for _, fill := range coverage.Fills {
		expected[fill.EntryID] = fill.Effective
	}
	first.principal = make(map[string]*big.Rat)
	second.principal = make(map[string]*big.Rat)
	for _, entry := range entries {
		if entry.Type != ledger.EntryTrade || !entry.Effective.After(start) || entry.Effective.After(end) {
			continue
		}
		at, ok := expected[entry.EntryID]
		if !ok {
			return first, second, "trade_coverage_mismatch"
		}
		if !at.Equal(entry.Effective) || (!flow.IsZero() && at.Equal(flow)) {
			return first, second, "trade_timing_unverified"
		}
		fee, err := ledger.VerifiedTradeFee(entry)
		if err != nil || entry.CashCurrency != e.Currency {
			return first, second, "trade_economics_unverified"
		}
		if sumByInstrument(quantity, entry.InstrumentID, entry.Quantity).Sign() < 0 {
			return first, second, "position_activity_unsupported"
		}
		period := &first
		if !flow.IsZero() && at.After(flow) {
			period = &second
		}
		sumByInstrument(period.principal, entry.InstrumentID, new(big.Rat).Mul(entry.Quantity, entry.Price))
		period.cash.Add(&period.cash, entry.Cash)
		period.fees.Add(&period.fees, fee)
		delete(expected, entry.EntryID)
	}
	if len(expected) != 0 {
		return first, second, "trade_coverage_mismatch"
	}
	return first, second, ""
}

func sumByInstrument(values map[string]*big.Rat, id string, change *big.Rat) *big.Rat {
	next := new(big.Rat)
	if previous := values[id]; previous != nil {
		next.Set(previous)
	}
	next.Add(next, change)
	values[id] = next
	return next
}

func deltaPosition(p contributionPeriod, id string) *big.Rat {
	change := new(big.Rat)
	if closing := p.to.positions[id]; closing != nil {
		change.Set(closing)
	}
	if opening := p.from.positions[id]; opening != nil {
		change.Sub(change, opening)
	}
	if principal := p.trading.principal[id]; principal != nil {
		change.Sub(change, principal)
	}
	return change
}
