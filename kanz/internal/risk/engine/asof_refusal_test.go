package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
)

// A PINNED QUERY IS REFUSED, NOT ANSWERED FROM LIVE STATE (#859).
//
// `as_of` was declared on the wire, documented as working, parsed and validated
// by the gateway, forwarded through the gRPC server — and never read. Both
// queries answered from `store.Snapshot(id)`, the latest applied state, and
// stamped the response with that state's timestamp. "What were this portfolio's
// exposures last Tuesday" returned today's book: internally consistent,
// unflagged, and wrong.
//
// These tests hold the BEHAVIOUR. test/arch's
// TestEveryRiskRequestFieldIsReadByTheEngine holds the structure — that no
// request field is ignored — and deliberately accepts either resolution, so it
// would pass on a `_ = req.AsOf` that refused nothing. The refusal itself is
// only asserted here.

// asOfLastTuesday is a plausible historical pin: exactly the shape a
// reconciliation or a regulatory as-of report sends, and the one that used to
// come back as today's book.
func asOfLastTuesday() time.Time { return time.Now().Add(-7 * 24 * time.Hour) }

func TestExposureRefusesAnAsOfItCannotHonour(t *testing.T) {
	e, s := newEngine()
	applyPosition(t, s, "PORT-1", "AAPL", 1000, time.Now().Add(-1*time.Second))

	resp, err := e.Exposure(context.Background(), v1.ExposureRequest{
		PortfolioID: "PORT-1",
		AsOf:        asOfLastTuesday(),
	})
	if err == nil {
		t.Fatalf("a query pinned to last Tuesday was ANSWERED, with as_of %v — this is the live "+
			"book returned under a historical label, which is what #859 is", resp.AsOf)
	}
	if !errors.Is(err, v1.ErrAsOfNotSupported) {
		t.Fatalf("err = %v, want ErrAsOfNotSupported", err)
	}
	// The transport mapping is keyed on ErrInvalidRequest, so the refusal must
	// remain within that family or it reaches the caller as an opaque Internal
	// error — a 500 for a request the caller could fix.
	if !errors.Is(err, v1.ErrInvalidRequest) {
		t.Fatalf("err = %v does not wrap ErrInvalidRequest — grpcsrv maps that sentinel to "+
			"codes.InvalidArgument, and without it this refusal surfaces as an Internal error "+
			"and a 500, which tells the caller nothing it can act on", err)
	}
	// NO PARTIAL ANSWER. A refusal that still returned a populated response would
	// let a caller ignoring the error read live numbers as historical ones.
	if resp.Set != nil || !resp.AsOf.IsZero() || resp.PortfolioID != "" {
		t.Fatalf("the refusal carried a response: %+v — a caller that ignores the error would "+
			"read live state as the pinned answer", resp)
	}
}

func TestMeasuresRefusesAnAsOfItCannotHonour(t *testing.T) {
	e, s := newEngine()
	applyPosition(t, s, "PORT-1", "AAPL", 1000, time.Now().Add(-1*time.Second))

	resp, err := e.Measures(context.Background(), v1.MeasuresRequest{
		PortfolioID: "PORT-1",
		AsOf:        asOfLastTuesday(),
	})
	if err == nil {
		t.Fatalf("a pinned measures query was ANSWERED, with as_of %v — a MeasureSet carries VaR "+
			"and the sensitivities a desk hedges on, so this is today's risk under a historical "+
			"label", resp.AsOf)
	}
	if !errors.Is(err, v1.ErrAsOfNotSupported) {
		t.Fatalf("err = %v, want ErrAsOfNotSupported", err)
	}
	if !errors.Is(err, v1.ErrInvalidRequest) {
		t.Fatalf("err = %v does not wrap ErrInvalidRequest, so it maps to Internal not "+
			"InvalidArgument", err)
	}
	if resp.Set != nil || !resp.AsOf.IsZero() || resp.PortfolioID != "" {
		t.Fatalf("the refusal carried a response: %+v", resp)
	}
}

// THE REFUSAL MUST NOT BREAK THE ONLY QUERY THE ENGINE CAN ANSWER. A zero AsOf
// means "latest", which is what every first-party caller sends today
// (services/mcp/internal/riskread and services/copilot/internal/governed both
// build these requests without AsOf) — so a guard that refused on the zero value
// would take the risk read plane down entirely.
func TestAnUnpinnedQueryIsStillAnswered(t *testing.T) {
	e, s := newEngine()
	asOf := time.Now().Add(-1 * time.Second)
	applyPosition(t, s, "PORT-1", "AAPL", 1000, asOf)

	exp, err := e.Exposure(context.Background(), v1.ExposureRequest{PortfolioID: "PORT-1"})
	if err != nil {
		t.Fatalf("Exposure with no as_of: %v — the refusal is firing on the zero value and the "+
			"latest-state query, which is every real caller, is now refused", err)
	}
	if exp.Set == nil {
		t.Fatal("Exposure returned no set for an unpinned query")
	}

	meas, err := e.Measures(context.Background(), v1.MeasuresRequest{PortfolioID: "PORT-1"})
	if err != nil {
		t.Fatalf("Measures with no as_of: %v", err)
	}
	if meas.Set == nil {
		t.Fatal("Measures returned no set for an unpinned query")
	}
}

