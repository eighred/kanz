// Package master is the golden-source security master (MASTER-01b): it resolves a
// single GOLDEN RECORD for an instrument across many vendors' records by
// survivorship rules, and builds the cross-vendor identifier crosswalk
// (ISIN/CUSIP/FIGI → canonical instrument_id). Reads are point-in-time-correct —
// a query as-of a past date sees the record that vendors reported then, not
// today's.
//
// It carries its own Go-native shapes (the master.v1 SDK is generated-not-
// committed, EVT-15a); the feed layer (MASTER-01c) maps these onto
// reference.v1.InstrumentReference at the boundary, so the core stays decoupled
// from the wire SDK and trivially testable.
package master

import (
	"fmt"
	"sort"
	"time"
)

// Scheme is a cross-vendor identifier scheme (mirrors master.v1.IdentifierScheme).
type Scheme string

const (
	SchemeISIN  Scheme = "ISIN"
	SchemeCUSIP Scheme = "CUSIP"
	SchemeSEDOL Scheme = "SEDOL"
	SchemeFIGI  Scheme = "FIGI"
	SchemeRIC   Scheme = "RIC"
)

// schemes is the crosswalk's resolution order — deterministic iteration.
var schemes = []Scheme{SchemeISIN, SchemeCUSIP, SchemeSEDOL, SchemeFIGI, SchemeRIC}

// Identifiers holds an instrument's external identifiers (mirrors
// reference.v1.InstrumentIdentifiers). An empty field means the scheme does not
// apply / the vendor did not report it.
type Identifiers struct {
	ISIN, CUSIP, SEDOL, FIGI, RIC, BloombergTicker string
}

// get returns the value for a scheme.
func (id Identifiers) get(s Scheme) string {
	switch s {
	case SchemeISIN:
		return id.ISIN
	case SchemeCUSIP:
		return id.CUSIP
	case SchemeSEDOL:
		return id.SEDOL
	case SchemeFIGI:
		return id.FIGI
	case SchemeRIC:
		return id.RIC
	}
	return ""
}

func (id *Identifiers) set(s Scheme, v string) {
	switch s {
	case SchemeISIN:
		id.ISIN = v
	case SchemeCUSIP:
		id.CUSIP = v
	case SchemeSEDOL:
		id.SEDOL = v
	case SchemeFIGI:
		id.FIGI = v
	case SchemeRIC:
		id.RIC = v
	}
}

// Sector mirrors reference.v1.SectorClassification.
type Sector struct {
	Taxonomy, Code, Name string
}

func (s Sector) empty() bool { return s.Taxonomy == "" && s.Code == "" }

// VendorRecord is one vendor's view of an instrument — a survivorship candidate.
type VendorRecord struct {
	Vendor       string
	InstrumentID string
	Identifiers  Identifiers
	AssetClass   string
	Sector       Sector
	CurrencyCode string
	Description  string
	AsOf         time.Time
	// Priority is the vendor's trust rank: LOWER wins (0 = most trusted).
	Priority int
}

// SecurityMaster is the resolved golden record with field-level provenance.
type SecurityMaster struct {
	InstrumentID string
	Identifiers  Identifiers
	AssetClass   string
	Sector       Sector
	CurrencyCode string
	Description  string
	AsOf         time.Time
	// Provenance maps a resolved field name to the vendor that won it.
	Provenance map[string]string
}

// IdentifierConflict is two vendors reporting different non-empty values for the
// same identifier scheme on one instrument — a crosswalk integrity break the
// pricing oversight queue surfaces as a DataException.
type IdentifierConflict struct {
	InstrumentID string
	Scheme       Scheme
	Values       []string // the distinct conflicting values, sorted
}

func (c IdentifierConflict) Error() string {
	return fmt.Sprintf("identifier conflict on %s/%s: %v", c.InstrumentID, c.Scheme, c.Values)
}

// rank orders records by survivorship precedence: lowest priority first, then
// most-recent as_of (recency breaks a priority tie), then vendor name for total
// determinism.
func rank(records []VendorRecord) []VendorRecord {
	out := append([]VendorRecord(nil), records...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		if !out[i].AsOf.Equal(out[j].AsOf) {
			return out[i].AsOf.After(out[j].AsOf)
		}
		return out[i].Vendor < out[j].Vendor
	})
	return out
}

