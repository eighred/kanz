// Package regulatory is the REG-01 regulatory-capital & reporting root. report.go
// is the REG-01d template-driven, point-in-time, signed report generator: it
// assembles a framework's required line items from computed values and signs the
// canonical bytes through a Signer seam — the AUDIT-01 hash-chain recorder
// pattern, so a filing is tamper-evident and reconstructable as-of its date.
package regulatory

import (
	"fmt"
	"math/big"
	"time"

	"github.com/eighred/kanz/internal/filing"
)

// Framework is a regulatory reporting regime.
type Framework string

const (
	FRTB   Framework = "FRTB"
	FormPF Framework = "FORM_PF"
	AIFMD  Framework = "AIFMD"
)

// templates defines the required line items (in report order) per framework. A
// report is COMPLETE only when every templated code has a value — the REG-01e
// completeness property.
var templates = map[Framework][]filing.Field{
	FRTB: {
		{Code: "FRTB_DELTA", Label: "SBM delta charge"},
		{Code: "FRTB_VEGA", Label: "SBM vega charge"},
		{Code: "FRTB_CURVATURE", Label: "SBM curvature charge"},
		{Code: "FRTB_DRC", Label: "Default risk charge"},
		{Code: "FRTB_RRAO", Label: "Residual risk add-on"},
		{Code: "FRTB_TOTAL", Label: "Total FRTB capital"},
	},
	FormPF: {
		{Code: "FORM_PF_GROSS_NAV", Label: "Gross asset value"},
		{Code: "FORM_PF_NET_NAV", Label: "Net asset value"},
		{Code: "FORM_PF_VAR", Label: "Value-at-risk"},
		{Code: "FORM_PF_GROSS_EXPOSURE", Label: "Gross notional exposure"},
	},
	AIFMD: {
		{Code: "AIFMD_AUM", Label: "Assets under management"},
		{Code: "AIFMD_LEVERAGE_GROSS", Label: "Gross-method leverage"},
		{Code: "AIFMD_LEVERAGE_COMMITMENT", Label: "Commitment-method leverage"},
	},
}

// LineItem is one row of a filing — internal/filing's, not a second copy. See
// that package for why: this one and internal/sustainability's were byte-identical
// and had already diverged where it mattered (#633).
type LineItem = filing.LineItem

// Report is a point-in-time, signed regulatory filing.
type Report = filing.Report

// Signer signs the canonical report bytes — the AUDIT-01 recorder seam. The
// default HashSigner is a SHA-256 digest; a deployment injects the audit hash-
// chain signer.
type Signer = filing.Signer

// HashSigner is the default content-hash Signer (tamper-evidence without a key).
type HashSigner = filing.HashSigner

// BuildReport assembles the framework's report from values (code → exact amount)
// as of asOf and signs it. It errors when the framework is unknown, when any
// templated line item is missing — so an incomplete filing never gets signed —
// and when a value is nil, which is what a non-finite model output (NaN, ±Inf)
// becomes at this boundary. A capital charge that is not a number must not be
// filed as one. A nil signer defaults to HashSigner.
func BuildReport(framework Framework, asOf time.Time, values map[string]*big.Rat, signer Signer) (Report, error) {
	tmpl, ok := templates[framework]
	if !ok {
		return Report{}, fmt.Errorf("regulatory: unknown framework %q", framework)
	}
	return filing.Build(string(framework), tmpl, asOf, values, signer)
}
