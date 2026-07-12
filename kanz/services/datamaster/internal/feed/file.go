// Vendor file-drop transport — the real RefSource / PriceSource this service was
// missing (DATA-M8c).
//
// # Why a file, and not an HTTP client
//
// This is how institutional reference data is actually delivered. Bloomberg Data
// License and Refinitiv DataScope Select publish scheduled flat-file extracts to
// SFTP; the fund mounts the drop directory and reads it. It is not the fallback
// for a "real" API — for a security master it IS the real integration, and it is
// the one an ops team can operate: the file that produced today's master is still
// on disk tomorrow, which is exactly what an auditor asks for.
//
// It is also the only transport that can be VERIFIED end to end without a
// commercial account, and an adapter nobody can execute is an adapter nobody
// should trust. A vendor with a REST reference API implements the same two seams
// (RefSource / PriceSource) beside this one; nothing downstream changes.
//
// # This is not a simulator
//
// The data comes from outside the process. The source invents nothing, defaults
// nothing, and skips nothing — a row it cannot decode fails the whole fetch, which
// (via the projector's all-or-nothing rule) leaves the last good projection in
// place. A half-read vendor file must never become a security master.
package feed

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"
	"time"
)

// FileRefSource reads a vendor's reference extract (CSV) from a mounted drop.
type FileRefSource struct {
	vendor   string
	priority int
	path     string
}

// NewFileRefSource returns a reference source over one vendor's file. Priority is
// the vendor's survivorship trust rank — LOWER WINS (0 = most trusted).
func NewFileRefSource(vendor string, priority int, path string) (*FileRefSource, error) {
	if strings.TrimSpace(vendor) == "" || strings.TrimSpace(path) == "" {
		return nil, errors.New("feed: a file reference source needs a vendor and a path")
	}
	if priority < 0 {
		return nil, fmt.Errorf("feed: vendor %s has a negative survivorship priority", vendor)
	}
	return &FileRefSource{vendor: vendor, priority: priority, path: path}, nil
}

func (s *FileRefSource) Vendor() string { return s.vendor }
func (s *FileRefSource) Priority() int  { return s.priority }

// Fetch reads the drop and decodes every row.
//
// The file is re-read on every cycle, so a fresh delivery is picked up without a
// restart. A missing file is an error, NOT an empty extract: "the vendor sent us
// nothing today" and "we could not read what the vendor sent" are different facts,
// and treating the second as the first would master the book from the remaining
// vendors as though this one had legitimately gone silent.
func (s *FileRefSource) Fetch(ctx context.Context) ([]RefRow, error) {
	recs, cols, err := readCSV(ctx, s.path)
	if err != nil {
		return nil, fmt.Errorf("vendor %s: %w", s.vendor, err)
	}
	out := make([]RefRow, 0, len(recs))
	for i, rec := range recs {
		line := i + 2 // 1-based, past the header
		row := RefRow{
			Symbol:          cols.get(rec, "symbol"),
			ISIN:            cols.get(rec, "isin"),
			CUSIP:           cols.get(rec, "cusip"),
			SEDOL:           cols.get(rec, "sedol"),
			FIGI:            cols.get(rec, "figi"),
			RIC:             cols.get(rec, "ric"),
			BloombergTicker: cols.get(rec, "bloomberg_ticker"),
			AssetClass:      cols.get(rec, "asset_class"),
			SectorTaxonomy:  cols.get(rec, "sector_taxonomy"),
			SectorCode:      cols.get(rec, "sector_code"),
			SectorName:      cols.get(rec, "sector_name"),
			Currency:        cols.get(rec, "currency"),
			Description:     cols.get(rec, "description"),
		}
		// A row that keys to no instrument cannot be mastered. ReferenceAdapter
		// would silently skip it; here, at the file boundary, we can name the exact
		// line — so we refuse the delivery instead of quietly dropping a security
		// that somebody may hold.
		if DefaultIDResolver(row) == "" {
			return nil, fmt.Errorf("vendor %s: %s line %d carries no identifier (need one of figi/isin/cusip/symbol)", s.vendor, s.path, line)
		}
		if row.AsOf, err = optionalTime(cols.get(rec, "as_of")); err != nil {
			return nil, fmt.Errorf("vendor %s: %s line %d: as_of: %w", s.vendor, s.path, line, err)
		}
		out = append(out, row)
	}
	return out, nil
}

// FilePriceSource reads a vendor's price extract (CSV) from a mounted drop.
type FilePriceSource struct {
	vendor string
	path   string
}

