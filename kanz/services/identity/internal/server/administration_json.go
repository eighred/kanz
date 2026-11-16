package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/eighred/kanz/internal/identity"
)

// revisionJSON accepts only a canonical nonnegative int64 decimal string.
// A JSON number may already have been rounded by the browser before submission.
type revisionJSON int64

// The reviewed command is exactly one bounded JSON object, without a second
// value that a proxy/client could interpret differently.
func decodeAdministration(w http.ResponseWriter, r *http.Request, into any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return false
	}
	return true
}

func (v *revisionJSON) UnmarshalJSON(raw []byte) error {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil || len(text) == 0 || len(text) > 19 {
		return errors.New("revision must be a decimal string")
	}
	for i, digit := range text {
		if digit < '0' || digit > '9' || i == 0 && len(text) > 1 && digit == '0' {
			return errors.New("revision must be a canonical nonnegative decimal string")
		}
	}
	parsed, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return errors.New("revision exceeds the signed 64-bit domain")
	}
	*v = revisionJSON(parsed)
	return nil
}

// These are HTTP projections, separate from the domain projections serialized
// into audit evidence. Session epochs stay int64 in signed token/state contracts.
type accessJSON struct {
	Subject      string          `json:"subject"`
	Tenant       string          `json:"tenant"`
	Roles        []string        `json:"roles"`
	Portfolios   []string        `json:"portfolios"`
	Status       identity.Status `json:"status"`
	Revision     string          `json:"revision"`
	SessionEpoch string          `json:"session_epoch"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
	CreatedBy    string          `json:"created_by"`
}

func accessResponse(a *identity.Access) accessJSON {
	return accessJSON{Subject: a.Subject, Tenant: a.Tenant, Roles: a.Roles, Portfolios: a.Portfolios,
		Status: a.Status, Revision: strconv.FormatInt(a.Revision, 10), SessionEpoch: strconv.FormatInt(a.SessionEpoch, 10),
		CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt, CreatedBy: a.CreatedBy}
}

type invitationJSON struct {
	ID         string     `json:"invite_id"`
	Subject    string     `json:"subject"`
	Tenant     string     `json:"tenant"`
	Roles      []string   `json:"roles"`
	Portfolios []string   `json:"portfolios"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	RedeemedAt *time.Time `json:"redeemed_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	RevokedBy  string     `json:"revoked_by,omitempty"`
	ReissuedAs *string    `json:"reissued_as,omitempty"`
	Revision   string     `json:"revision"`
	State      string     `json:"state"`
	Redeemable bool       `json:"redeemable"`
}

func invitationResponse(a identity.InvitationSummary) invitationJSON {
	return invitationJSON{ID: a.ID, Subject: a.Subject, Tenant: a.Tenant, Roles: a.Roles, Portfolios: a.Portfolios,
		CreatedBy: a.CreatedBy, CreatedAt: a.CreatedAt, ExpiresAt: a.ExpiresAt, RedeemedAt: a.RedeemedAt,
		RevokedAt: a.RevokedAt, RevokedBy: a.RevokedBy, ReissuedAs: a.ReissuedAs,
		Revision: strconv.FormatInt(a.Revision, 10), State: a.State, Redeemable: a.Redeemable}
}
