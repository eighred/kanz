package feed

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/kanz-eng/kanz/services/datamaster/internal/master"
	"github.com/kanz-eng/kanz/services/datamaster/internal/pricing"
)

// The price half of the vendor seam. reference.go already established that a
// reference feed carries no prices ("price arbitration is fed by the market-data
// price adapters, not the reference feed") — this is the other side of that
// sentence, and it had no implementation either.
//
// It is deliberately a SEPARATE source from RefSource rather than extra columns on
// RefRow. Reference data and price data have different shapes, different delivery
// cadences (a security master changes on corporate actions; a price changes all
// day) and different failure modes, and a vendor delivers them as different files.
// Folding them into one row type would force a full reference re-delivery to update
// a price.

// PriceRow is one vendor-native price observation — the identifiers it is quoted
// against, the price, and when it was struck. The vendor adapter decodes its
// native format into this shape.
type PriceRow struct {
	Symbol, ISIN, CUSIP, FIGI string
	// Price is exact. A vendor quotes a decimal; nothing on this path is allowed
	// to turn it into a binary double (DATA-M8b).
	Price *big.Rat
	AsOf  time.Time
}

// PriceSource is the vendor price-feed transport seam. There is no Priority: a
// consensus is the MEDIAN of what the sources say, not the opinion of the most
// trusted one. Survivorship ranks vendors for reference fields; arbitration
// deliberately does not, because a trusted vendor's fat-finger is still a
// fat-finger and the whole point of the median is that one source cannot carry it.
type PriceSource interface {
	Vendor() string
	Fetch(ctx context.Context) ([]PriceRow, error)
}

// PriceAdapter implements VendorFeed over a price PriceSource. It is a pure price
// feed — Records returns nil (the security master is resolved from the reference
// feeds, not from whatever a price file happens to mention).
type PriceAdapter struct {
	src      PriceSource
	resolver IDResolver
}

// NewPriceAdapter builds the adapter over a vendor price source. A nil resolver
// uses DefaultIDResolver — the SAME resolver the reference adapter uses, which is
// what makes a price land on the instrument the reference feed mastered. Two
// different id policies here would silently price the wrong security.
func NewPriceAdapter(src PriceSource, resolver IDResolver) *PriceAdapter {
	if resolver == nil {
		resolver = DefaultIDResolver
	}
	return &PriceAdapter{src: src, resolver: resolver}
}

// Vendor returns the source vendor name.
func (a *PriceAdapter) Vendor() string { return a.src.Vendor() }

// Records returns nil — a price feed carries no reference records.
func (a *PriceAdapter) Records(context.Context) ([]master.VendorRecord, error) { return nil, nil }

// Prices fetches the vendor's rows and keys each one to a canonical instrument.
//
// A row that resolves to no instrument id is an ERROR, not a skip. A price quietly
// dropped here is a price that never votes: the consensus silently shifts toward
// whoever remains, and nothing anywhere says a source went missing. The
// all-or-nothing refresh then keeps the last good projection, which is the correct
// degrade.
func (a *PriceAdapter) Prices(ctx context.Context) ([]pricing.Candidate, error) {
	rows, err := a.src.Fetch(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]pricing.Candidate, 0, len(rows))
	for _, row := range rows {
		id := a.resolver(RefRow{Symbol: row.Symbol, ISIN: row.ISIN, CUSIP: row.CUSIP, FIGI: row.FIGI})
		if id == "" {
			return nil, fmt.Errorf("feed: vendor %s quoted a price that keys to no instrument (symbol=%q isin=%q cusip=%q figi=%q)",
				a.src.Vendor(), row.Symbol, row.ISIN, row.CUSIP, row.FIGI)
		}
		if row.Price == nil {
			return nil, fmt.Errorf("feed: vendor %s row %s carries no price", a.src.Vendor(), id)
		}
		out = append(out, pricing.Candidate{
			InstrumentID: id,
			Source:       a.src.Vendor(),
			Price:        row.Price,
			AsOf:         row.AsOf,
		})
	}
	return out, nil
}

var _ VendorFeed = (*PriceAdapter)(nil)
