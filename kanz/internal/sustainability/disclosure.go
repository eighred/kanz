package sustainability

import (
	"fmt"
	"math/big"
	"strings"
	"time"
)

// TCFD/SFDR disclosures end-to-end (PARITY-06c / CLIMATE-01d). The package ships
// the climate METRICS as isolated functions — WACI, PCAF financed emissions,
// temperature alignment, climate VaR; this is the assembler that drives them from
// ONE live holdings book + net-zero pathway into the signed, completeness-gated
// disclosure, reusing the REG-01 BuildReport signing path (no parallel report
// machinery). The metrics are computed from the joined ESG/carbon holdings the
// PARITY-01f ingest produced, so the disclosure is live, not stubbed.

// DisclosureInputs is the live input set for a climate disclosure: the joined
// holdings, the net-zero glide path + disclosure year (for temperature
// alignment), the climate scenario (for climate VaR), and the fossil-fuel
// instrument flags (SFDR PAI). TempSensitivity scales the implied-temperature
// overshoot; 0 leaves TemperatureAlignment's default.
type DisclosureInputs struct {
	Holdings        []Holding
	GlidePath       GlidePath
	Year            int
	Scenario        ClimateScenario
	TempSensitivity float64
	FossilFuel      map[string]bool
}

// Disclosure line-item codes for the data-coverage record (#618). They are
// TEMPLATED FIELDS, not an optional annex: internal/filing refuses a report with a
// templated item missing, so a WACI can no longer be signed without the share of
// the book it was computed from being signed beside it.
const (
	tcfdIntensityCoverage   = "TCFD_WACI_DATA_COVERAGE"
	tcfdAttributionCoverage = "TCFD_EMISSIONS_DATA_COVERAGE"
	sfdrIntensityCoverage   = "SFDR_GHG_INTENSITY_DATA_COVERAGE"
	sfdrAttributionCoverage = "SFDR_EMISSIONS_DATA_COVERAGE"
)

// TCFDValues computes the four TCFD line items from the live holdings + pathway,
// plus the two data-coverage items that qualify them. The implied temperature rise
// measures the portfolio's financed emissions against its glide-path target for the
// disclosure year; climate VaR runs the supplied NGFS scenario over the book.
//
// It returns an error where it used to return only a map: a book carrying value
// but no carbon reference data at all is REFUSED. See unmeasured.
func (in DisclosureInputs) TCFDValues() (map[string]*big.Rat, error) {
	waci, intensityCov := WeightedAverageCarbonIntensity(in.Holdings)
	financed, attributionCov := FinancedEmissions(in.Holdings)
	target := in.GlidePath.Target(in.Year)
	impliedRise, _ := TemperatureAlignment(financed, target, 0, in.TempSensitivity)
	// The climate-VaR coverage is the attribution coverage again — the same EVIC
	// gap, already carried. Taking it twice would file the identical number under
	// two codes and invite them to drift.
	climateVaR, _ := in.Scenario.ClimateVaR(in.Holdings)
	if err := unmeasured(TCFD, intensityCov, attributionCov); err != nil {
		return nil, err
	}
	// The metrics come out of a climate model as float64 and stay honest about
	// that; exactOrNil captures each double's TRUE value so nothing further is lost
	// between the model and the regulator, and the filed number is deterministic and
	// signable. A non-finite metric maps to nil, which BuildReport refuses.
	return map[string]*big.Rat{
		"TCFD_WACI":               exactOrNil(waci),
		"TCFD_FINANCED_EMISSIONS": exactOrNil(financed),
		"TCFD_IMPLIED_TEMP_RISE":  exactOrNil(impliedRise),
		"TCFD_CLIMATE_VAR":        exactOrNil(climateVaR),
		tcfdIntensityCoverage:     exactOrNil(intensityCov.Fraction()),
		tcfdAttributionCoverage:   exactOrNil(attributionCov.Fraction()),
	}, nil
}

