package okx

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/venueadapter/exchangeauth"
	"github.com/eighred/kanz/internal/venuemargin"
	"github.com/eighred/kanz/pkg/bus"

	collateralpb "github.com/eighred/kanz/kanz-schemas-go/collateral/v1"
)

// Physical OKX demo exercise of the WHOLE #408 control-1 path: the live venue →
// okxREST.MarginState → venuemargin.Reporter.build → the VenueMarginState FACT.
// Gated on TEST_OKX_TESTNET=1 plus OKX_TESTNET_KEY/_SECRET/_PASSPHRASE, the same
// contract TestOKXTestnet_SignedRoundTrip uses.
//
// GET ONLY, STRUCTURALLY. MarginState reads /api/v5/account/config to establish
// the account mode, then /api/v5/account/balance and /api/v5/account/positions.
// The publisher below is a capture that touches no network. Nothing in this
// file can place, amend or cancel an order.
//
// # WHY THE TRANSPORT PROBES DID NOT ALREADY COVER THIS
//
// A signed balance read proves the HMAC + passphrase + x-simulated-trading
// handshake works. It says nothing about whether the REPORTER maps that response
// into a MarginState correctly, and that mapping is the whole of control 1. The
// fake-server tests exercise the mapping against bytes we wrote ourselves, so
// between them there was a gap exactly the width of "what OKX actually returns".
//
// # THE TWO OUTCOMES ARE BOTH RESULTS, AND THE TEST SAYS WHICH
//
// A demo SPOT account may carry no margin ratio because its observed account
// mode does not support one. A margin-capable account with an absent ratio has
// an UNKNOWN observation and remains uncovered. This test asserts the invariant
// that holds in each arm and reports which one ran:
//
//	ratio REPORTED    → MarginRatio is set, and coverage counted it
//	ratio ABSENT + UNSUPPORTED → nil, no synthetic zero, complete for this field
//	ratio ABSENT + SUPPORTED/UNKNOWN → nil and an exclusion names no_margin_ratio
//
// WHAT MUST NEVER HAPPEN IS THE THIRD OUTCOME: a MarginRatio present and zero
// where the venue reported none. venuemargin.toDecimal returns (nil, false) for a
// nil *big.Rat precisely so an unreported quantity cannot become a measured
// zero — and a zero margin ratio reads as "no collateral cushion", which a
// fail-closed gate would treat as the most alarming number the venue can send.
// That is the assertion carrying this test.
func TestOKXDemo_ReporterMapsLiveMarginState(t *testing.T) {
	if os.Getenv("TEST_OKX_TESTNET") == "" {
		t.Skip("set TEST_OKX_TESTNET=1 + OKX_TESTNET_KEY/_SECRET/_PASSPHRASE to run the live demo margin-state read")
	}
	key, secret, pass := os.Getenv("OKX_TESTNET_KEY"), os.Getenv("OKX_TESTNET_SECRET"), os.Getenv("OKX_TESTNET_PASSPHRASE")
	if key == "" || secret == "" || pass == "" {
		t.Fatal("OKX_TESTNET_KEY, OKX_TESTNET_SECRET and OKX_TESTNET_PASSPHRASE are required")
	}

	rest := newOKXREST(okxRestConfig{
		BaseURL: envOrDefault("OKX_TESTNET_BASE", "https://www.okx.com"),
		APIKey:  key, APISecret: secret, Passphrase: pass,
		Buckets:    newOKXBuckets(NewWeightBucket(60, 2*time.Second, nil), nil),
		HTTPClient: NewExchangeHTTPClient(0),
		// Demo, for the reason the sibling harness gives: these are demo keys, and
		// without the header they ask production about an account that does not
		// exist there.
		Mode: exchangeauth.OKXDemo,
	})

	cap := &captureOnly{}
	var uncovered []string
	reporter := venuemargin.NewReporter(venuemargin.ReporterConfig{
		Source: rest,
		Pub:    cap,
		Venue:  "XOKX",
		// Venue and Account must be non-empty or build refuses — an unattributed
		// margin figure cannot be bound to the portfolio it gates.
		Account: "okx-demo-probe",
		Tenant:  "probe",
		OnUncovered: func(reason string, n int) {
			uncovered = append(uncovered, reason)
			t.Logf("coverage: %s ×%d", reason, n)
		},
		OnError: func(err error) { t.Logf("reporter error callback: %v", err) },
	})
	if reporter == nil {
		t.Fatal("NewReporter returned nil — a nil Source or Pub, which this test supplies both of")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := reporter.Report(ctx); err != nil {
		t.Fatalf("Report against the live demo book failed: %v\n\n"+
			"This is #408 control 1's path end to end. The account leg is required — without it "+
			"there is no observation at all — so a failure here means the pre-trade margin gate "+
			"has no input and fails closed on every order.", err)
	}

	msg, ok := cap.last.(*collateralpb.VenueMarginState)
	if !ok || msg == nil {
		t.Fatalf("the reporter published %T, want *collateralpb.VenueMarginState", cap.last)
	}
	// The reporter refuses an undated observation (ErrUndated), so reaching here
	// means it was dated — assert it rather than assume it, since ObservedAt is
	// what every downstream freshness bound is measured against.
	if msg.GetObservedAt() == nil || msg.GetObservedAt().AsTime().IsZero() {
		t.Error("published FACT carries no observed_at — a margin number that cannot be dated is " +
			"not a margin number")
	}

	// ── The arm that ran, and the invariant that holds either way.
	switch ratio := msg.GetMarginRatio(); {
	case ratio != nil:
		t.Logf("REPORTED: OKX demo returned a margin ratio, and the reporter mapped it "+
			"(coefficient=%d exponent=%d). #408 control 1's populated path is exercised.",
			ratio.GetCoefficient(), ratio.GetExponent())
		if cov := msg.GetCoverage(); cov != nil && cov.GetContributed() == 0 {
			t.Error("a margin ratio was mapped but coverage counted no contribution — the FACT " +
				"would understate what the venue actually told us")
		}
	default:
		switch msg.GetMarginRatioSupport() {
		case collateralpb.SupportStatus_SUPPORT_STATUS_UNSUPPORTED:
			t.Log("UNSUPPORTED: OKX account mode does not support a margin ratio; the FACT carries no numeric value.")
			if containsReason(uncovered, venuemargin.SkipNoMarginRatio) {
				t.Errorf("unsupported margin ratio was counted as an observation failure (reasons: %v)", uncovered)
			}
		case collateralpb.SupportStatus_SUPPORT_STATUS_SUPPORTED,
			collateralpb.SupportStatus_SUPPORT_STATUS_UNSPECIFIED:
			t.Log("UNKNOWN: the account may support a margin ratio, but this observation carried none.")
			if !containsReason(uncovered, venuemargin.SkipNoMarginRatio) {
				t.Errorf("no margin ratio was reported and no %q exclusion was raised (reasons: %v)",
					venuemargin.SkipNoMarginRatio, uncovered)
			}
			cov := msg.GetCoverage()
			if cov == nil || cov.GetExcludedCount() == 0 {
				t.Error("missing supported/unknown ratio was presented as complete coverage")
			}
		default:
			t.Errorf("unknown margin-ratio support enum %v", msg.GetMarginRatioSupport())
		}
	}

	// ── THE THIRD OUTCOME, WHICH IS THE DEFECT. Asserted unconditionally because
	// it is wrong in both arms: a zero ratio the venue never sent reads as "no
	// collateral cushion" — the most alarming figure it could carry — and a
	// fail-closed gate would act on it.
	if r := msg.GetMarginRatio(); r != nil && r.GetCoefficient() == 0 && !containsReason(uncovered, venuemargin.SkipNoMarginRatio) {
		t.Error("margin_ratio is present and ZERO with no exclusion raised. If the venue reported " +
			"no ratio, this is an unreported quantity that became a measured zero — the exact " +
			"substitution venuemargin.toDecimal returns (nil, false) to prevent.")
	}

	t.Logf("maintenance_margin set=%t, liquidation_prices=%d, coverage contributed=%d excluded=%d",
		msg.GetMaintenanceMargin() != nil, len(msg.GetLiquidationPrices()),
		msg.GetCoverage().GetContributed(), msg.GetCoverage().GetExcludedCount())
}

// captureOnly is an execution.Publisher that records the payload and reaches no
// network. The probe asserts on what the REPORTER BUILT; putting it on a bus
// would add a broker to a test about a mapping.
type captureOnly struct{ last any }

func (c *captureOnly) Publish(_ context.Context, e bus.Event) error {
	c.last = e.Payload
	return nil
}

func containsReason(reasons []string, want string) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}
