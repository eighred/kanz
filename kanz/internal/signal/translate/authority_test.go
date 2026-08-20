package translate

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	signalpb "github.com/eighred/kanz/kanz-schemas-go/signal/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// THE CROSS-TENANT ORDER INJECTION (#632).
//
// webhook-ingest authenticates a sender as a STRATEGY — HMAC over the raw body
// against that strategy's secret — and then took the TENANT from the `fund_id`
// carried in the same body. The seam that mapped fund→tenant defaulted to the
// identity function and was assigned at no composition root, so the tenant of a
// signal-originated order was literally the string the caller typed. One holder
// of one strategy's secret could publish SubmitOrder commands onto any tenant's
// book, from the internet, with the audit trail naming the victim.
//
// These tests pin the two halves of the repair: an authenticated strategy can
// only reach a fund it is BOUND to, and a deployment that declares no binding
// cannot construct a translator at all.

// boundAuthority is the binding used across this package's tests: `momentum`
// (the strategy in intent()) may trade fund-alpha, which tenant acme owns.
// `intruder` holds a valid secret of its own and is bound to a DIFFERENT tenant's
// fund — which is exactly the attacker's position in #632.
func boundAuthority(t *testing.T) *StaticFundAuthority {
	t.Helper()
	a, err := NewFundAuthority(
		map[string]string{"fund-alpha": "acme", "fund-victim": "globex"},
		map[string][]string{
			"momentum": {"fund-alpha"},
			"intruder": {"fund-victim"},
		},
	)
	if err != nil {
		t.Fatalf("NewFundAuthority: %v", err)
	}
	return a
}

