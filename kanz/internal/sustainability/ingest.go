package sustainability

import (
	"context"
	"sort"
)

// PARITY-01f — ESG/carbon vendor ingestion. An ESG data vendor (MSCI /
// Sustainalytics / S&P Trucost) publishes per-issuer ESG ratings + GHG
// footprints; this normalizes the vendor's native rows into the analytics
// shapes (ESGScore + CarbonMetrics) and indexes them by instrument, the
// reference store the WACI / financed-emissions / exclusion aggregations join
// portfolio positions against. Only the live vendor API (ESGSource) is
// composition-root; the decode + the coverage accounting are vendor-SDK-free and
// tested here.
//
// Coverage discipline: a row with no EVIC is normalized faithfully (its ESG is
// still valid) and is NOT given an invented EVIC — the PCAF financed-emissions
// aggregation already SKIPS a no-EVIC holding (carbon.go), so the gap is
// surfaced as a Coverage metric rather than papered over.

// ESGRow is one vendor-native ESG/carbon row. Scores are 0–100 (higher better);
// emissions are tCO2e; Revenue/EVIC are money (the intensity / PCAF denominators).
type ESGRow struct {
	InstrumentID                               string
	Overall, Environmental, Social, Governance float64
	Scope1, Scope2, Scope3                     float64
	Revenue, EVIC                              float64
}

// IssuerESG is the normalized issuer datum — ESG + carbon, keyed by instrument.
// A portfolio position joins to it (by instrument) to form a Holding.
type IssuerESG struct {
	InstrumentID string
	ESG          ESGScore
	Carbon       CarbonMetrics
}

// ESGSource is the vendor ESG-feed transport seam (composition-root: the real
// MSCI/Sustainalytics/Trucost API).
type ESGSource interface {
	Vendor() string
	Fetch(ctx context.Context) ([]ESGRow, error)
}

// Index is the issuer→ESG store the carbon/ESG analytics read.
type Index struct {
	byID map[string]IssuerESG
}

// Ingest pulls the vendor's ESG rows and normalizes them into an Index. A row
// with no instrument id is skipped (it cannot be joined to a position).
func Ingest(ctx context.Context, src ESGSource) (*Index, error) {
	rows, err := src.Fetch(ctx)
	if err != nil {
		return nil, err
	}
	idx := &Index{byID: make(map[string]IssuerESG, len(rows))}
	for _, r := range rows {
		if r.InstrumentID == "" {
			continue
		}
		idx.byID[r.InstrumentID] = decodeESGRow(r)
	}
	return idx, nil
}

func decodeESGRow(r ESGRow) IssuerESG {
	return IssuerESG{
		InstrumentID: r.InstrumentID,
		ESG:          ESGScore{Overall: r.Overall, Environmental: r.Environmental, Social: r.Social, Governance: r.Governance},
		Carbon:       CarbonMetrics{Scope1: r.Scope1, Scope2: r.Scope2, Scope3: r.Scope3, Revenue: r.Revenue, EVIC: r.EVIC},
	}
}

// Lookup returns the issuer ESG for an instrument.
func (x *Index) Lookup(instrumentID string) (IssuerESG, bool) {
	v, ok := x.byID[instrumentID]
	return v, ok
}

// Coverage reports the ESG-data coverage of the index — the data-quality view an
// oversight desk watches. WithoutEVIC are the issuers the PCAF financed-emissions
// aggregation will skip (the no-EVIC coverage gap, surfaced not hidden).
type Coverage struct {
	Total       int
	WithEVIC    int
	WithoutEVIC []string // instrument ids lacking EVIC, sorted
}

// Coverage computes the coverage metric over the index.
func (x *Index) Coverage() Coverage {
	c := Coverage{Total: len(x.byID)}
	for id, v := range x.byID {
		if v.Carbon.EVIC > 0 {
			c.WithEVIC++
		} else {
			c.WithoutEVIC = append(c.WithoutEVIC, id)
		}
	}
	sort.Strings(c.WithoutEVIC)
	return c
}

// Holdings joins a position book (instrument → market value) to the ESG index,
// producing the Holdings the carbon/ESG aggregations run over. A position with
// no ESG coverage gets a zero issuer datum (it contributes nothing to WACI /
// financed emissions — the same degraded-to-zero discipline the analytics use)
// and is returned in `uncovered` so the gap is visible.
func (x *Index) Holdings(positions map[string]float64) (holdings []Holding, uncovered []string) {
	ids := make([]string, 0, len(positions))
	for id := range positions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		h := Holding{InstrumentID: id, MarketValue: positions[id]}
		if iss, ok := x.byID[id]; ok {
			h.ESG, h.Carbon = iss.ESG, iss.Carbon
		} else {
			uncovered = append(uncovered, id)
		}
		holdings = append(holdings, h)
	}
	return holdings, uncovered
}
