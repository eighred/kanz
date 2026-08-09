package identity

// THE INVITE IS THE PROVISIONING BOUNDARY (#99).
//
// It is where "an operator decides your authority" is enforced rather than
// stated. Every test here is a rule that, if it stopped holding, would turn
// provisioning back into registration without anything failing.

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

func validInvite(t *testing.T) *Invite {
	t.Helper()
	_, hash, err := NewInviteToken()
	if err != nil {
		t.Fatalf("NewInviteToken: %v", err)
	}
	inv, err := NewInvite("inv-1", hash, "user:alice", "acme",
		[]string{"kanz-trader"}, []string{"pf-1"}, "user:operator", t0, 0)
	if err != nil {
		t.Fatalf("NewInvite: %v", err)
	}
	return inv
}

// THE RAW TOKEN IS NEVER RECOVERABLE FROM WHAT IS STORED.
func TestTheStoredFormDoesNotContainTheToken(t *testing.T) {
	raw, hash, err := NewInviteToken()
	if err != nil {
		t.Fatalf("NewInviteToken: %v", err)
	}
	if raw == "" || hash == "" {
		t.Fatal("empty token or hash")
	}
	if strings.Contains(hash, raw) || hash == raw {
		t.Fatal("the stored hash contains the raw token — a stolen database yields usable " +
			"invites, each carrying whatever authority an operator attached to it")
	}
	if InviteTokenHash(raw) != hash {
		t.Fatal("hashing the raw token does not reproduce the stored hash — redemption could " +
			"never find the row")
	}
}

// TWO TOKENS ARE NEVER THE SAME.
func TestEveryInviteTokenIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 16; i++ {
		raw, _, err := NewInviteToken()
		if err != nil {
			t.Fatalf("NewInviteToken: %v", err)
		}
		if seen[raw] {
			t.Fatal("a token repeated — one invite would redeem another's account")
		}
		seen[raw] = true
	}
}

// SINGLE USE, AND EXPIRING.
func TestAnInviteIsSingleUseAndExpires(t *testing.T) {
	inv := validInvite(t)

	if err := inv.Redeemable(t0); err != nil {
		t.Fatalf("a fresh invite is not redeemable: %v", err)
	}
	if err := inv.Redeemable(inv.ExpiresAt); err != ErrInviteExpired {
		t.Errorf("at exactly ExpiresAt: got %v, want ErrInviteExpired — an invite valid AT its "+
			"expiry is valid for one more instant than the operator chose", err)
	}
	if err := inv.Redeemable(inv.ExpiresAt.Add(time.Hour)); err != ErrInviteExpired {
		t.Errorf("after expiry: got %v, want ErrInviteExpired", err)
	}

	used := t0.Add(time.Minute)
	inv.RedeemedAt = &used
	if err := inv.Redeemable(t0.Add(2 * time.Minute)); err != ErrInviteAlreadyRedeemed {
		t.Errorf("after redemption: got %v, want ErrInviteAlreadyRedeemed.\n\n"+
			"A redeemed invite that still works is a shared password with an expiry date.", err)
	}
}

// THE AUTHORITY IS FIXED AT CREATION, BY THE OPERATOR.
//
// This is the rule that makes it provisioning. The invite carries tenant, roles
// and portfolios; redemption supplies only a credential. If these were ever
// taken from the redeeming caller, whoever held the link would choose their own
// authority — the exact failure self-service signup has on a platform where a
// role moves capital.
func TestTheInviteCarriesTheAuthority(t *testing.T) {
	inv := validInvite(t)

	if inv.Tenant != "acme" || len(inv.Roles) != 1 || inv.Roles[0] != "kanz-trader" {
		t.Fatalf("invite = tenant %q roles %v, want acme / [kanz-trader]", inv.Tenant, inv.Roles)
	}
	if inv.CreatedBy != "user:operator" {
		t.Errorf("CreatedBy = %q — 'who granted this person trade authority' is the first "+
			"question after an incident", inv.CreatedBy)
	}

	// The slices are copied, not aliased: an operator's slice mutated after the
	// call must not silently re-grant.
	roles := []string{"kanz-user"}
	inv2, err := NewInvite("inv-2", "h", "user:bob", "acme", roles, nil, "user:operator", t0, 0)
	if err != nil {
		t.Fatalf("NewInvite: %v", err)
	}
	roles[0] = "kanz-operator"
	if inv2.Roles[0] != "kanz-user" {
		t.Fatal("the invite aliases the caller's roles slice — mutating it after creation " +
			"silently changes the authority already granted")
	}
}

// AN INVITE THAT GRANTS NOTHING, OR CANNOT BE AUDITED, IS REFUSED AT CREATION.
func TestAnUnusableInviteIsRefused(t *testing.T) {
	for name, mk := range map[string]func() (*Invite, error){
		"no id":      func() (*Invite, error) { return NewInvite("", "h", "s", "acme", []string{"r"}, nil, "op", t0, 0) },
		"no token":   func() (*Invite, error) { return NewInvite("i", "", "s", "acme", []string{"r"}, nil, "op", t0, 0) },
		"no subject": func() (*Invite, error) { return NewInvite("i", "h", "", "acme", []string{"r"}, nil, "op", t0, 0) },
		"no tenant":  func() (*Invite, error) { return NewInvite("i", "h", "s", "", []string{"r"}, nil, "op", t0, 0) },
		"no roles":   func() (*Invite, error) { return NewInvite("i", "h", "s", "acme", nil, nil, "op", t0, 0) },
		"no creator": func() (*Invite, error) { return NewInvite("i", "h", "s", "acme", []string{"r"}, nil, "", t0, 0) },
	} {
		if _, err := mk(); err == nil {
			t.Errorf("%s: NewInvite succeeded", name)
		}
	}
}

// A ZERO TTL FALLS BACK TO THE DEFAULT rather than minting an invite that
// expired the instant it was created.
func TestAZeroTTLUsesTheDefault(t *testing.T) {
	inv, err := NewInvite("i", "h", "s", "acme", []string{"r"}, nil, "op", t0, 0)
	if err != nil {
		t.Fatalf("NewInvite: %v", err)
	}
	if got := inv.ExpiresAt.Sub(inv.CreatedAt); got != DefaultInviteTTL {
		t.Fatalf("ttl = %v, want %v — a zero TTL must not mean 'already expired'", got, DefaultInviteTTL)
	}
}
