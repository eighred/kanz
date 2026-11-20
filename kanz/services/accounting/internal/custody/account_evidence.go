package custody

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"time"
	"unicode/utf8"
)

// AccountEvidence is signed by an independent venue identity or custody assignment
// authority. The signature covers the original bytes, including the effective dates.
type AccountEvidence struct {
	Kind              string `json:"kind"`
	TenantID          string `json:"tenant_id"`
	Authority         string `json:"authority"`
	Venue             string `json:"venue,omitempty"`
	AccountID         string `json:"account_id"`
	ExchangeAccountID string `json:"exchange_account_id,omitempty"`
	ProofID           string `json:"proof_id"`
	Version           string `json:"version,omitempty"`
	Revision          int64  `json:"revision,omitempty"`
	Withdrawn         bool   `json:"withdrawn,omitempty"`
	PortfolioID       string `json:"portfolio_id,omitempty"`
	CustodianID       string `json:"custodian_id,omitempty"`
	EffectiveFrom     string `json:"effective_from"`
	EffectiveUntil    string `json:"effective_until"`
}

const AccountEvidenceLimit = 16 << 10

var errAccountEvidence = errors.New("custody: invalid signed account evidence")

// ParseAccountEvidence rejects ambiguous JSON before the issuer or scope is selected.
// Only positive, bounded, explicitly named attributes may identify a financial account.
func ParseAccountEvidence(payload []byte) (AccountEvidence, error) {
	var e AccountEvidence
	if len(payload) == 0 || len(payload) > AccountEvidenceLimit || !utf8.Valid(payload) {
		return e, errAccountEvidence
	}
	d := json.NewDecoder(bytes.NewReader(payload))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return e, errAccountEvidence
	}
	fields := map[string]*string{"kind": &e.Kind, "tenant_id": &e.TenantID, "authority": &e.Authority, "venue": &e.Venue, "account_id": &e.AccountID, "exchange_account_id": &e.ExchangeAccountID, "proof_id": &e.ProofID, "version": &e.Version, "portfolio_id": &e.PortfolioID, "custodian_id": &e.CustodianID, "effective_from": &e.EffectiveFrom, "effective_until": &e.EffectiveUntil}
	seen := make(map[string]bool, len(fields)+1)
	for d.More() {
		tok, err = d.Token()
		name, ok := tok.(string)
		if err != nil || !ok || seen[name] {
			return AccountEvidence{}, errAccountEvidence
		}
		seen[name] = true
		if name == "withdrawn" {
			var v *bool
			if d.Decode(&v) != nil || v == nil {
				return AccountEvidence{}, errAccountEvidence
			}
			e.Withdrawn = *v
			continue
		}
		if name == "revision" {
			if d.Decode(&e.Revision) != nil || e.Revision < 1 {
				return AccountEvidence{}, errAccountEvidence
			}
			continue
		}
		value, ok := fields[name]
		if !ok || d.Decode(value) != nil || !accountEvidenceID(*value) {
			return AccountEvidence{}, errAccountEvidence
		}
	}
	if tok, err = d.Token(); err != nil || tok != json.Delim('}') || d.Decode(new(json.RawMessage)) != io.EOF {
		return AccountEvidence{}, errAccountEvidence
	}
	required := []string{"kind", "tenant_id", "authority", "account_id", "proof_id", "effective_from", "effective_until"}
	switch e.Kind {
	case "venue_identity":
		required = append(required, "venue", "exchange_account_id")
	case "custody_assignment":
		required = append(required, "portfolio_id", "custodian_id", "version", "revision")
		if seen["withdrawn"] {
			required = append(required, "withdrawn")
		}
	default:
		return AccountEvidence{}, errAccountEvidence
	}
	if len(seen) != len(required) {
		return AccountEvidence{}, errAccountEvidence
	}
	for _, k := range required {
		if !seen[k] {
			return AccountEvidence{}, errAccountEvidence
		}
	}
	from, errFrom := time.Parse(time.DateOnly, e.EffectiveFrom)
	until, errUntil := time.Parse(time.DateOnly, e.EffectiveUntil)
	if errFrom != nil || errUntil != nil || from.Year() < 1 || !until.After(from) {
		return AccountEvidence{}, errAccountEvidence
	}
	return e, nil
}

func accountEvidenceID(value string) bool {
	if len(value) == 0 || len(value) > 256 {
		return false
	}
	for _, r := range value {
		if r < '!' || r > '~' {
			return false
		}
	}
	return true
}

// VerifyAccountEvidence independently binds the PG projection to signed content.
// An INSERT from an eligible database role alone cannot stand in for the signature.
func VerifyAccountEvidence(expected AccountEvidence, payload, signature, publicKey []byte) bool {
	if len(publicKey) != ed25519.PublicKeySize || len(signature) != ed25519.SignatureSize || len(payload) > AccountEvidenceLimit {
		return false
	}
	actual, err := ParseAccountEvidence(payload)
	return err == nil && actual == expected && ed25519.Verify(publicKey, payload, signature)
}
