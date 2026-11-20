// Package returnbasis retains signed Accounting close evidence for one UTC day.
package returnbasis

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/eighred/kanz/internal/dec"
)

const Limit = 16 << 10
const Format = "kanz.accounting.return-basis.v1"

var (
	ErrEvidence = errors.New("return basis: invalid signed evidence")
	ErrConflict = errors.New("return basis: conflicting edition or predecessor")
	ErrNotFound = errors.New("return basis: evidence unavailable")
	identifier  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
	currency    = regexp.MustCompile(`^[A-Z]{3}$`)
	hash        = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Close's reference identifies the finalized Accounting NAV version, including
// the book, price and classification inputs used to produce its total.
type Close struct {
	Reference string    `json:"reference"`
	Total     dec.Exact `json:"total"`
}

// FlowCompletion is the signed issuer's affirmative conclusion for the whole
// enrolled cash-flow scope in (start,end]. Missing coverage is a null object.
type FlowCompletion struct {
	Scope      string  `json:"scope"`
	Reference  string  `json:"reference"`
	EventCount *uint64 `json:"event_count"`
}

type Evidence struct {
	Format         string          `json:"format"`
	Tenant         string          `json:"tenant"`
	Portfolio      string          `json:"portfolio"`
	Currency       string          `json:"currency"`
	CloseDate      string          `json:"close_date"`
	AsOf           time.Time       `json:"as_of"`
	Issuer         string          `json:"issuer"`
	PreviousDigest string          `json:"previous_digest"`
	Opening        *Close          `json:"opening"`
	Closing        *Close          `json:"closing"`
	Flows          *FlowCompletion `json:"external_flows"`
}

type authority struct {
	issuer, flowScope string
	key               []byte
}

func parse(raw []byte) (Evidence, error) {
	var e Evidence
	if len(raw) == 0 || len(raw) > Limit || json.Unmarshal(raw, &e) != nil {
		return Evidence{}, ErrEvidence
	}
	canonical, err := json.Marshal(e)
	if err != nil || !bytes.Equal(canonical, raw) {
		return Evidence{}, ErrEvidence
	}
	end, err := time.Parse(time.DateOnly, e.CloseDate)
	if err != nil || e.Format != Format || !identifier.MatchString(e.Tenant) || !identifier.MatchString(e.Portfolio) ||
		!identifier.MatchString(e.Issuer) || !currency.MatchString(e.Currency) || e.AsOf.IsZero() ||
		e.AsOf.Location() != time.UTC || e.AsOf.Before(end) || e.AsOf.Nanosecond()%1000 != 0 ||
		(e.PreviousDigest != "" && !hash.MatchString(e.PreviousDigest)) {
		return Evidence{}, ErrEvidence
	}
	for _, close := range []*Close{e.Opening, e.Closing} {
		if close == nil {
			continue
		}
		value, err := close.Total.Rat()
		if err != nil || !identifier.MatchString(close.Reference) {
			return Evidence{}, ErrEvidence
		}
		if _, ok := dec.ToProtoExact(value); !ok {
			return Evidence{}, ErrEvidence
		}
	}
	if e.Flows != nil && (!identifier.MatchString(e.Flows.Scope) || !identifier.MatchString(e.Flows.Reference) || e.Flows.EventCount == nil) {
		return Evidence{}, ErrEvidence
	}
	return e, nil
}

func (a authority) verifies(e Evidence, raw, signature []byte) bool {
	return a.issuer == e.Issuer && len(a.key) == ed25519.PublicKeySize && len(signature) == ed25519.SignatureSize &&
		(e.Flows == nil || e.Flows.Scope == a.flowScope) && ed25519.Verify(ed25519.PublicKey(a.key), raw, signature)
}

func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
