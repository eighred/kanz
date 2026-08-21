// Package filing is THE signed regulatory filing: one LineItem, one Report, one
// Signer seam, one canonical form, and ONE renderer.
//
// # Why it is one package (#633)
//
// It used to be two, and the copies were byte-identical: internal/regulatory
// (FRTB / Form PF / AIFMD) and internal/sustainability (TCFD / SFDR) each held
// their own LineItem, Report, Signer, HashSigner, BuildReport, canonical() and
// Lookup, serving ONE service. Nothing related them, so the property they both
// claimed survived in one of them and was lost in the other:
//
//   - THE RENDERING. Both packages' LineItem.MarshalJSON emitted dec.Str — and
//     neither was ever called. services/regulatory rendered the JSON body by hand
//     in two functions, and the climate one passed the *big.Rat straight to
//     encoding/json. big.Rat implements TextMarshaler as RatString(), so a WACI of
//     1234.5678 was FILED as "5429686605511341/4398046511104" while its signature
//     committed to "1234.5678". The recipient of a signed filing could not
//     re-derive the signature it carried, which is the only thing a signature is
//     for. An invariant living in a method nothing calls is not an invariant.
//
//   - THE NON-FINITE REFUSAL. regulatory.BuildReport refused a nil value — the way
//     a NaN/±Inf model output arrives at this boundary. sustainability.BuildReport
//     did not, so a non-finite climate metric was rendered "0" into the canonical
//     bytes and SIGNED. sustainability/disclosure.go's own comment said "a
//     non-finite metric maps to nil, which BuildReport refuses"; that had not been
//     true of the copy it called. A filing signed over a number nobody computed is
//     the #617 shape: authority the number does not deserve.
//
// Frameworks stay with their owners — internal/regulatory and
// internal/sustainability each keep their Framework type and their template
// table, because those ARE their domain. What they no longer keep is a second
// answer to "what does a filed number look like".
//
// internal/validation is deliberately NOT a consumer: its report is over
// benchmark Cases (name/got/want/tolerance), not line items, and its Signer seam
// returns no error. It shares the word "signed", not the concept.
package filing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/eighred/kanz/internal/dec"
)

// Field is one required line item in a framework's template: the code the
// regulator knows it by, and the human label.
type Field struct {
	Code  string
	Label string
}

// LineItem is one row of a filing.
//
// Value is an EXACT base-10 rational, not a float64. It used to be a float64, and
// on the money filings (Form PF gross/net NAV, AIFMD AUM) that meant the ledger's
// exact figure was rounded to the nearest binary double on its way into a filing.
// A filed NAV that does not reconcile to the book of record is a reportable
// discrepancy — `double` is banned on any path carrying money, and a regulatory
// filing is the last place to relax that. The climate METRICS behind a TCFD/SFDR
// row are model outputs and honestly float64 in the model; what is exact is the
// FILED number.
//
// The rational is exact; what gets FILED and what gets SIGNED are the same
// rendering of it (dec.Str), so the signature always commits to the number the
// regulator actually receives. That is enforced by construction: Canonical and
// MarshalJSON below are the only two renderings, and both call dec.Str.
type LineItem struct {
	Code  string
	Label string
	Value *big.Rat
}

// MarshalJSON emits the value as a decimal STRING, never a JSON number.
//
// A JSON float is exactly the ambiguity this type exists to remove: 0.1 is not 0.1
// in IEEE-754, and a regulator parsing our filing must not have to guess which
// double we meant. Marshalling the *big.Rat directly is worse still — its
// TextMarshaler is RatString(), which files "1/10".
//
// This method is REACHED: Report.Body puts the []LineItem into the response map,
// so encoding/json calls it. It was dead code in both predecessor packages, which
// is how the climate renderer lost the property (#633).
func (l LineItem) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Code  string `json:"code"`
		Label string `json:"label"`
		Value string `json:"value"`
	}{Code: l.Code, Label: l.Label, Value: dec.Str(l.Value)})
}

// Report is a point-in-time, signed filing.
type Report struct {
	// Framework is the regime's wire name ("FRTB", "TCFD", …). It is a plain
	// string here because the domain packages own the typed constants; this
	// package must not know the list.
	Framework string
	AsOf      time.Time
	LineItems []LineItem
	Signature string
}

// Signer signs the canonical report bytes — the AUDIT-01 recorder seam. The
// default HashSigner is a SHA-256 digest; a deployment injects the audit
// hash-chain signer.
type Signer interface {
	// Sign returns the signature, or an error if the report could not be recorded
	// in the durable audit chain — in which case it must NOT be issued.
	Sign(canonical []byte) (string, error)
}

// HashSigner is the default content-hash Signer (tamper-evidence without a key).
type HashSigner struct{}