// SFDRValues computes the three SFDR principal-adverse-impact line items and the
// two coverage items behind them. Carbon footprint is the PCAF financed emissions
// normalized per $m invested; fossil-fuel exposure is the market-value share of
// holdings flagged as fossil-fuel companies.
//
// FOSSIL-FUEL EXPOSURE HAS NO COVERAGE ITEM, and the omission is not an oversight.
// The other metrics read vendor reference data that is present or absent per
// holding; this one reads DisclosureInputs.FossilFuel, a caller-supplied map whose
// absent key means "not a fossil-fuel company" by construction. There is no
// "unknown" for this package to count. That map having no third state is a real
// gap, one layer out, and not this one.
func (in DisclosureInputs) SFDRValues() (map[string]*big.Rat, error) {
	ghgIntensity, intensityCov := WeightedAverageCarbonIntensity(in.Holdings)
	financed, attributionCov := FinancedEmissions(in.Holdings)
	total := totalValue(in.Holdings)

	var footprint float64
	if total > 0 {
		footprint = financed / (total / 1_000_000) // tCO2e per $m invested
	}

	var fossil float64
	if total > 0 {
		for _, h := range in.Holdings {
			if in.FossilFuel[h.InstrumentID] {
				fossil += abs(h.MarketValue)
			}
		}
		fossil = fossil / total // fraction of NAV in fossil-fuel companies
	}
	if err := unmeasured(SFDR, intensityCov, attributionCov); err != nil {
		return nil, err
	}
	return map[string]*big.Rat{
		"SFDR_GHG_INTENSITY":        exactOrNil(ghgIntensity),
		"SFDR_CARBON_FOOTPRINT":     exactOrNil(footprint),
		"SFDR_FOSSIL_FUEL_EXPOSURE": exactOrNil(fossil),
		sfdrIntensityCoverage:       exactOrNil(intensityCov.Fraction()),
		sfdrAttributionCoverage:     exactOrNil(attributionCov.Fraction()),
	}, nil
}

// unmeasured refuses a filing whose metrics rest on nothing.
//
// A ZERO COVERAGE IS NOT A LOW NUMBER. With value in the book and the datum for
// none of it, WACI is 0 and financed emissions are 0 — and those zeros would be
// rendered, filed and SIGNED exactly like a genuinely decarbonized portfolio's.
// This is the same refusal internal/filing already makes for a non-finite metric
// (#633) and for a value too small to render at the filing scale (#672), for the
// same stated reason: a signature is what makes a wrong number authoritative.
//
// PARTIAL coverage is NOT refused. Vendor ESG data is never complete, and a
// platform that filed only on perfect coverage would file nothing; the coverage
// line item is what makes a partial figure honest. Only the total absence, which
// is a broken join rather than a data gap, stops the filing.
//
// A coverage so small it renders as "0" at the filing scale is refused too, by
// #672's rule one layer down — a disclosure whose coverage rounds to zero is one
// whose numbers mean nothing, so the two refusals agree.
func unmeasured(framework Framework, covs ...Coverage) error {
	for _, c := range covs {
		if !c.Unmeasured() {
			continue
		}
		return fmt.Errorf("sustainability: %s disclosure cannot be filed — no holding carries %s "+
			"data, so every metric derived from it would be filed and signed as a measured zero "+
			"(%d holdings covering %.0f of market value, none with %s; e.g. %s)",
			framework, c.Datum, c.UncoveredCount, c.TotalValue, c.Datum, strings.Join(c.Uncovered, ", "))
	}
	return nil
}

// FileTCFD assembles the signed, completeness-gated TCFD disclosure as of asOf
// from the live inputs. A nil signer defaults to HashSigner; a deployment injects
// the AUDIT-01 hash-chain signer.
func FileTCFD(in DisclosureInputs, asOf time.Time, signer Signer) (Report, error) {
	values, err := in.TCFDValues()
	if err != nil {
		return Report{}, err
	}
	return BuildReport(TCFD, asOf, values, signer)
}

// FileSFDR assembles the signed, completeness-gated SFDR disclosure as of asOf.
func FileSFDR(in DisclosureInputs, asOf time.Time, signer Signer) (Report, error) {
	values, err := in.SFDRValues()
	if err != nil {
		return Report{}, err
	}
	return BuildReport(SFDR, asOf, values, signer)
}

// exactOrNil converts a model float to its exact rational value, or nil if it is
// not a finite number (SetFloat64 returns nil for NaN/±Inf). A metric that is not
// a number must not be filed as one.
func exactOrNil(f float64) *big.Rat {
	return new(big.Rat).SetFloat64(f)
}
