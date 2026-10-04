package execution

import (
	"fmt"
	"strings"
)

// ExchangeInstrument is metadata returned by the exchange for the exact symbol
// the adapter would send on an order. It is distinct from a configured symbol
// map: only the exchange can attest the settlement assets and trading state.
type ExchangeInstrument struct {
	Symbol   string
	Base     string
	Quote    string
	State    string
	FeeGroup string // OKX instrument catalogue binding; empty for venues without groups
	// Digest identifies the exact exchange response; it is not a future-validity
	// guarantee. ObservedAt is the local time of the completed exchange read.
	Digest               string
	ObservedAtUnixMillis int64
}

// CheckExchangeCatalogue refuses a deployment whose configured instruments do
// not match the exchange's current instrument catalogue. Unknown or inactive
// symbols cannot be made safe by parsing their spelling.
func CheckExchangeCatalogue(configured []InstrumentSymbol, exchange []ExchangeInstrument, activeState string) error {
	if len(configured) == 0 {
		return fmt.Errorf("exchange catalogue: no configured instruments")
	}
	listed := make(map[string]ExchangeInstrument, len(exchange))
	for _, in := range exchange {
		if in.Symbol == "" || in.Base == "" || in.Quote == "" {
			return fmt.Errorf("exchange catalogue: incomplete instrument metadata")
		}
		if _, duplicate := listed[in.Symbol]; duplicate {
			return fmt.Errorf("exchange catalogue: duplicate symbol %q", in.Symbol)
		}
		listed[in.Symbol] = in
	}
	for _, cfg := range configured {
		got, ok := listed[cfg.VenueSymbol]
		if !ok {
			return fmt.Errorf("exchange catalogue: configured symbol %q absent", cfg.VenueSymbol)
		}
		if got.State != activeState {
			return fmt.Errorf("exchange catalogue: symbol %q state %q, expected %q", cfg.VenueSymbol, got.State, activeState)
		}
		if !strings.EqualFold(cfg.Pair.Base, got.Base) || !strings.EqualFold(cfg.Pair.Quote, got.Quote) {
			return fmt.Errorf("exchange catalogue: %q configured as %s/%s, exchange reports %s/%s", cfg.VenueSymbol, cfg.Pair.Base, cfg.Pair.Quote, got.Base, got.Quote)
		}
		claimed := strings.Split(cfg.InstrumentID, "-")
		if len(claimed) != 2 || !strings.EqualFold(claimed[0], got.Base) || !strings.EqualFold(claimed[1], got.Quote) {
			return fmt.Errorf("exchange catalogue: instrument %q does not name exchange pair %s/%s", cfg.InstrumentID, got.Base, got.Quote)
		}
		if cfg.QuoteMismatch {
			return fmt.Errorf("exchange catalogue: %q instrument identity disagrees with exchange settlement assets", cfg.InstrumentID)
		}
	}
	return nil
}
