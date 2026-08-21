package sustainability

// CLIMATE-01d — net-zero alignment + TCFD/SFDR reporting. Temperature alignment
// and glide-path tracking measure whether the portfolio is on a net-zero pathway;
// BuildReport assembles the regulatory climate disclosures through the SAME
// template-driven, completeness-gated, signed pattern as the REG-01 reporting
// plane (an incomplete filing is never signed; the canonical bytes are signed
// through a Signer seam the AUDIT-01 hash-chain wires into).

import (
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/eighred/kanz/internal/filing"
)

// GlidePath is a net-zero emissions trajectory: a linear decline from a base-year
// footprint to (near) zero at the target year — the path the portfolio's financed
// emissions must stay under to be Paris-aligned.
type GlidePath struct {
	BaseYear      int
	TargetYear    int
	BaseEmissions float64
	// ReductionFloor is the residual emissions fraction allowed at the target year
	// (e.g. 0.10 ⇒ a 90% cut to net-zero-with-offsets). Default 0.
	ReductionFloor float64
}

// Target returns the allowed financed emissions in a given year on the glide path
// — a linear interpolation from BaseEmissions at BaseYear to ReductionFloor·Base
// at TargetYear, clamped outside [BaseYear, TargetYear].
func (g GlidePath) Target(year int) float64 {
	if year <= g.BaseYear {
		return g.BaseEmissions
	}
	floor := g.ReductionFloor * g.BaseEmissions
	if year >= g.TargetYear {
		return floor
	}
	frac := float64(year-g.BaseYear) / float64(g.TargetYear-g.BaseYear)
	return g.BaseEmissions + frac*(floor-g.BaseEmissions)
}

// OnTrack reports whether the portfolio's actual financed emissions in year are at
// or below the glide-path target — the net-zero tracking signal. gap is actual −
// target (positive ⇒ over budget / off track).
func (g GlidePath) OnTrack(year int, actualEmissions float64) (onTrack bool, gap float64) {
	target := g.Target(year)
	gap = actualEmissions - target
	return gap <= 0, gap
}

// TemperatureAlignment estimates the portfolio's implied temperature rise from how
// far its emissions overshoot the glide path: an on-track portfolio aligns to the
// target (default 1.5°C); an overshoot adds warming proportional to the overshoot
// ratio scaled by a sensitivity. Simplified — a real ITR uses a carbon-budget
// model — but monotone in the overshoot, which is the property that matters for
// tracking.
func TemperatureAlignment(actualEmissions, targetEmissions, targetDegrees, sensitivity float64) (impliedRise float64, aligned bool) {
	if targetDegrees <= 0 {
		targetDegrees = 1.5
	}
	if targetEmissions <= 0 {
		// No budget defined: align iff there are effectively no emissions.
		return targetDegrees, actualEmissions <= 0
	}
	overshoot := (actualEmissions - targetEmissions) / targetEmissions
	if overshoot < 0 {
		overshoot = 0 // under budget caps the implied rise at the target
	}
	impliedRise = targetDegrees + sensitivity*overshoot
	return impliedRise, math.Abs(impliedRise-targetDegrees) < 1e-9
}

// --- TCFD / SFDR report (the REG-01 template-driven signed pattern) ----------

// Framework is a climate-disclosure regime.
type Framework string

const (
	TCFD Framework = "TCFD"
	SFDR Framework = "SFDR"
)

// templates lists the required line items per framework — a report is COMPLETE
// only when every code has a value (the REG-01 completeness discipline).
var templates = map[Framework][]filing.Field{
	TCFD: {
		{Code: "TCFD_WACI", Label: "Weighted-average carbon intensity"},
		{Code: "TCFD_FINANCED_EMISSIONS", Label: "Financed emissions (PCAF)"},
		{Code: "TCFD_IMPLIED_TEMP_RISE", Label: "Implied temperature rise"},
		{Code: "TCFD_CLIMATE_VAR", Label: "Climate value-at-risk"},
	},
	SFDR: {
		{Code: "SFDR_GHG_INTENSITY", Label: "GHG intensity of investments"},
		{Code: "SFDR_CARBON_FOOTPRINT", Label: "Carbon footprint"},
		{Code: "SFDR_FOSSIL_FUEL_EXPOSURE", Label: "Exposure to fossil-fuel companies"},
	},
}

// LineItem is one row of a filing — internal/filing's, not a second copy.
//
// The METRICS behind it are model outputs (emissions intensity, implied
// temperature rise, climate VaR) — float64 in the model, and honestly so: they
// are not exact base-10 quantities and no type can make them so. What is exact is
// the FILED number: what the regulator receives and what the signature commits to
// are one deterministic decimal string, which is a property of internal/filing and
// not of this package. It was NOT that while this package had its own copy: the
// climate filing was served in rational a/b form while its signature committed to
// the decimal (#633).
type LineItem = filing.LineItem

// Report is a point-in-time, signed climate disclosure.
type Report = filing.Report

// Signer signs the canonical report bytes — the AUDIT-01 recorder seam. The
// default HashSigner is a SHA-256 digest; a deployment injects the audit
// hash-chain signer (shared discipline with REG-01 report signing).
type Signer = filing.Signer

// HashSigner is the default content-hash Signer (tamper-evidence without a key).
type HashSigner = filing.HashSigner

// BuildReport assembles a framework's climate report from values (code → amount)
// as of asOf and signs it. It errors when the framework is unknown, when any
// templated line item is missing — so an incomplete disclosure never gets signed
// — and when a value is nil, which is what exactOrNil returns for a non-finite
// metric (NaN, ±Inf). That last refusal is new here: disclosure.go has always
// said "a non-finite metric maps to nil, which BuildReport refuses", and it was
// true of internal/regulatory's copy and false of this package's, so a NaN climate
// metric was filed as 0 and SIGNED (#633). A nil signer defaults to HashSigner.
func BuildReport(framework Framework, asOf time.Time, values map[string]*big.Rat, signer Signer) (Report, error) {
	tmpl, ok := templates[framework]
	if !ok {
		return Report{}, fmt.Errorf("sustainability: unknown framework %q", framework)
	}
	return filing.Build(string(framework), tmpl, asOf, values, signer)
}
