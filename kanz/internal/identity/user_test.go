package identity_test

// ACCOUNT STATUS (#525).
//
// Ungated — no database. Status.Validate is what keeps a bad status from
// reaching the column at all, and the reason it exists in Go rather than being
// left to the CHECK constraint is that the constraint's refusal is an opaque
// database error: a caller cannot tell it from a dead pool, so a mistyped status
// would be reported as an outage and retried instead of fixed.

import (
	"errors"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/identity"
)

func TestOnlyTheEnumeratedStatusesValidate(t *testing.T) {
	for _, s := range []identity.Status{identity.StatusActive, identity.StatusDisabled} {
		if err := identity.Status(s).Validate(); err != nil {
			t.Errorf("Validate(%q) = %v, want nil — this is a status the CHECK constraint admits, "+
				"so refusing it here makes a legitimate state unreachable", s, err)
		}
	}
}

// EVERY NEAR-MISS IS REFUSED, AND WITH AN ERROR THE CALLER CAN CLASSIFY.
//
// The empty string is the one that matters most: it is what an unset Status
// carries, so accepting it would let "nobody said" reach the column as a value.
func TestAnUnknownStatusIsRefusedAndIsIdentifiable(t *testing.T) {
	for _, s := range []identity.Status{"", "Disabled", "DISABLED", "disable", "suspended", "active "} {
		err := identity.Status(s).Validate()
		if err == nil {
			t.Errorf("Validate(%q) = nil — the store would then send it to Postgres and the caller "+
				"would be told the database is unwell rather than that there is no such status", s)
			continue
		}
		if !errors.Is(err, identity.ErrUnknownStatus) {
			t.Errorf("Validate(%q) = %v, which does not wrap ErrUnknownStatus — a handler cannot "+
				"tell a bad request from a store failure", s, err)
		}
	}
}

// THE ERROR NAMES THE ALTERNATIVES. "unknown status" alone leaves the caller
// guessing at the set, which on a two-member enum is a needless round trip.
func TestTheUnknownStatusErrorNamesTheKnownSet(t *testing.T) {
	err := identity.Status("suspended").Validate()
	if err == nil {
		t.Fatal("Validate accepted suspended")
	}
	for _, want := range []string{"suspended", string(identity.StatusActive), string(identity.StatusDisabled)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}
