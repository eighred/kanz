package feed

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kanz-eng/kanz/internal/dec"
)

func drop(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The reference extract decodes into survivorship candidates, keyed on the
// canonical id the resolver derives.
func TestFileRefSourceDecodesAVendorExtract(t *testing.T) {
	path := drop(t, "reference.csv", strings.Join([]string{
		"symbol,isin,cusip,figi,asset_class,currency,description,as_of",
		"AAPL,US0378331005,037833100,BBG000B9XRY4,EQUITY,USD,Apple Inc,2026-07-12T00:00:00Z",
		"MSFT,US5949181045,594918104,BBG000BPH459,EQUITY,USD,Microsoft Corp,2026-07-12T00:00:00Z",
	}, "\n")+"\n")

	src, err := NewFileRefSource("BLOOMBERG", 0, path)
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewReferenceAdapter(src, nil)
	recs, err := adapter.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("decoded %d records, want 2", len(recs))
	}
	// DefaultIDResolver prefers FIGI — the most stable cross-vendor identifier.
	if recs[0].InstrumentID != "BBG000B9XRY4" {
		t.Errorf("instrument id = %q, want the FIGI", recs[0].InstrumentID)
	}
	if recs[0].Vendor != "BLOOMBERG" || recs[0].Priority != 0 {
		t.Errorf("provenance lost: vendor=%q priority=%d", recs[0].Vendor, recs[0].Priority)
	}
	if recs[0].Description != "Apple Inc" || recs[0].CurrencyCode != "USD" {
		t.Errorf("record = %+v", recs[0])
	}
	if recs[0].AsOf.IsZero() {
		t.Error("as_of was not decoded")
	}
}

// Columns are matched by NAME, not position: a vendor that adds a column, reorders
// them, or ships them in caps must not silently shift every field by one.
func TestFileRefSourceIsIndifferentToColumnOrder(t *testing.T) {
	path := drop(t, "reference.csv", "CURRENCY,DESCRIPTION,UNKNOWN_NEW_COLUMN,FIGI\nUSD,Apple Inc,ignore me,BBG000B9XRY4\n")
	src, _ := NewFileRefSource("ICE", 1, path)
	recs, err := NewReferenceAdapter(src, nil).Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].InstrumentID != "BBG000B9XRY4" || recs[0].CurrencyCode != "USD" || recs[0].Description != "Apple Inc" {
		t.Fatalf("column-name binding failed: %+v", recs)
	}
}

// A row that keys to no instrument FAILS the delivery. It must never be quietly
// dropped: a silently absent security is one that answers "instrument not found"
// to somebody who holds it.
func TestFileRefSourceRefusesAnUnkeyableRow(t *testing.T) {
	path := drop(t, "reference.csv", "isin,currency,description\n,USD,A security with no identifier at all\n")
	src, _ := NewFileRefSource("BLOOMBERG", 0, path)
	_, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("a row with no identifier was accepted — the security would vanish from the master with no trace")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("the error must name the offending line, got: %v", err)
	}
}

// A missing file is not an empty extract. "The vendor sent nothing today" and "we
// could not read what the vendor sent" are different facts, and conflating them
// would master the book from the remaining vendors as if this one had legitimately
// gone silent.
func TestFileRefSourceRefusesAMissingDrop(t *testing.T) {
	src, _ := NewFileRefSource("BLOOMBERG", 0, filepath.Join(t.TempDir(), "not-delivered.csv"))
	if _, err := src.Fetch(context.Background()); err == nil {
		t.Fatal("a missing vendor drop was read as an empty extract")
	}
}

// The price extract becomes exact-decimal arbitration candidates, keyed to the same
// instrument the reference feed mastered.
func TestFilePriceSourceDecodesQuotes(t *testing.T) {
	path := drop(t, "prices.csv", strings.Join([]string{
		"figi,price,as_of",
		"BBG000B9XRY4,201.4567,2026-07-12T16:00:00Z",
	}, "\n")+"\n")
	src, err := NewFilePriceSource("BLOOMBERG", path)
	if err != nil {
		t.Fatal(err)
	}
	cands, err := NewPriceAdapter(src, nil).Prices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("decoded %d candidates, want 1", len(cands))
	}
	c := cands[0]
	if c.InstrumentID != "BBG000B9XRY4" || c.Source != "BLOOMBERG" {
		t.Errorf("candidate keyed wrong: %+v", c)
	}
	// The vendor quoted a decimal. It stays a decimal — no double, anywhere.
	if c.Price.Cmp(dec.Rat("201.4567")) != 0 {
		t.Errorf("price = %v, want exactly 201.4567", c.Price)
	}
	if !c.AsOf.Equal(time.Date(2026, 7, 12, 16, 0, 0, 0, time.UTC)) {
		t.Errorf("as_of = %v", c.AsOf)
	}
	// A price feed masters nothing — the security master comes from the reference
	// feeds, not from whatever a price file happens to mention.
	if recs, _ := NewPriceAdapter(src, nil).Records(context.Background()); recs != nil {
		t.Error("a price feed supplied reference records")
	}
}

// Every way a price file can lie, and the refusal for each.
func TestFilePriceSourceRefusesUnusableQuotes(t *testing.T) {
	cases := map[string]string{
		"price is not a number":  "figi,price,as_of\nBBG000B9XRY4,about a hundred,2026-07-12T16:00:00Z\n",
		"price is zero":          "figi,price,as_of\nBBG000B9XRY4,0,2026-07-12T16:00:00Z\n",
		"price is negative":      "figi,price,as_of\nBBG000B9XRY4,-5,2026-07-12T16:00:00Z\n",
		"no price at all":        "figi,price,as_of\nBBG000B9XRY4,,2026-07-12T16:00:00Z\n",
		"no as_of to age it by":  "figi,price\nBBG000B9XRY4,201.45\n",
		"as_of is not RFC3339":   "figi,price,as_of\nBBG000B9XRY4,201.45,12/07/2026\n",
		"quoted against nothing": "figi,price,as_of\n,201.45,2026-07-12T16:00:00Z\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			src, _ := NewFilePriceSource("BLOOMBERG", drop(t, "prices.csv", body))
			if _, err := src.Fetch(context.Background()); err == nil {
				t.Fatalf("accepted a quote that %s — it would vote in a consensus mark", name)
			}
		})
	}
}

// A zero price is the one that matters most: admitted, it drags the median of every
// honest source toward zero on an instrument somebody holds.
func TestAZeroPriceNeverVotes(t *testing.T) {
	src, _ := NewFilePriceSource("BROKEN", drop(t, "prices.csv", "figi,price,as_of\nBBG000B9XRY4,0.00,2026-07-12T16:00:00Z\n"))
	if _, err := NewPriceAdapter(src, nil).Prices(context.Background()); err == nil {
		t.Fatal("a zero quote was admitted as a price")
	}
}
