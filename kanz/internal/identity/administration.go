package identity

import (
	"errors"
	"slices"
	"time"
)

// AdminRole is reserved for identity administration, never a gateway capability.
const AdminRole = "kanz-identity-admin"

var (
	ErrAdminAuthority       = errors.New("identity: current administrator authority required")
	ErrSelfDisable          = errors.New("identity: self-disable is forbidden")
	ErrLastAdmin            = errors.New("identity: cannot disable the last active administrator")
	ErrAdminRoleCombination = errors.New("identity: administrator role may only accompany kanz-user")
)

// ValidateAdminRoles prevents provisioning a principal that combines identity
// administration with trading, approval, or infrastructure control. Bootstrap
// and redemption use the same rule, including invites created before rollout.
func ValidateAdminRoles(roles []string) error {
	if slices.Contains(roles, AdminRole) {
		for _, role := range roles {
			if role != AdminRole && role != "kanz-user" {
				return ErrAdminRoleCombination
			}
		}
	}
	return nil
}

// Administration is derived from a verified token, never from request JSON.
// The durable store rechecks it under the tenant administration lock.
type Administration struct {
	SessionEpoch int64
	Subject      string
	Tenant       string
	IssuedAt     time.Time
}

func (a Administration) Allows(u *User) bool {
	if a.Subject == "" || a.Tenant == "" || u == nil || u.Subject != a.Subject ||
		u.Tenant != a.Tenant || !u.Active() || !slices.Contains(u.Roles, AdminRole) ||
		ValidateAdminRoles(u.Roles) != nil || a.SessionEpoch != u.SessionEpoch {
		return false
	}
	if u.SessionEpoch > 0 {
		return !a.IssuedAt.IsZero()
	}
	return u.TokensInvalidBefore == nil || (!a.IssuedAt.IsZero() && a.IssuedAt.Unix() > u.TokensInvalidBefore.Unix())
}
