package okx

// What these prove, and what they cannot (#408, control 1; #70).
//
// PROVEN HERE: the decode, the empty-field discipline, the observation time, and
// the degradation when only half the answer arrives. Every case runs against a
// local server serving OKX's documented response SHAPE.
//
// NOT PROVEN ANYWHERE: that OKX serves these field names and units today, and
// that the account behind this deployment's credential is in a mode that
// populates them at all. This estate holds no OKX credentials, so no test has
// ever seen the real endpoint answer. #529's lesson stands — a venue's wire
// rules are only knowable by asking the venue — and this is the half of the
// control that needs a key, not more code.

import (
	"context"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/venueadapter/exchangeauth"
)

type fakeMarginOKX struct {
	srv          *httptest.Server
	balanceBody  string
	positionBody string
	balanceCode  int
	positionCode int
	positionHits int
}

func newFakeMarginOKX(t *testing.T) *fakeMarginOKX {
	t.Helper()
	f := &fakeMarginOKX{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/account/balance", func(w http.ResponseWriter, _ *http.Request) {
		if f.balanceCode != 0 {
			w.WriteHeader(f.balanceCode)
		}
		_, _ = w.Write([]byte(f.balanceBody))
	})
	mux.HandleFunc("/api/v5/account/positions", func(w http.ResponseWriter, _ *http.Request) {
		f.positionHits++
		if f.positionCode != 0 {
			w.WriteHeader(f.positionCode)
		}
		_, _ = w.Write([]byte(f.positionBody))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func marginRESTOver(f *fakeMarginOKX) *okxREST {
	return newOKXREST(okxRestConfig{
		BaseURL: f.srv.URL, APIKey: "k", APISecret: "s", Passphrase: "p",
		Bucket: NewWeightBucket(60, 2*time.Second, nil), Mode: exchangeauth.OKXDemo,
	})
}

// A margined unified account: OKX populates mmr, mgnRatio and uTime, and the
// positions endpoint carries a liquidation price.
const okxMarginedBalance = `{"code":"0","msg":"","data":[{"uTime":"1755691200000",` +
	`"totalEq":"92000","imr":"3000","mmr":"1250.5","mgnRatio":"3.75"}]}`

const okxPositions = `{"code":"0","msg":"","data":[` +
	`{"instId":"BTC-USDT-SWAP","liqPx":"41000","uTime":"1755691200000"},` +
	`{"instId":"ETH-USDT-SWAP","liqPx":"","uTime":"1755691200000"}]}`

// TestMarginStateReadsOKXsOwnFigures is the non-vacuity arm for the refusals
// below: without it a source that returned an empty observation for every input
// would pass all of them.
func TestMarginStateReadsOKXsOwnFigures(t *testing.T) {
	f := newFakeMarginOKX(t)
	f.balanceBody, f.positionBody = okxMarginedBalance, okxPositions

	got, err := marginRESTOver(f).MarginState(context.Background())
	if err != nil {
		t.Fatalf("MarginState: %v", err)
	}
	if got.MaintenanceMargin == nil || got.MaintenanceMargin.Cmp(mrat(t, "1250.5")) != 0 {
		t.Errorf("maintenance margin = %v, want 1250.5 (OKX's mmr)", got.MaintenanceMargin)
	}
	if got.MarginRatio == nil || got.MarginRatio.Cmp(mrat(t, "3.75")) != 0 {
		t.Errorf("margin ratio = %v, want 3.75 (OKX's mgnRatio)", got.MarginRatio)
	}
	want := time.UnixMilli(1755691200000).UTC()
	if !got.ObservedAt.Equal(want) {
		t.Errorf("observedAt = %s, want OKX's own uTime %s — the fetch clock says how recently WE asked, "+
			"not how recently OKX looked", got.ObservedAt, want)
	}
	if len(got.Positions) != 2 {
		t.Fatalf("positions = %d, want 2 (the one WITHOUT a liquidation price is still reported)", len(got.Positions))
	}
	byInst := map[string]*big.Rat{}
	for _, p := range got.Positions {
		byInst[p.Symbol] = p.LiquidationPrice
	}
	if lp := byInst["BTC-USDT-SWAP"]; lp == nil || lp.Cmp(mrat(t, "41000")) != 0 {
		t.Errorf("BTC liquidation price = %v, want 41000", lp)
	}
	if lp, held := byInst["ETH-USDT-SWAP"]; !held {
		t.Error("a position OKX reported without a liquidation price was DROPPED — it is exactly the " +
			"position nobody can see the liquidation distance of")
	} else if lp != nil {
		t.Errorf("ETH liquidation price = %v, want nil (OKX reported none)", lp)
	}
}

// TestCashModeAccountReportsNoMarginAtAll is the case this adapter will meet
// most often until an account is switched to a margin mode: OKX returns "" for
// every margin field. Parsed as zero it would claim the account needs no
// collateral and liquidates at zero — the confident zero #408 singles out.
func TestCashModeAccountReportsNoMarginAtAll(t *testing.T) {
	f := newFakeMarginOKX(t)
	f.balanceBody = `{"code":"0","msg":"","data":[{"uTime":"1755691200000","totalEq":"92000","imr":"","mmr":"","mgnRatio":""}]}`
	f.positionBody = `{"code":"0","msg":"","data":[]}`

	got, err := marginRESTOver(f).MarginState(context.Background())
	if err != nil {
		t.Fatalf("MarginState: %v", err)
	}
	if got.MaintenanceMargin != nil {
		t.Errorf("maintenance margin = %v for an account OKX does not margin, want nil (UNKNOWN)", got.MaintenanceMargin)
	}
	if got.MarginRatio != nil {
		t.Errorf("margin ratio = %v for an account OKX does not margin, want nil (UNKNOWN)", got.MarginRatio)
	}
	if got.ObservedAt.IsZero() {
		t.Error("observedAt is zero — an observation that reports UNKNOWN still has to be datable")
	}
}

// TestPositionsFailureDoesNotDiscardTheAccountFigures: the maintenance margin is
// what a pre-trade gate reads. Throwing it away because the SECOND call was rate
// limited turns a partial answer into a total refusal, which under a fail-closed
// gate is a trading halt caused by the less important endpoint.
func TestPositionsFailureDoesNotDiscardTheAccountFigures(t *testing.T) {
	f := newFakeMarginOKX(t)
	f.balanceBody = okxMarginedBalance
	f.positionBody, f.positionCode = `{"code":"50011","msg":"rate limited"}`, http.StatusTooManyRequests

	got, err := marginRESTOver(f).MarginState(context.Background())
	if err != nil {
		t.Fatalf("MarginState: %v", err)
	}
	if got.MaintenanceMargin == nil {
		t.Error("the account's maintenance margin was discarded because the POSITIONS call failed")
	}
	if len(got.Positions) != 0 {
		t.Errorf("positions = %d after a failed positions call, want 0 — a failure must not become a "+
			"claim that the account holds none", len(got.Positions))
	}
}

// TestAccountFailureIsAnError: without the account leg there is no observation
// at all, and inventing an empty one would publish "OKX reports no margin" when
// the truth is "we could not ask".
func TestAccountFailureIsAnError(t *testing.T) {
	f := newFakeMarginOKX(t)
	f.balanceBody = `{"code":"50011","msg":"rate limited"}`

	if _, err := marginRESTOver(f).MarginState(context.Background()); err == nil {
		t.Error("a failed account call produced an observation instead of an error")
	}
	if f.positionHits != 0 {
		t.Errorf("positions was called %d times after the account leg failed, want 0", f.positionHits)
	}
}

// TestUnparseableAndNegativeFiguresAreUnknown. ParseDec answers an unparseable
// string with ZERO, which is why this path does not use it: neither quantity is
// meaningful below zero on any venue, so a negative one is a field this code has
// misread rather than a state OKX is in.
func TestUnparseableAndNegativeFiguresAreUnknown(t *testing.T) {
	for _, tc := range []struct{ name, mmr string }{
		{"garbage", "n/a"},
		{"negative", "-100"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeMarginOKX(t)
			f.balanceBody = `{"code":"0","msg":"","data":[{"uTime":"1755691200000","mmr":"` + tc.mmr + `","mgnRatio":"3.75"}]}`
			f.positionBody = `{"code":"0","msg":"","data":[]}`

			got, err := marginRESTOver(f).MarginState(context.Background())
			if err != nil {
				t.Fatalf("MarginState: %v", err)
			}
			if got.MaintenanceMargin != nil {
				t.Errorf("maintenance margin = %v from mmr %q, want nil (UNKNOWN)", got.MaintenanceMargin, tc.mmr)
			}
			if got.MarginRatio == nil {
				t.Error("the SIBLING field was refused too — one bad quantity must not blind the others")
			}
		})
	}
}

// TestMissingVenueStampFallsBackToTheFetchClock, and never to the Unix epoch: a
// 1970 observation time would be judged stale by every bound and would silently
// disable the account's margin controls while looking like ordinary staleness.
func TestMissingVenueStampFallsBackToTheFetchClock(t *testing.T) {
	f := newFakeMarginOKX(t)
	f.balanceBody = `{"code":"0","msg":"","data":[{"uTime":"","mmr":"1250.5","mgnRatio":"3.75"}]}`
	f.positionBody = `{"code":"0","msg":"","data":[]}`

	got, err := marginRESTOver(f).MarginState(context.Background())
	if err != nil {
		t.Fatalf("MarginState: %v", err)
	}
	if got.ObservedAt.Year() < 2020 {
		t.Errorf("observedAt = %s — an absent uTime became the epoch, which reads as ordinary staleness "+
			"and silently disables every margin control on the account", got.ObservedAt)
	}
	if time.Since(got.ObservedAt) > time.Minute {
		t.Errorf("observedAt = %s, want the fetch clock", got.ObservedAt)
	}
}

// TestRESTClientSatisfiesTheSeam: the connector hands *okxREST out as the margin
// source, and nothing else checks that it still fits.
func TestRESTClientSatisfiesTheSeam(t *testing.T) {
	var _ VenueMarginSource = (*okxREST)(nil)
}

func mrat(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("bad rat %q", s)
	}
	return r
}
