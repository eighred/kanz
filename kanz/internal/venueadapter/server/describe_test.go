package server

// Describe is how the OMS asks an adapter WHOSE MONEY IT SPENDS.
//
// The account is the collateral boundary (EXEC-M16) — an exchange margins and
// LIQUIDATES per account — and the OMS used to learn it from a string in its own
// manifest. The adapter is the only process holding the credential, so it is the
// only one that can answer honestly, and an adapter that was never PROVEN against
// the exchange must say that too: an unverified claim and a verified one must not
// be the same observable answer.

import (
	"context"
	"io"
	"log/slog"
	"testing"

	venuepb "github.com/kanz-eng/kanz-schemas-go/venue/v1"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/venueadapter/orderview"
)

func newServerWithProof(t *testing.T, v *fakeVenue, proof execution.AccountProof) *Server {
	t.Helper()
	closes := execution.NewCloseRegistry()
	v.closes = closes
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(v, orderview.NewMemory(), closes, proof, logger)
}

// TestDescribeReportsTheProvenAccount: the exchange confirmed the key behind this
// adapter belongs to binance-main, and Describe carries that fact — including the
// exchange's OWN id for it, which is what the label was bound to.
func TestDescribeReportsTheProvenAccount(t *testing.T) {
	s := newServerWithProof(t, &fakeVenue{}, execution.AccountProof{
		Verified:          true,
		ExchangeAccountID: "12345678",
	})

	resp, err := s.Describe(context.Background(), &venuepb.DescribeRequest{})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if resp.GetMic() != "XBIN" {
		t.Errorf("mic = %q, want XBIN", resp.GetMic())
	}
	if resp.GetAccount() != "binance-main" {
		t.Errorf("account = %q, want binance-main", resp.GetAccount())
	}
	if !resp.GetAccountVerified() {
		t.Error("account_verified = false for an account the exchange confirmed")
	}
	if resp.GetExchangeAccountId() != "12345678" {
		t.Errorf("exchange_account_id = %q, want 12345678", resp.GetExchangeAccountId())
	}
}

// TestDescribeAdmitsAnUnprovenAccount: with no proof, the adapter still names the
// account it was CONFIGURED with — but it must not claim the exchange agreed.
// The zero value of AccountProof is unverified, so an adapter that forgets to
// verify reports the safe answer, not the flattering one.
func TestDescribeAdmitsAnUnprovenAccount(t *testing.T) {
	s := newServerWithProof(t, &fakeVenue{}, execution.AccountProof{})

	resp, err := s.Describe(context.Background(), &venuepb.DescribeRequest{})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if resp.GetAccount() != "binance-main" {
		t.Errorf("account = %q, want binance-main", resp.GetAccount())
	}
	if resp.GetAccountVerified() {
		t.Error("account_verified = true with no proof — an unverified claim is being reported as a confirmed one")
	}
	if resp.GetExchangeAccountId() != "" {
		t.Errorf("exchange_account_id = %q, want empty when nothing was verified", resp.GetExchangeAccountId())
	}
}
