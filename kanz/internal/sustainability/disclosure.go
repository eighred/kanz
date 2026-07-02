package sustainability

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
func (in DisclosureInputs) TCFDValues() map[string]float64 {
	waci := WeightedAverageCarbonIntensity(in.Holdings)
	financed := FinancedEmissions(in.Holdings)
	target := in.GlidePath.Target(in.Year)
	impliedRise, _ := TemperatureAlignment(financed, target, 0, in.TempSensitivity)
	climateVaR := in.Scenario.ClimateVaR(in.Holdings)
	return map[string]float64{
		"TCFD_WACI":               waci,
		"TCFD_FINANCED_EMISSIONS": financed,
		"TCFD_IMPLIED_TEMP_RISE":  impliedRise,
		"TCFD_CLIMATE_VAR":        climateVaR,
	}
}

// SFDRValues computes the three SFDR principal-adverse-impact line items. Carbon
// footprint is the PCAF financed emissions normalized per $m invested; fossil-fuel
// exposure is the market-value share of holdings flagged as fossil-fuel companies.
func (in DisclosureInputs) SFDRValues() map[string]float64 {
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
	return map[string]float64{
		"SFDR_GHG_INTENSITY":        ghgIntensity,
		"SFDR_CARBON_FOOTPRINT":     footprint,
		"SFDR_FOSSIL_FUEL_EXPOSURE": fossil,
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