// TestASignedSignalForAnotherTenantsFundIsRefusedAndPublishesNothing is the
// attack, run end to end through Emit.
//
// `momentum` is authentic — its intent would trade fine against its own fund.
// Here it names fund-victim, which belongs to tenant globex. The refusal must be
// total: no SubmitOrder command, and no StrategySignal FACT either, because the
// audit root is what every order chains causation to and writing one under
// globex would itself be the cross-tenant write.
func TestASignedSignalForAnotherTenantsFundIsRefusedAndPublishesNothing(t *testing.T) {
	rec := &recorder{}
	tr, err := New(Options{
		Prices:    StaticPrices{"BTC-USD": big.NewRat(50_000, 1)},
		Equity:    StaticEquity{"fund-alpha": big.NewRat(1_000_000, 1), "fund-victim": big.NewRat(1_000_000, 1)},
		Positions: StaticPositions{},
		Alloc: StaticAllocation{
			"fund-alpha":  {{Venue: "BINANCE", Weight: big.NewRat(1, 1)}},
			"fund-victim": {{Venue: "BINANCE", Weight: big.NewRat(1, 1)}},
		},
		Publisher: rec,
		Gate:      OpenGate(nil),
		Authority: boundAuthority(t),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	in := intent(signalpb.SignalAction_SIGNAL_ACTION_BUY, big.NewRat(1, 1), signalpb.SizeType_SIZE_TYPE_ABSOLUTE_QTY)
	in.FundID = "fund-victim" // the other tenant's fund — everything else is genuine

	_, err = tr.Emit(context.Background(), in)
	if !errors.Is(err, ErrUnboundFund) {
		t.Fatalf("Emit = %v, want ErrUnboundFund.\n\n"+
			"An authenticated strategy named a fund belonging to ANOTHER TENANT and the platform "+
			"acted on it. That is #632: the HMAC proves the strategy, the fund_id is caller-supplied, "+
			"and the fund_id was the tenant.", err)
	}
	// The refusal has to name both sides or an operator cannot tell an attack from a
	// bootstrap file missing a binding.
	for _, want := range []string{"momentum", "fund-victim"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}
	if len(rec.events) != 0 {
		t.Fatalf("%d event(s) were published for a refused signal: %v.\n\n"+
			"Nothing may reach the bus — not the order command, and not the StrategySignal FACT "+
			"either: the FACT is the audit root every order chains causation to, so writing one "+
			"under the victim's tenant IS the cross-tenant write.", len(rec.events), rec.events)
	}
}

// THE AUTHORIZATION DECISION IS NOT MASKED BY A LESSER ONE.
//
// A signal that is BOTH unbound and stale must report the unbound fund. Emit
// checks several things, and whichever runs first is the only one an operator
// ever sees: if the freshness bound (#416) ran first, an attacker probing with
// slightly old alerts would generate "stale signal" lines and the attempted
// cross-tenant write would be invisible in the logs and absent from the counter.
func TestAnUnboundFundIsReportedEvenWhenTheSignalIsAlsoStale(t *testing.T) {
	rec := &recorder{}
	fired := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	tr, err := New(Options{
		Prices:    StaticPrices{"BTC-USD": big.NewRat(50_000, 1)},
		Equity:    StaticEquity{"fund-victim": big.NewRat(1_000_000, 1)},
		Positions: StaticPositions{},
		Alloc: StaticAllocation{
			"fund-victim": {{Venue: "BINANCE", Weight: big.NewRat(1, 1)}},
		},
		Publisher:    rec,
		Gate:         OpenGate(nil),
		Authority:    boundAuthority(t),
		MaxSignalAge: 2 * time.Minute,
		Now:          func() time.Time { return fired.Add(30 * time.Minute) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	in := intent(signalpb.SignalAction_SIGNAL_ACTION_BUY, big.NewRat(1, 1), signalpb.SizeType_SIZE_TYPE_ABSOLUTE_QTY)
	in.FundID = "fund-victim"
	in.SourceTS = timestamppb.New(fired) // 30 minutes old — also refusable on age

	_, err = tr.Emit(context.Background(), in)
	if !errors.Is(err, ErrUnboundFund) {
		t.Fatalf("Emit = %v, want ErrUnboundFund.\n\n"+
			"A signal reaching into another tenant's fund was reported as something else. The "+
			"security decision has to run first, or an attacker's probes are filed under a "+
			"different (and unalarming) refusal.", err)
	}
	if errors.Is(err, ErrStaleSignal) {
		t.Errorf("the refusal is also ErrStaleSignal (%v) — the two must not be conflated: one "+
			"is a sender's clock, the other is a sender reaching for a book it does not own", err)
	}
}

// A strategy this deployment has never heard of is refused for the same reason,
// and with the same answer: an unknown strategy and an unbound pair are not
// distinguished, so a caller cannot use the response to enumerate the estate.
func TestAnUnknownStrategyIsRefused(t *testing.T) {
	a := boundAuthority(t)
	if _, ok := a.TenantForFund("no-such-strategy", "fund-alpha"); ok {
		t.Fatal("an unknown strategy resolved a tenant — any sender who can guess a fund id " +
			"would be trading it")
	}
}

// AND THE BOUND CASE STILL WORKS. Without this the test above is satisfied by an
// authority that refuses everything — a trading outage wearing the shape of a fix.
func TestABoundStrategyResolvesItsFundsTenant(t *testing.T) {
	a := boundAuthority(t)
	tenant, ok := a.TenantForFund("momentum", "fund-alpha")
	if !ok {
		t.Fatal("the bound strategy/fund pair was refused — this fix would take every legitimate " +
			"sender offline")
	}
	if tenant != "acme" {
		t.Errorf("tenant = %q, want acme (from configuration, NOT the fund id)", tenant)
	}
	// The tenant is not the fund id. If it were, this whole change would be a no-op
	// dressed as a fix, and #632 would still be live.
	if tenant == "fund-alpha" {
		t.Error("the tenant is the fund id — the identity default is back")
	}
}

// A TRANSLATOR WITH NO BINDING TABLE MUST NOT CONSTRUCT.
//
// This is the shape of the original defect: the seam existed, nothing assigned
// it, and the nil default made the deployment look configured. There is no
// default now, so an unconfigured binary fails at construction — which in
// webhook-ingest is config.Load, before the listener exists.
func TestNewRefusesWithoutAFundAuthority(t *testing.T) {
	_, err := New(Options{
		Prices: StaticPrices{}, Equity: StaticEquity{}, Positions: StaticPositions{},
		Alloc:     StaticAllocation{"fund-alpha": {{Venue: "BINANCE", Weight: big.NewRat(1, 1)}}},
		Publisher: &recorder{}, Gate: OpenGate(nil),
		// Authority deliberately absent.
	})
	if err == nil {
		t.Fatal("New accepted a nil FundAuthority. That is the #632 seam exactly: nobody assigns " +
			"it, the default makes the caller's fund_id the tenant, and the deployment looks healthy.")
	}
	if !strings.Contains(err.Error(), "FundAuthority") {
		t.Errorf("error %q does not name the missing seam — an operator cannot see what to wire", err)
	}
}

// NewFundAuthority is where "nothing configured" is made to look different from
// "checked, and fine". Each of these is a bootstrap file that would otherwise
// have produced a service that starts and then refuses (or misroutes) traffic.
func TestNewFundAuthorityRefusesAnIncoherentTable(t *testing.T) {
	for _, tc := range []struct {
		name          string
		fundTenant    map[string]string
		strategyFunds map[string][]string
		want          string
	}{
		{
			"no funds at all",
			map[string]string{},
			map[string][]string{"momentum": {"fund-alpha"}},
			"no fund is bound to a tenant",
		},
		{
			"no strategies at all",
			map[string]string{"fund-alpha": "acme"},
			map[string][]string{},
			"no strategy is bound to a fund",
		},
		{
			"a fund with no tenant",
			map[string]string{"fund-alpha": "  "},
			map[string][]string{"momentum": {"fund-alpha"}},
			"declares no tenant",
		},
		{
			"a strategy bound to nothing",
			map[string]string{"fund-alpha": "acme"},
			map[string][]string{"momentum": {}},
			"bound to no fund",
		},
		{
			"a strategy bound to a fund that does not exist",
			map[string]string{"fund-alpha": "acme"},
			map[string][]string{"momentum": {"fund-typo"}},
			"which no `funds` entry declares",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewFundAuthority(tc.fundTenant, tc.strategyFunds)
			if !errors.Is(err, ErrNoFundAuthority) {
				t.Fatalf("NewFundAuthority = %v, want ErrNoFundAuthority — this deployment would "+
					"have started", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q — an operator cannot see what to fix", err, tc.want)
			}
		})
	}
}
