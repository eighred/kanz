// kanz-invite issues a single-use invitation to an account (#364).
//
// THIS IS THE BOOTSTRAP PATH, AND IT IS DELIBERATELY NARROW. Provisioning is
// normally an operator action on the authenticated control plane — but the FIRST
// operator cannot be invited by an operator, and an estate with no accounts has
// nobody who could authenticate to create one. So this writes to the identity
// store directly, which means holding the database credential: the authority to
// run it is the authority to reach the store, and that is the point.
//
// IT IS NOT A SUBSTITUTE FOR THE CONTROL PLANE. Once an operator account exists,
// further invitations belong on the authenticated surface, where they are
// attributable to a person rather than to whoever holds a DSN.
//
// THE TOKEN IS PRINTED ONCE AND IS NOT RECOVERABLE. Only its SHA-256 is stored,
// so a lost token means issuing a new invitation — which is the same property
// that makes a leaked database useless for impersonating an invitee.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/internal/pg"
)

func main() { os.Exit(run()) }

func run() int {
	var (
		dsn        = flag.String("dsn", os.Getenv("IDENTITY_DATABASE_URL"), "identity store DSN (default $IDENTITY_DATABASE_URL)")
		subject    = flag.String("subject", "", "the account to offer, e.g. user:alice (required)")
		tenant     = flag.String("tenant", "", "the tenant the account belongs to (required)")
		roles      = flag.String("roles", "", "comma-separated roles the account will carry (required)")
		portfolios = flag.String("portfolios", "", "comma-separated portfolios the account may act on")
		ttl        = flag.Duration("ttl", identity.DefaultInviteTTL, "how long the invitation stays usable")
		by         = flag.String("by", "", "who is issuing this, recorded on the invite (required)")
	)
	flag.Parse()

	missing := []string{}
	for name, v := range map[string]string{
		"-dsn": *dsn, "-subject": *subject, "-tenant": *tenant, "-roles": *roles, "-by": *by,
	} {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "kanz-invite: missing required flags: %s\n\n", strings.Join(missing, " "))
		flag.Usage()
		return 2
	}

	// ROLES ARE THE AUTHORITY BEING GRANTED, so an empty list is refused rather
	// than treated as "none" — an invitation that grants nothing is never what
	// somebody meant to type, and a typo'd flag would produce exactly that.
	roleList := splitList(*roles)
	if len(roleList) == 0 {
		fmt.Fprintln(os.Stderr, "kanz-invite: -roles parsed to an empty list; an invitation that grants "+
			"no authority creates an account that can do nothing")
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pg.NewGlobalPool(ctx, *dsn, identity.WhyNoTenantScope)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kanz-invite: connect: %v\n", err)
		return 1
	}
	defer pool.Close()

	raw, hash, err := identity.NewInviteToken()
	if err != nil {
		fmt.Fprintf(os.Stderr, "kanz-invite: mint token: %v\n", err)
		return 1
	}
	inv, err := identity.NewInvite(uuid.NewString(), hash, *subject, *tenant,
		roleList, splitList(*portfolios), *by, time.Now().UTC(), *ttl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kanz-invite: %v\n", err)
		return 2
	}
	if err := identity.NewPostgres(pool).CreateInvite(ctx, inv); err != nil {
		// The most likely cause is the partial unique index: one LIVE invite per
		// subject, so a second is refused rather than racing the first.
		fmt.Fprintf(os.Stderr, "kanz-invite: %v\n\nIf this subject already has an unredeemed "+
			"invitation, that one must be redeemed or expire first — two live invitations are two "+
			"authorities competing to become one account.\n", err)
		return 1
	}

	// STDOUT IS THE TOKEN AND NOTHING ELSE, so it can be piped; everything a
	// human needs goes to stderr. Printed once — only its hash is stored.
	fmt.Fprintf(os.Stderr, "invitation created for %s in %s\n  roles:      %s\n  portfolios: %s\n"+
		"  expires:    %s\n\nGive the invitee this single-use token; it is not recoverable:\n\n",
		inv.Subject, inv.Tenant, strings.Join(inv.Roles, ", "), listOrNone(inv.Portfolios),
		inv.ExpiresAt.Format(time.RFC3339))
	fmt.Println(raw)
	return 0
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func listOrNone(v []string) string {
	if len(v) == 0 {
		return "(none)"
	}
	return strings.Join(v, ", ")
}