// Resolve assembles the golden record for one instrument from its vendor records
// by survivorship: each field is taken from the highest-trust vendor that reports
// a non-empty value (recency breaks a priority tie). Identifiers are the union
// across vendors; a scheme reported with two different values yields an
// IdentifierConflict (the survivor — highest trust — is still chosen). The
// record's as_of is the latest vendor as_of. Records must share one instrument_id
// (the caller groups by it); an empty slice yields a zero record.
func Resolve(records []VendorRecord) (SecurityMaster, []IdentifierConflict) {
	if len(records) == 0 {
		return SecurityMaster{}, nil
	}
	ranked := rank(records)
	sm := SecurityMaster{
		InstrumentID: ranked[0].InstrumentID,
		Provenance:   map[string]string{},
	}

	// Scalar survivorship: first non-empty in ranked order wins.
	for _, r := range ranked {
		if sm.AssetClass == "" && r.AssetClass != "" {
			sm.AssetClass, sm.Provenance["asset_class"] = r.AssetClass, r.Vendor
		}
		if sm.CurrencyCode == "" && r.CurrencyCode != "" {
			sm.CurrencyCode, sm.Provenance["currency_code"] = r.CurrencyCode, r.Vendor
		}
		if sm.Description == "" && r.Description != "" {
			sm.Description, sm.Provenance["description"] = r.Description, r.Vendor
		}
		if sm.Sector.empty() && !r.Sector.empty() {
			sm.Sector, sm.Provenance["sector"] = r.Sector, r.Vendor
		}
		if r.AsOf.After(sm.AsOf) {
			sm.AsOf = r.AsOf
		}
	}

	// Identifier survivorship + conflict detection, per scheme.
	var conflicts []IdentifierConflict
	for _, s := range schemes {
		seen := map[string]struct{}{}
		var distinct []string
		winner := ""
		for _, r := range ranked {
			v := r.Identifiers.get(s)
			if v == "" {
				continue
			}
			if winner == "" {
				winner = v
				sm.Provenance["identifier."+string(s)] = r.Vendor
			}
			if _, ok := seen[v]; !ok {
				seen[v] = struct{}{}
				distinct = append(distinct, v)
			}
		}
		if winner != "" {
			sm.Identifiers.set(s, winner)
		}
		if len(distinct) > 1 {
			sort.Strings(distinct)
			conflicts = append(conflicts, IdentifierConflict{InstrumentID: sm.InstrumentID, Scheme: s, Values: distinct})
		}
	}
	return sm, conflicts
}

// ResolveAsOf is the point-in-time read: it resolves over only the records
// effective at or before asOf, so a query as-of a past date reproduces the record
// vendors reported then. A zero asOf means "now" (all records).
func ResolveAsOf(records []VendorRecord, asOf time.Time) (SecurityMaster, []IdentifierConflict) {
	if asOf.IsZero() {
		return Resolve(records)
	}
	var visible []VendorRecord
	for _, r := range records {
		if !r.AsOf.After(asOf) {
			visible = append(visible, r)
		}
	}
	return Resolve(visible)
}

// Crosswalk resolves an external identifier (scheme + value) to the canonical
// instrument_id — the cross-vendor lookup the platform keys joins on.
type Crosswalk struct {
	byKey map[string]string
}

func key(s Scheme, v string) string { return string(s) + "|" + v }

// BuildCrosswalk indexes every (scheme, value) → instrument_id across the records.
// A value mapping to two different instrument_ids is a crosswalk conflict (the
// first-seen mapping is kept; the conflict is reported so the oversight queue can
// raise it).
func BuildCrosswalk(records []VendorRecord) (*Crosswalk, []IdentifierConflict) {
	x := &Crosswalk{byKey: map[string]string{}}
	var conflicts []IdentifierConflict
	for _, r := range rank(records) {
		for _, s := range schemes {
			v := r.Identifiers.get(s)
			if v == "" {
				continue
			}
			k := key(s, v)
			if existing, ok := x.byKey[k]; ok {
				if existing != r.InstrumentID {
					conflicts = append(conflicts, IdentifierConflict{
						InstrumentID: existing,
						Scheme:       s,
						Values:       []string{existing, r.InstrumentID},
					})
				}
				continue
			}
			x.byKey[k] = r.InstrumentID
		}
	}
	return x, conflicts
}

// Lookup resolves a scheme/value to its canonical instrument_id.
func (x *Crosswalk) Lookup(s Scheme, value string) (string, bool) {
	id, ok := x.byKey[key(s, value)]
	return id, ok
}
