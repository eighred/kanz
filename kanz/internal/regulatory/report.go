// Package regulatory is the REG-01 regulatory-capital & reporting root. report.go
// is the REG-01d template-driven, point-in-time, signed report generator: it
// assembles a framework's required line items from computed values and signs the
// canonical bytes through a Signer seam — the AUDIT-01 hash-chain recorder
// pattern, so a filing is tamper-evident and reconstructable as-of its date.
package regulatory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"time"
)

// Framework is a regulatory reporting regime.
type Framework string

const (
	FRTB   Framework = "FRTB"
	FormPF Framework = "FORM_PF"
	AIFMD  Framework = "AIFMD"
)

// field is one required line item in a framework template.
type field struct {
	Code  string
	Label string
}

// templates defines the required line items (in report order) per framework. A
// report is COMPLETE only when every templated code has a value — the REG-01e
// completeness property.
var templates = map[Framework][]field{
	FRTB: {
		{"FRTB_DELTA", "SBM delta charge"},
		{"FRTB_VEGA", "SBM vega charge"},
		{"FRTB_CURVATURE", "SBM curvature charge"},
		{"FRTB_DRC", "Default risk charge"},
		{"FRTB_RRAO", "Residual risk add-on"},
		{"FRTB_TOTAL", "Total FRTB capital"},
	},
	FormPF: {
		{"FORM_PF_GROSS_NAV", "Gross asset value"},
		{"FORM_PF_NET_NAV", "Net asset value"},
		{"FORM_PF_VAR", "Value-at-risk"},
		{"FORM_PF_GROSS_EXPOSURE", "Gross notional exposure"},
	},
	AIFMD: {
		{"AIFMD_AUM", "Assets under management"},
		{"AIFMD_LEVERAGE_GROSS", "Gross-method leverage"},
		{"AIFMD_LEVERAGE_COMMITMENT", "Commitment-method leverage"},
	},
}

// LineItem is one row of a report.
type LineItem struct {
	Code  string
	Label string
	Value float64
}

// Report is a point-in-time, signed regulatory filing.
type Report struct {
	Framework Framework
	AsOf      time.Time
	LineItems []LineItem
	Signature string
}

// Signer signs the canonical report bytes — the AUDIT-01 recorder seam. The
// default HashSigner is a SHA-256 digest; a deployment injects the audit hash-
// chain signer.
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

// BuildReport assembles the framework's report from values (code → amount) as of
// asOf and signs it. It errors when the framework is unknown or any templated
// line item is missing — so an incomplete filing never gets signed. A nil signer
// defaults to HashSigner.
func BuildReport(framework Framework, asOf time.Time, values map[string]float64, signer Signer) (Report, error) {
	tmpl, ok := templates[framework]
	if !ok {
		return Report{}, fmt.Errorf("regulatory: unknown framework %q", framework)
	}
	if signer == nil {
		signer = HashSigner{}
	}
	items := make([]LineItem, 0, len(tmpl))
	for _, f := range tmpl {
		v, ok := values[f.Code]
		if !ok {
			return Report{}, fmt.Errorf("regulatory: %s report missing required line item %q", framework, f.Code)
		}
		items = append(items, LineItem{Code: f.Code, Label: f.Label, Value: v})
	}
	r := Report{Framework: framework, AsOf: asOf, LineItems: items}
	r.Signature = signer.Sign(r.canonical())
	return r, nil
}

// canonical renders the report to deterministic bytes for signing: framework, the
// RFC-3339 as-of, and the line items sorted by code. Stable across runs so the
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