// AN AS_OF OF "NOW" IS STILL REFUSED, and this is the case worth stating.
//
// It is tempting to honour a pin that happens to fall at or after the latest
// applied state, on the reasoning that live state IS the answer. It is not: the
// engine cannot know whether an event between the caller's clock and its own is
// still in flight, so "as of now" answered from whatever has arrived is a claim
// about completeness the engine has no basis for. The refusal is about the
// engine's inability to reason about a point in time at all, not about how far
// back the point is.
func TestAnAsOfAtTheCurrentInstantIsAlsoRefused(t *testing.T) {
	e, s := newEngine()
	applyPosition(t, s, "PORT-1", "AAPL", 1000, time.Now().Add(-1*time.Second))

	_, err := e.Exposure(context.Background(), v1.ExposureRequest{
		PortfolioID: "PORT-1",
		AsOf:        time.Now(),
	})
	if !errors.Is(err, v1.ErrAsOfNotSupported) {
		t.Fatalf("err = %v, want ErrAsOfNotSupported — a pin at the current instant is still a "+
			"pin, and answering it from whatever has arrived asserts a completeness the engine "+
			"cannot establish", err)
	}
}

// THE REFUSAL PRECEDES OWNERSHIP AND EXISTENCE CHECKS ONLY WHERE IT MUST NOT
// CHANGE THEM. An empty PortfolioID is still ErrInvalidRequest rather than the
// as_of refusal, so the more specific diagnosis is not masked by the newer one.
func TestAnEmptyPortfolioIDStillReportsItsOwnError(t *testing.T) {
	e, _ := newEngine()

	_, err := e.Exposure(context.Background(), v1.ExposureRequest{AsOf: asOfLastTuesday()})
	if !errors.Is(err, v1.ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if errors.Is(err, v1.ErrAsOfNotSupported) {
		t.Fatal("an empty PortfolioID was reported as an as_of problem — the caller is sent to " +
			"fix the wrong field, and the two are independent defects in one request")
	}
}

// A PORTFOLIO THAT DOES NOT EXIST IS REPORTED AS AN AS_OF REFUSAL, and that
// ordering is deliberate rather than accidental.
//
// The as_of check runs before the store is consulted, so a pinned query for an
// unknown portfolio reports the as_of. That is the right way round: as_of is
// unsupported for EVERY portfolio, so it is the caller's first problem
// regardless, and reporting "not found" would send them to look for a portfolio
// whose existence would not have helped.
func TestAPinnedQueryForAnUnknownPortfolioReportsTheAsOf(t *testing.T) {
	e, _ := newEngine()

	_, err := e.Exposure(context.Background(), v1.ExposureRequest{
		PortfolioID: "NOPE",
		AsOf:        asOfLastTuesday(),
	})
	if !errors.Is(err, v1.ErrAsOfNotSupported) {
		t.Fatalf("err = %v, want ErrAsOfNotSupported", err)
	}
	if errors.Is(err, v1.ErrPortfolioNotFound) {
		t.Fatal("reported as not-found — the caller would go looking for a portfolio that would " +
			"not have been answerable at that instant even if it existed")
	}
}

// THE MESSAGE MUST NAME THE FIELD. #859's "Verified when" asks for a refusal
// "naming the field", because the whole defect was a caller who could not tell
// which part of their request was not working. An opaque "invalid request" would
// reproduce that at one remove.
func TestTheRefusalNamesTheFieldAndTheReason(t *testing.T) {
	e, _ := newEngine()
	_, err := e.Exposure(context.Background(), v1.ExposureRequest{
		PortfolioID: "PORT-1",
		AsOf:        asOfLastTuesday(),
	})
	if err == nil {
		t.Fatal("no error")
	}
	msg := err.Error()
	for _, want := range []string{"as_of", "live state", "859"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message does not mention %q — a caller reading it must learn which "+
				"field is unsupported, why answering it would be wrong, and where the work to "+
				"support it is tracked.\n\ngot: %s", want, msg)
		}
	}
}