// Sign returns the hex SHA-256 of the canonical bytes.
func (HashSigner) Sign(canonical []byte) (string, error) {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// Build assembles framework's report from template (in report order) and values
// (code → exact amount) as of asOf, and signs it.
//
// It errors when a templated line item is absent, and when one is present but
// nil — which is what a non-finite model output (NaN, ±Inf) becomes at this
// boundary, since big.Rat.SetFloat64 returns nil for it. An incomplete filing and
// a filing carrying a number that is not a number are both refused BEFORE the
// signature, because a signature is the one thing that makes a wrong number
// authoritative. A nil signer defaults to HashSigner.
func Build(framework string, template []Field, asOf time.Time, values map[string]*big.Rat, signer Signer) (Report, error) {
	if signer == nil {
		signer = HashSigner{}
	}
	items := make([]LineItem, 0, len(template))
	for _, f := range template {
		v, ok := values[f.Code]
		if !ok {
			return Report{}, fmt.Errorf("filing: %s report missing required line item %q — an incomplete filing is not signed", framework, f.Code)
		}
		if v == nil {
			// nil is how a non-finite float (NaN/±Inf) arrives here from a model. It
			// is not a number, so it is not a filing. Rendering it would file "0",
			// which reads as a real, measured zero.
			return Report{}, fmt.Errorf("filing: %s report line item %q is not a finite number", framework, f.Code)
		}
		// A VALUE TOO SMALL TO RENDER IS NOT A ZERO (#672).
		//
		// dec.Str renders at dec.Scale decimal places, so anything below 10^-Scale
		// comes out as "0" — and that string is what Body serves AND what Canonical
		// signs. The signature then commits to "we measured zero" over a number the
		// model reported as non-zero. It is the nil case one line up wearing a
		// different hat: there the value was not a number, here it is a number this
		// filing cannot express, and both would be FILED as a measured zero.
		//
		// REFUSED RATHER THAN WIDENED OR TOKENISED. Raising the scale moves the
		// boundary without removing it, and a non-numeric token ("<0.00000001") is a
		// wire-format change a regulator's parser has not agreed to. Refusing names
		// the item and its exact value, which is what an operator needs in order to
		// decide whether the figure is real or an artefact of an upstream unit.
		if v.Sign() != 0 && dec.Str(v) == "0" {
			return Report{}, fmt.Errorf("filing: %s report line item %q is %s, which renders as \"0\" "+
				"at the filing scale of %d decimal places — filing it would assert a measured zero and "+
				"sign that assertion", framework, f.Code, v.RatString(), dec.Scale)
		}
		items = append(items, LineItem{Code: f.Code, Label: f.Label, Value: v})
	}
	r := Report{Framework: framework, AsOf: asOf, LineItems: items}
	sig, err := signer.Sign(r.Canonical())
	if err != nil {
		// The filing could not be recorded in the audit chain. Do not hand back a
		// report claiming a chain position it does not have.
		return Report{}, fmt.Errorf("%s: %w", framework, err)
	}
	r.Signature = sig
	return r, nil
}

// Canonical renders the report to deterministic bytes for signing: framework, the
// RFC-3339 as-of, and the line items sorted by code. Stable across runs so the
// signature is reproducible for a point-in-time re-derivation.
//
// It is EXPORTED because the recipient of a filing has to be able to run it: the
// verification is "parse the served body, re-canonicalize, re-hash, compare".
// While it was unexported in both copies, nothing outside could perform that
// check — and nothing did, which is why the climate body could disagree with its
// own signature undetected (#633).
func (r Report) Canonical() []byte {
	lines := make([]string, 0, len(r.LineItems)+2)
	lines = append(lines, r.Framework, r.AsOf.UTC().Format(time.RFC3339))
	codes := make([]string, len(r.LineItems))
	for i, li := range r.LineItems {
		// dec.Str is the platform's canonical decimal rendering — the SAME string
		// LineItem.MarshalJSON emits, and therefore the same string the regulator
		// receives. Sign what you file.
		codes[i] = li.Code + "=" + dec.Str(li.Value)
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

// Body is THE JSON body of a filing — the one renderer.
//
// The line items go in as []LineItem, not as hand-built maps, so the response is
// produced by LineItem.MarshalJSON and cannot drift from Canonical without one of
// them being edited. The two hand-rolled renderers this replaces are the whole of
// #633: they were the same function written twice, and one of them had lost the
// decimal rendering.
//
// A caller may add framework-specific keys (an FRTB or AIFMD breakdown) to the
// returned map; it owns the map.
func (r Report) Body() map[string]any {
	return map[string]any{
		"framework":  r.Framework,
		"as_of":      r.AsOf.UTC().Format(time.RFC3339),
		"line_items": r.LineItems,
		"signature":  r.Signature,
	}
}

// Lookup returns a line item's exact value by code.
func (r Report) Lookup(code string) (*big.Rat, bool) {
	for _, li := range r.LineItems {
		if li.Code == code {
			return li.Value, true
		}
	}
	return nil, false
}
