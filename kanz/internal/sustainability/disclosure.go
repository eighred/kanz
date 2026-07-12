package sustainability

import "math/big"

import "time"

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

// TCFDValues computes the four TCFD line items from the live holdings + pathway.
// The implied temperature rise measures the portfolio's financed emissions against
// its glide-path target for the disclosure year; climate VaR runs the supplied
// NGFS scenario over the book.
func (in DisclosureInputs) TCFDValues() map[string]*big.Rat {
	waci := WeightedAverageCarbonIntensity(in.Holdings)
	financed := FinancedEmissions(in.Holdings)
	target := in.GlidePath.Target(in.Year)
	impliedRise, _ := TemperatureAlignment(financed, target, 0, in.TempSensitivity)
	climateVaR := in.Scenario.ClimateVaR(in.Holdings)
	// The metrics come out of a climate model as float64 and stay honest about
	// that; exactOrNil captures each double's TRUE value so nothing further is lost
	// between the model and the regulator, and the filed number is deterministic and
	// signable. A non-finite metric maps to nil, which BuildReport refuses.
	return map[string]*big.Rat{
		"TCFD_WACI":               exactOrNil(waci),
		"TCFD_FINANCED_EMISSIONS": exactOrNil(financed),
		"TCFD_IMPLIED_TEMP_RISE":  exactOrNil(impliedRise),
		"TCFD_CLIMATE_VAR":        exactOrNil(climateVaR),
	}
}

// SFDRValues computes the three SFDR principal-adverse-impact line items. Carbon
// footprint is the PCAF financed emissions normalized per $m invested; fossil-fuel
// exposure is the market-value share of holdings flagged as fossil-fuel companies.
func (in DisclosureInputs) SFDRValues() map[string]*big.Rat {
	ghgIntensity := WeightedAverageCarbonIntensity(in.Holdings)
	financed := FinancedEmissions(in.Holdings)
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
	return map[string]*big.Rat{
		"SFDR_GHG_INTENSITY":        exactOrNil(ghgIntensity),
		"SFDR_CARBON_FOOTPRINT":     exactOrNil(footprint),
		"SFDR_FOSSIL_FUEL_EXPOSURE": exactOrNil(fossil),
	}
}

// FileTCFD assembles the signed, completeness-gated TCFD disclosure as of asOf
// from the live inputs. A nil signer defaults to HashSigner; a deployment injects
// the AUDIT-01 hash-chain signer.
func FileTCFD(in DisclosureInputs, asOf time.Time, signer Signer) (Report, error) {
	return BuildReport(TCFD, asOf, in.TCFDValues(), signer)
}

// FileSFDR assembles the signed, completeness-gated SFDR disclosure as of asOf.
func FileSFDR(in DisclosureInputs, asOf time.Time, signer Signer) (Report, error) {
	return BuildReport(SFDR, asOf, in.SFDRValues(), signer)
}

// exactOrNil converts a model float to its exact rational value, or nil if it is
// not a finite number (SetFloat64 returns nil for NaN/±Inf). A metric that is not
// a number must not be filed as one.
func exactOrNil(f float64) *big.Rat {
	return new(big.Rat).SetFloat64(f)
}