// NewFilePriceSource returns a price source over one vendor's file.
func NewFilePriceSource(vendor, path string) (*FilePriceSource, error) {
	if strings.TrimSpace(vendor) == "" || strings.TrimSpace(path) == "" {
		return nil, errors.New("feed: a file price source needs a vendor and a path")
	}
	return &FilePriceSource{vendor: vendor, path: path}, nil
}

func (s *FilePriceSource) Vendor() string { return s.vendor }

// Fetch reads the drop and decodes every quote.
//
// price and as_of are BOTH required. A price with no timestamp cannot be aged, so
// the staleness check that keeps a dead quote out of the consensus would silently
// pass it; a price that will not parse as an exact decimal is not a price. Either
// one fails the delivery rather than being guessed at.
func (s *FilePriceSource) Fetch(ctx context.Context) ([]PriceRow, error) {
	recs, cols, err := readCSV(ctx, s.path)
	if err != nil {
		return nil, fmt.Errorf("vendor %s: %w", s.vendor, err)
	}
	out := make([]PriceRow, 0, len(recs))
	for i, rec := range recs {
		line := i + 2
		row := PriceRow{
			Symbol: cols.get(rec, "symbol"),
			ISIN:   cols.get(rec, "isin"),
			CUSIP:  cols.get(rec, "cusip"),
			FIGI:   cols.get(rec, "figi"),
		}
		if DefaultIDResolver(RefRow{Symbol: row.Symbol, ISIN: row.ISIN, CUSIP: row.CUSIP, FIGI: row.FIGI}) == "" {
			return nil, fmt.Errorf("vendor %s: %s line %d quotes a price against no identifier", s.vendor, s.path, line)
		}
		raw := cols.get(rec, "price")
		if raw == "" {
			return nil, fmt.Errorf("vendor %s: %s line %d has no price", s.vendor, s.path, line)
		}
		price, ok := new(big.Rat).SetString(raw)
		if !ok {
			return nil, fmt.Errorf("vendor %s: %s line %d: price %q is not an exact decimal", s.vendor, s.path, line, raw)
		}
		// A non-positive price is not a quote. Admitting one would drag the median
		// of every other source toward zero on an instrument somebody holds.
		if price.Sign() <= 0 {
			return nil, fmt.Errorf("vendor %s: %s line %d: price %q is not positive", s.vendor, s.path, line, raw)
		}
		row.Price = price

		asOf := cols.get(rec, "as_of")
		if asOf == "" {
			return nil, fmt.Errorf("vendor %s: %s line %d: a price with no as_of cannot be aged for staleness", s.vendor, s.path, line)
		}
		if row.AsOf, err = optionalTime(asOf); err != nil {
			return nil, fmt.Errorf("vendor %s: %s line %d: as_of: %w", s.vendor, s.path, line, err)
		}
		out = append(out, row)
	}
	return out, nil
}

// columns maps a header name to its position. Lookup is case-insensitive and
// order-free: a vendor may add columns, reorder them, or ship them in caps, and an
// extract that gains a column we do not read must not break the master.
type columns map[string]int

func (c columns) get(rec []string, name string) string {
	i, ok := c[name]
	if !ok || i >= len(rec) {
		return ""
	}
	return strings.TrimSpace(rec[i])
}

// readCSV opens the drop and returns its data rows plus the header index.
func readCSV(ctx context.Context, path string) ([]([]string), columns, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	f, err := os.Open(path) //#nosec G304 -- an operator-configured vendor drop path
	if err != nil {
		return nil, nil, fmt.Errorf("open extract: %w", err)
	}
	defer func() { _ = f.Close() }()

	r := csv.NewReader(f)
	r.TrimLeadingSpace = true
	r.FieldsPerRecord = -1 // a short trailing field is the header's problem, not a panic

	head, err := r.Read()
	if errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("%s is empty: a vendor extract must at least carry its header", path)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read header of %s: %w", path, err)
	}
	cols := columns{}
	for i, h := range head {
		cols[strings.ToLower(strings.TrimSpace(h))] = i
	}

	recs, err := r.ReadAll()
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	return recs, cols, nil
}

// optionalTime parses an RFC3339 timestamp; empty is the zero time.
func optionalTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not an RFC3339 timestamp", s)
	}
	return t, nil
}

var (
	_ RefSource   = (*FileRefSource)(nil)
	_ PriceSource = (*FilePriceSource)(nil)
)
