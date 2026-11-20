// Package returnbasis retains signed Accounting close evidence for one UTC day.
package returnbasis

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
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
	Scope      string     `json:"scope"`
	Reference  string     `json:"reference"`
	EventCount *uint64    `json:"event_count"`
	Split      *FlowSplit `json:"split,omitempty"`
}

// FlowSplit names one booked external movement and the authoritative NAVs
// immediately before and after it, at the same effective instant.
type FlowSplit struct {
	MovementID    string    `json:"movement_id"`
	CommandDigest string    `json:"command_digest"`
	Effective     time.Time `json:"effective"`
	SignedAmount  dec.Exact `json:"signed_amount"`
	Before        *Close    `json:"before"`
	After         *Close    `json:"after"`
}

type Evidence struct {
	Format         string             `json:"format"`
	Tenant         string             `json:"tenant"`
	Portfolio      string             `json:"portfolio"`
	Currency       string             `json:"currency"`
	CloseDate      string             `json:"close_date"`
	AsOf           time.Time          `json:"as_of"`
	Issuer         string             `json:"issuer"`
	PreviousDigest string             `json:"previous_digest"`
	Opening        *Close             `json:"opening"`
	Closing        *Close             `json:"closing"`
	Flows          *FlowCompletion    `json:"external_flows"`
	Benchmark      *BenchmarkEvidence `json:"benchmark,omitempty"`
	PositionBasis  *PositionBasis     `json:"position_basis,omitempty"`
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
	marks := []*Close{e.Opening, e.Closing}
	if !validBenchmark(e) || !validPositionBasis(e) {
		return Evidence{}, ErrEvidence
	}
	if e.Flows != nil {
		if !identifier.MatchString(e.Flows.Scope) || !identifier.MatchString(e.Flows.Reference) || e.Flows.EventCount == nil {
			return Evidence{}, ErrEvidence
		}
		if split := e.Flows.Split; split != nil {
			if *e.Flows.EventCount != 1 || !validSplit(split, end) {
				return Evidence{}, ErrEvidence
			}
			marks = append(marks, split.Before, split.After)
		}
	}
	for _, close := range marks {
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
	return e, nil
}

func validSplit(split *FlowSplit, end time.Time) bool {
	if !identifier.MatchString(split.MovementID) || !hash.MatchString(split.CommandDigest) ||
		split.Effective.Location() != time.UTC || split.Effective.Nanosecond()%1000 != 0 ||
		!split.Effective.After(end.AddDate(0, 0, -1)) || !split.Effective.Before(end) {
		return false
	}
	amount, err := split.SignedAmount.Rat()
	if err != nil || amount.Sign() == 0 {
		return false
	}
	if _, ok := dec.ToProtoExact(amount); !ok {
		return false
	}
	if split.Before == nil || split.After == nil {
		return true
	}
	before, beforeErr := split.Before.Total.Rat()
	after, afterErr := split.After.Total.Rat()
	return beforeErr == nil && afterErr == nil && split.Before.Reference != split.After.Reference &&
		new(big.Rat).Sub(after, before).Cmp(amount) == 0
}

func (a authority) verifies(e Evidence, raw, signature []byte) bool {
	return a.issuer == e.Issuer && len(a.key) == ed25519.PublicKeySize && len(signature) == ed25519.SignatureSize &&
		(e.Flows == nil || e.Flows.Scope == a.flowScope) && ed25519.Verify(ed25519.PublicKey(a.key), raw, signature)
}

func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
