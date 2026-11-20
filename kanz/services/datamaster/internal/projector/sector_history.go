package projector

import (
	"context"
	"errors"
	"time"

	"github.com/eighred/kanz/internal/refdata"
	"github.com/eighred/kanz/services/datamaster/internal/master"
	"github.com/eighred/kanz/services/datamaster/internal/store"
)

func (p *Projector) persistSectorHistory(ctx context.Context, dated map[string][]master.VendorRecord) error {
	if len(dated) == 0 {
		return nil
	}
	history, ok := p.golden.(store.SectorHistory)
	if !ok {
		return errors.New("datamaster: dated sector feeds require the durable golden store")
	}
	observations := make([]refdata.DatedSector, 0, len(dated))
	for _, key := range sortedKeys(dated) {
		group := dated[key]
		date := group[0].SectorEffectiveDate
		if _, err := time.Parse(time.DateOnly, date); err != nil {
			return err
		}
		byVendor := make(map[string]master.VendorRecord, len(group))
		for _, row := range group {
			if row.AsOf.IsZero() || row.AsOf.Nanosecond()%1000 != 0 || (row.Sector.Taxonomy == "") != (row.Sector.Code == "") {
				return errors.New("datamaster: incomplete dated sector source revision")
			}
			previous, seen := byVendor[row.Vendor]
			if seen && row.AsOf.Equal(previous.AsOf) && row.Sector != previous.Sector {
				return store.ErrSectorRevisionConflict
			}
			if !seen || row.AsOf.After(previous.AsOf) {
				byVendor[row.Vendor] = row
			}
		}
		// A cumulative drop may carry old revisions from the same vendor.
		// Resolve still falls through to a different authorized vendor for a
		// missing sector, while the latest revision replaces that vendor's row.
		current := make([]master.VendorRecord, 0, len(byVendor))
		for _, row := range byVendor {
			current = append(current, row)
		}
		resolved, _ := master.Resolve(current)
		observations = append(observations, refdata.DatedSector{
			InstrumentID: resolved.InstrumentID, EffectiveDate: date, SourceAsOf: resolved.AsOf,
			Taxonomy: resolved.Sector.Taxonomy, Code: resolved.Sector.Code, Name: resolved.Sector.Name,
			Vendor: resolved.Provenance["sector"],
		})
	}
	return history.PutSectorHistory(ctx, observations)
}
