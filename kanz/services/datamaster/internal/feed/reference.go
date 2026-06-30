package feed

import (
	"context"
	"time"

	"github.com/kanz-eng/kanz/services/datamaster/internal/master"
	"github.com/kanz-eng/kanz/services/datamaster/internal/pricing"
)

// PARITY-01e — reference-data vendor adapter. The MASTER service resolves a
// golden security record across vendors' reference feeds (survivorship +
// ISIN/CUSIP/FIGI crosswalk). This wires a real vendor reference feed
// (Bloomberg / Refinitiv reference API) into the VendorFeed contract by
// normalizing the vendor's native reference rows into master.VendorRecords. Only
// the live vendor API (RefSource) is composition-root; the decode + the
// instrument_id resolution are vendor-SDK-free and tested here. Retires the
// datamaster SimFeed for the reference path.

// RefRow is one vendor-native reference row — the identifiers + classification a
// reference feed publishes for an instrument. The vendor adapter decodes its
// native row format into this shape.
type RefRow struct {
	Symbol                                         string // the vendor's display symbol (advisory)
	ISIN, CUSIP, SEDOL, FIGI, RIC, BloombergTicker string
	AssetClass                                     string // "EQUITY", "FIXED_INCOME", ... (reference.v1 vocabulary, sans prefix)
	SectorTaxonomy, SectorCode, SectorName         string
	Currency                                       string
	Description                                    string
	AsOf                                           time.Time
}

// RefSource is the vendor reference-feed transport seam. The real Bloomberg /
// Refinitiv reference API implements it at the composition root; Priority is the
// vendor's survivorship trust rank (lower wins, master.VendorRecord.Priority).
type RefSource interface {
	Vendor() string
	Priority() int
	Fetch(ctx context.Context) ([]RefRow, error)
}

// IDResolver assigns the canonical instrument_id for a vendor row. The default
// (DefaultIDResolver) prefers the most stable cross-vendor identifier
// (FIGI → ISIN → CUSIP → Symbol); a deployment with a Kanz-minted id space
// supplies its own.
type IDResolver func(RefRow) string

// DefaultIDResolver keys the canonical id on the most stable identifier present.
func DefaultIDResolver(r RefRow) string {
	switch {
	case r.FIGI != "":
		return r.FIGI
	case r.ISIN != "":
		return r.ISIN
	case r.CUSIP != "":
		return r.CUSIP
	default:
		return r.Symbol
	}
}

// ReferenceAdapter implements VendorFeed over a reference RefSource. It is a pure
// reference feed — Prices returns nil (price arbitration is fed by the market-
// data price adapters, not the reference feed).
type ReferenceAdapter struct {
	src      RefSource
	resolver IDResolver
}

// NewReferenceAdapter builds the adapter over a vendor reference source. A nil
// resolver uses DefaultIDResolver.
func NewReferenceAdapter(src RefSource, resolver IDResolver) *ReferenceAdapter {
	if resolver == nil {
		resolver = DefaultIDResolver
	}
	return &ReferenceAdapter{src: src, resolver: resolver}
}

// Vendor returns the source vendor name.
func (a *ReferenceAdapter) Vendor() string { return a.src.Vendor() }

// Records fetches the vendor's reference rows and normalizes each into a
// master.VendorRecord (the survivorship candidate). A row that resolves to no
// canonical id is skipped — it cannot be mastered without one.
func (a *ReferenceAdapter) Records(ctx context.Context) ([]master.VendorRecord, error) {
	rows, err := a.src.Fetch(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]master.VendorRecord, 0, len(rows))
	for _, row := range rows {
		id := a.resolver(row)
		if id == "" {
			continue
		}
		out = append(out, a.decode(row, id))
	}
	return out, nil
}

// Prices returns nil — a reference feed carries no price candidates.
func (a *ReferenceAdapter) Prices(context.Context) ([]pricing.Candidate, error) { return nil, nil }

func (a *ReferenceAdapter) decode(row RefRow, instrumentID string) master.VendorRecord {
	return master.VendorRecord{
		Vendor:       a.src.Vendor(),
		InstrumentID: instrumentID,
		Identifiers: master.Identifiers{
			ISIN: row.ISIN, CUSIP: row.CUSIP, SEDOL: row.SEDOL,
			FIGI: row.FIGI, RIC: row.RIC, BloombergTicker: row.BloombergTicker,
		},
		AssetClass:   row.AssetClass,
		Sector:       master.Sector{Taxonomy: row.SectorTaxonomy, Code: row.SectorCode, Name: row.SectorName},
		CurrencyCode: row.Currency,
		Description:  row.Description,
		AsOf:         row.AsOf,
		Priority:     a.src.Priority(),
	}
}

var _ VendorFeed = (*ReferenceAdapter)(nil)
