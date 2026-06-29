// Package feed is the data master's vendor-integration layer (MASTER-01c): a
// pluggable VendorFeed seam (Bloomberg/Refinitiv/ICE) whose records and prices
// normalize onto the platform's canonical reference.v1 / market.v1 schemas. The
// seam mirrors the OMS-01c execution.Venue and POST-01c SettlementVenue stance —
// a log/sim default that needs no external dependency, with the real vendor
// adapter wired at the composition root (DEBT-02). Normalizing here is what makes
// master.v1 the resolution layer in FRONT of reference.v1, not a parallel one.
package feed

import (
	"context"
	"math"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	marketpb "github.com/kanz-eng/kanz-schemas-go/market/v1"
	referencepb "github.com/kanz-eng/kanz-schemas-go/reference/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/services/datamaster/internal/master"
	"github.com/kanz-eng/kanz/services/datamaster/internal/pricing"
)

// VendorFeed is a pluggable vendor data source. The SimFeed default serves canned
// records with no external dependency; a Bloomberg/Refinitiv/ICE adapter
// implements the same interface and wires at the composition root.
type VendorFeed interface {
	// Vendor is the source name ("BLOOMBERG", "REFINITIV", "ICE").
	Vendor() string
	// Records returns the vendor's reference records (survivorship candidates).
	Records(ctx context.Context) ([]master.VendorRecord, error)
	// Prices returns the vendor's price candidates.
	Prices(ctx context.Context) ([]pricing.Candidate, error)
}

// SimFeed is the dependency-free default VendorFeed — it replays canned records
// and prices, so the whole resolve→arbitrate path runs deterministically with no
// vendor connection (the SimVenue stance).
type SimFeed struct {
	Name       string
	VRecords   []master.VendorRecord
	Candidates []pricing.Candidate
}

func (f SimFeed) Vendor() string { return f.Name }

func (f SimFeed) Records(context.Context) ([]master.VendorRecord, error) { return f.VRecords, nil }

func (f SimFeed) Prices(context.Context) ([]pricing.Candidate, error) { return f.Candidates, nil }

// priceExponent is the fixed scale for a normalized price Decimal (4 dp).
const priceExponent = -4

// NormalizeReference maps a resolved golden record onto a reference.v1
// InstrumentReference — the canonical instrument-master shape the analytics plane
// reads. The string asset class resolves to the reference enum (unknown ⇒
// UNSPECIFIED).
func NormalizeReference(sm master.SecurityMaster) *referencepb.InstrumentReference {
	out := &referencepb.InstrumentReference{
		InstrumentId: sm.InstrumentID,
		Identifiers: &referencepb.InstrumentIdentifiers{
			Isin:            sm.Identifiers.ISIN,
			Cusip:           sm.Identifiers.CUSIP,
			Sedol:           sm.Identifiers.SEDOL,
			Figi:            sm.Identifiers.FIGI,
			Ric:             sm.Identifiers.RIC,
			BloombergTicker: sm.Identifiers.BloombergTicker,
		},
		AssetClass:   assetClass(sm.AssetClass),
		CurrencyCode: sm.CurrencyCode,
		Description:  sm.Description,
	}
	if sm.Sector.Taxonomy != "" || sm.Sector.Code != "" {
		out.Sector = &referencepb.SectorClassification{
			Taxonomy: sm.Sector.Taxonomy,
			Code:     sm.Sector.Code,
			Name:     sm.Sector.Name,
		}
	}
	if !sm.AsOf.IsZero() {
		out.AsOf = timestamppb.New(sm.AsOf)
	}
	return out
}

// NormalizePrice maps an arbitrated consensus price onto a market.v1
// MarketDataEvent as a degenerate Quote (bid = ask = the consensus mark) — the
// canonical hot-path shape, so a mastered price flows to consumers like any
// other market quote. No consensus (all-stale / missing) ⇒ nil.
func NormalizePrice(a pricing.Arbitration, asOf time.Time) *marketpb.MarketDataEvent {
	if !a.HasPrice {
		return nil
	}
	p := floatToDecimal(a.Chosen, priceExponent)
	ev := &marketpb.MarketDataEvent{
		InstrumentId: a.InstrumentID,
		Mic:          "COMPOSITE",
		Data: &marketpb.MarketDataEvent_Quote{
			Quote: &marketpb.Quote{BidPrice: p, AskPrice: p},
		},
	}
	if !asOf.IsZero() {
		ev.EventTime = timestamppb.New(asOf)
	}
	return ev
}

// assetClass resolves a string asset class ("EQUITY") to the reference enum.
func assetClass(s string) referencepb.AssetClass {
	if v, ok := referencepb.AssetClass_value["ASSET_CLASS_"+s]; ok {
		return referencepb.AssetClass(v)
	}
	return referencepb.AssetClass_ASSET_CLASS_UNSPECIFIED
}

// floatToDecimal builds a common.v1.Decimal at the given exponent (value =
// coefficient·10^exponent), rounding the coefficient to nearest.
func floatToDecimal(f float64, exp int32) *commonpb.Decimal {
	scale := math.Pow(10, float64(-exp))
	return &commonpb.Decimal{Coefficient: int64(math.Round(f * scale)), Exponent: exp}
}
