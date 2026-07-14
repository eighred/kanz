// Package accountproof settles, at adapter startup, the one question the OMS cannot
// answer for itself: WHICH EXCHANGE ACCOUNT does this adapter's API credential spend?
//
// The account is the collateral boundary (EXEC-M16) — an exchange margins, nets and
// LIQUIDATES per account — and every layer above this one was believing a string. The
// OMS read the account from its own manifest; the adapter read it from its own config.
// Two declarations, agreeing with each other, describing a credential neither of them
// had ever asked about. A mis-configured adapter and a correct one were the same
// observable deployment, and the difference only showed up as fills booked to one
// fund's ledger while the exchange debited another's.
//
// The exchange is the only authority on whose key this is, so we ask it, once, at
// startup, and refuse to trade on an answer we do not like.
package accountproof

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/kanz-eng/kanz/internal/execution"
)

// Exchange reports the exchange's OWN id for the account behind the adapter's API
// credential — a Binance/OKX uid. Implemented by each venue's signed REST client
// (Binance GET /api/v3/account, OKX GET /api/v5/account/config).
type Exchange interface {
	ExchangeAccountID(ctx context.Context) (string, error)
}

// Want is what the deployment CLAIMS this adapter is.
type Want struct {
	// Account is the platform's label for the collateral pool ("binance-main"),
	// stamped on every OrderState, Fill and LedgerEntry as venue_account_id.
	Account string
	// ExchangeUID is the exchange's own id for that pool — what the label is bound TO.
	// Empty ⇒ nobody has bound it, so nothing can be proved.
	ExchangeUID string
	// AllowUnverified permits booting an account that was never confirmed. It does NOT
	// permit booting a WRONG one.
	AllowUnverified bool
}

var (
	// ErrWrongAccount: the exchange says this credential belongs to a different account
	// than the deployment claims. Fatal, always, whatever the flags say — the fills of
	// this adapter would margin against money that is not the money its ledger names.
	ErrWrongAccount = errors.New("accountproof: this API credential belongs to a DIFFERENT exchange account than the adapter claims")
	// ErrUnverifiable: the account could not be confirmed — no uid was bound, or the
	// exchange could not be asked. Fatal unless the deployment explicitly accepts it.
	ErrUnverifiable = errors.New("accountproof: the adapter's exchange account could not be verified")
)

// Resolve asks the exchange who this credential belongs to and returns the proof the
// adapter reports over venue.v1.Describe.
//
// The zero-value proof (unverified) is returned ONLY when the deployment explicitly
// allowed it. Absence of configuration never silently becomes trust.
func Resolve(ctx context.Context, ex Exchange, want Want, logger *slog.Logger) (execution.AccountProof, error) {
	if want.ExchangeUID == "" {
		// Nobody said which exchange account this adapter is, so there is nothing to
		// check it against. That is a gap, not a posture.
		if !want.AllowUnverified {
			return execution.AccountProof{}, fmt.Errorf("%w: no exchange account id is bound to account %q, so the "+
				"credential cannot be checked against it. Bind one (the uid the exchange knows this account by), or set "+
				"the adapter's ALLOW_UNVERIFIED_ACCOUNT to accept an account nobody has confirmed",
				ErrUnverifiable, want.Account)
		}
		logger.Warn("EXCHANGE ACCOUNT NOT VERIFIED — no exchange account id is bound, so nobody has confirmed this API "+
			"credential spends the collateral pool it names. An exchange liquidates per account",
			"account", want.Account)
		return execution.AccountProof{}, nil
	}

	got, err := ex.ExchangeAccountID(ctx)
	if err != nil {
		// The deployment DID bind a uid — it asked for this check — and we could not
		// perform it. Booting anyway would make exactly the assumption it declined to.
		if !want.AllowUnverified {
			return execution.AccountProof{}, fmt.Errorf("%w: could not ask the exchange which account %q's credential "+
				"belongs to: %w", ErrUnverifiable, want.Account, err)
		}
		logger.Warn("EXCHANGE ACCOUNT NOT VERIFIED — the exchange could not be asked which account this credential holds",
			"account", want.Account, "err", err)
		return execution.AccountProof{}, nil
	}

	if got != want.ExchangeUID {
		// No flag forgives this. The adapter is not unproven — it is WRONG, and the
		// money it moves is not the money it says it moves.
		return execution.AccountProof{}, fmt.Errorf("%w: deployed as account %q (exchange account id %q) but the "+
			"exchange says this credential belongs to exchange account id %q. Every fill would be booked to %[2]q's "+
			"ledger rows while the exchange margined and liquidated %[4]q",
			ErrWrongAccount, want.Account, want.ExchangeUID, got)
	}

	logger.Info("exchange account VERIFIED — the exchange confirms this credential belongs to the account we claim",
		"account", want.Account, "exchange_account_id", got)
	return execution.AccountProof{Verified: true, ExchangeAccountID: got}, nil
}
