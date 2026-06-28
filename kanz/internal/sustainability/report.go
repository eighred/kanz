package sustainability

// CLIMATE-01d — net-zero alignment + TCFD/SFDR reporting. Temperature alignment
// and glide-path tracking measure whether the portfolio is on a net-zero pathway;
// BuildReport assembles the regulatory climate disclosures through the SAME
// template-driven, completeness-gated, signed pattern as the REG-01 reporting
// plane (an incomplete filing is never signed; the canonical bytes are signed
// through a Signer seam the AUDIT-01 hash-chain wires into).

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"
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

type field struct{ Code, Label string }

// templates lists the required line items per framework — a report is COMPLETE
// only when every code has a value (the REG-01 completeness discipline).
var templates = map[Framework][]field{
	TCFD: {
		{"TCFD_WACI", "Weighted-average carbon intensity"},
		{"TCFD_FINANCED_EMISSIONS", "Financed emissions (PCAF)"},
		{"TCFD_IMPLIED_TEMP_RISE", "Implied temperature rise"},
		{"TCFD_CLIMATE_VAR", "Climate value-at-risk"},
	},
	SFDR: {
		{"SFDR_GHG_INTENSITY", "GHG intensity of investments"},
		{"SFDR_CARBON_FOOTPRINT", "Carbon footprint"},
		{"SFDR_FOSSIL_FUEL_EXPOSURE", "Exposure to fossil-fuel companies"},
	},
}

// LineItem is one row of a report.
type LineItem struct {
	Code  string
	Label string
	Value float64
}

// Report is a point-in-time, signed climate disclosure.
type Report struct {
	Framework Framework
	AsOf      time.Time
	LineItems []LineItem
	Signature string
}

// Signer signs the canonical report bytes — the AUDIT-01 recorder seam. The
// default HashSigner is a SHA-256 digest; a deployment injects the audit
// hash-chain signer (shared discipline with REG-01 report signing).
type Signer interface {
	Sign(canonical []byte) string
}

// HashSigner is the default content-hash Signer (tamper-evidence without a key).
type HashSigner struct{}

// Sign returns the hex SHA-256 of the canonical bytes.
func (HashSigner) Sign(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// BuildReport assembles a framework's climate report from values (code → amount)
// as of asOf and signs it. It errors when the framework is unknown or any
// templated line item is missing — so an incomplete disclosure never gets signed.
// A nil signer defaults to HashSigner.
func BuildReport(framework Framework, asOf time.Time, values map[string]float64, signer Signer) (Report, error) {
	tmpl, ok := templates[framework]
	if !ok {
		return Report{}, fmt.Errorf("sustainability: unknown framework %q", framework)
	}
	if signer == nil {
		signer = HashSigner{}
	}
	items := make([]LineItem, 0, len(tmpl))
	for _, f := range tmpl {
		v, ok := values[f.Code]
		if !ok {
			return Report{}, fmt.Errorf("sustainability: %s report missing required line item %q", framework, f.Code)
		}
		items = append(items, LineItem{Code: f.Code, Label: f.Label, Value: v})
	}
	r := Report{Framework: framework, AsOf: asOf, LineItems: items}
	r.Signature = signer.Sign(r.canonical())
	return r, nil
}

// canonical renders the report to deterministic bytes for signing: framework, the
// RFC-3339 as-of, and the line items sorted by code — stable across runs so the
// signature is reproducible for a point-in-time re-derivation.
func (r Report) canonical() []byte {
	lines := make([]string, 0, len(r.LineItems)+2)
	lines = append(lines, string(r.Framework), r.AsOf.UTC().Format(time.RFC3339))
	codes := make([]string, len(r.LineItems))
	for i, li := range r.LineItems {
		codes[i] = li.Code + "=" + strconv.FormatFloat(li.Value, 'f', -1, 64)
	}
	sort.Strings(codes)
	lines = append(lines, codes...)
	var b []byte
	for _, l := range lines {
		b = append(b, l...)
		b = append(b, '\n')
	}
	return b
}

// Lookup returns a line item's value by code.
func (r Report) Lookup(code string) (float64, bool) {
	for _, li := range r.LineItems {
		if li.Code == code {
			return li.Value, true
		}
	}
	return 0, false
}
