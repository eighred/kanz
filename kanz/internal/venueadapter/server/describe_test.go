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

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/internal/venueadapter/orderview"
)

func newServerWithProof(t *testing.T, v *fakeVenue, proof execution.AccountProof) *Server {
	t.Helper()
	closes := execution.NewCloseRegistry()
	v.closes = closes
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(v, orderview.NewMemory(), closes, proof, halt.OpenGate(nil), logger)
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

// ===== THE CAPABILITY DECLARATIONS MUST REACH THE WIRE (#405, #486) =====
//
// Describe is the ONLY way the OMS learns what an adapter can do. Both
// declarations arm admission gates: an order type or a time-in-force the adapter
// cannot handle is refused before the order is stored and announced.
//
// A DECLARATION READ AND DISCARDED WOULD SATISFY EVERY OTHER TEST. The connector
// tests prove the lists match the translation switches; the OMS tests prove a
// declared list arms the gate. Neither touches this hop — the server asking the
// venue and putting the answer on the response — so until now a Describe handler
// that simply never set the fields would leave both gates open with nothing
// failing anywhere.

// declaringVenue is a fakeVenue that also answers the capability questions.
type declaringVenue struct {
	fakeVenue
	types []orderpb.OrderType
	tifs  []orderpb.TimeInForce
}

func (d *declaringVenue) OrderTypes() []orderpb.OrderType    { return d.types }
func (d *declaringVenue) TimeInForce() []orderpb.TimeInForce { return d.tifs }

func TestDescribeReportsBothCapabilityDeclarations(t *testing.T) {
	v := &declaringVenue{
		types: []orderpb.OrderType{
			orderpb.OrderType_ORDER_TYPE_MARKET,
			orderpb.OrderType_ORDER_TYPE_LIMIT,
		},
		tifs: []orderpb.TimeInForce{
			orderpb.TimeInForce_TIME_IN_FORCE_GTC,
			orderpb.TimeInForce_TIME_IN_FORCE_IOC,
		},
	}
	closes := execution.NewCloseRegistry()
	v.closes = closes
	s := New(v, orderview.NewMemory(), closes, execution.AccountProof{Verified: true}, halt.OpenGate(nil), slog.New(slog.NewTextHandler(io.Discard, nil)))

	resp, err := s.Describe(context.Background(), &venuepb.DescribeRequest{})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(resp.GetSupportedOrderTypes()) != 2 {
		t.Errorf("supported_order_types = %v, want the adapter's two — a declaration read and "+
			"discarded leaves the admission gate open with nothing failing anywhere",
			resp.GetSupportedOrderTypes())
	}
	if len(resp.GetSupportedTimeInForce()) != 2 {
		t.Errorf("supported_time_in_force = %v, want the adapter's two — without it the OMS "+
			"admits an instruction the connector will refuse, after the order is stored and "+
			"announced", resp.GetSupportedTimeInForce())
	}
}

// AN ADAPTER THAT DECLARES NOTHING REPORTS NOTHING, and that is the honest
// answer rather than an empty list meaning "supports none". The OMS reads
// silence as "did not say" and leaves the gate open deliberately — refusing every
// order for an adapter that predates these fields would turn a schema addition
// into a trading outage.
func TestDescribeReportsNoCapabilitiesForAnUndeclaringAdapter(t *testing.T) {
	s := newServerWithProof(t, &fakeVenue{}, execution.AccountProof{Verified: true})

	resp, err := s.Describe(context.Background(), &venuepb.DescribeRequest{})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(resp.GetSupportedOrderTypes()) != 0 || len(resp.GetSupportedTimeInForce()) != 0 {
		t.Errorf("an adapter that declares nothing reported types=%v tifs=%v — inventing a "+
			"declaration on its behalf would arm a gate against a list nobody stated",
			resp.GetSupportedOrderTypes(), resp.GetSupportedTimeInForce())
	}
}
