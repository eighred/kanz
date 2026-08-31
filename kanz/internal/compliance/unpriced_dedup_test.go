package compliance

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// ONE EVENT, ONE DEDUP POLICY (#883).
//
// The gate says an unpriced refusal once per (tenant, portfolio, instrument) and
// counts it always. Its observer used to be told nothing about that verdict, so
// the OMS composition root — which LOGS from that seam as well as counting —
// warned on every order while the gate beside it warned once. An operator saw one
// line from one and N from the other about the same refusal.
//
// The fix is not a second ledger in the caller. It is this: the gate consults its
// ledger once and hands the answer out, so anything downstream that chooses to
// speak speaks on exactly the orders the gate speaks on.

// sayLog captures WARN lines so a test can count what an operator would actually
// have seen, rather than assert on the ledger's internals.
func sayLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})), &buf
}

// unpricedSighting is one call the gate made into the observer.
type unpricedSighting struct {
	portfolioID  string
	instrumentID string
	first        bool
}

// THE OBSERVER IS TOLD ABOUT EVERY ORDER AND TOLD WHICH ONE IS THE FIRST.
//
// Both halves matter and they pull in opposite directions. A seam that fired
// only on first sightings would break the counter behind it — the composition
// root's kanz_compliance_unpriced_orders_total is a RATE, and a rate that counts
// distinct instruments is not the number anybody is watching during a price-feed
// outage. A seam that says nothing about first-ness leaves a caller that wants to
// log with no option but its own ledger, which is where the two policies came
// from.
func TestUnpricedObserverIsToldEveryOrderAndWhichOneIsFirst(t *testing.T) {
	logger, buf := sayLog()
	var seen []unpricedSighting
	g := NewPreTradeGate(nil, nil, nil, nil, nil, logger,
		WithUnpricedObserver(func(portfolioID, instrumentID string, first bool) {
			seen = append(seen, unpricedSighting{portfolioID, instrumentID, first})
		}))

	const orders = 5
	for i := 0; i < orders; i++ {
		g.noteUnpriced("tenant-a", "flagship", "NO-QUOTES")
	}

	if len(seen) != orders {
		t.Fatalf("the observer was called %d times for %d refused orders — a counter behind this "+
			"seam would stop being a rate and start being a count of distinct instruments",
			len(seen), orders)
	}
	if !seen[0].first {
		t.Error("the FIRST refusal was not reported as the first sighting — a caller gating its log " +
			"on this would never announce the condition at all")
	}
	for i, s := range seen[1:] {
		if s.first {
			t.Errorf("refusal %d was also reported as a first sighting — a caller that logs on this "+
				"is back to a line per order, which is the flood #883 names", i+2)
		}
	}

	if got := strings.Count(buf.String(), "no usable price"); got != 1 {
		t.Fatalf("the gate itself wrote %d WARN lines for %d orders on one pair, want 1", got, orders)
	}
}

// THE VERDICT HANDED OUT IS THE ONE THE GATE ACTED ON — not an approximation of
// it. If the two ever diverged, an operator would get a gate line with no
// diagnosis beside it, or a diagnosis with no refusal line, and would have no way
// to tell which.
func TestUnpricedObserverVerdictMatchesTheGatesOwnWarning(t *testing.T) {
	logger, buf := sayLog()
	spoke := 0
	g := NewPreTradeGate(nil, nil, nil, nil, nil, logger,
		WithUnpricedObserver(func(_, _ string, first bool) {
			if first {
				spoke++
			}
		}))

	for i := 0; i < 3; i++ {
		for j := 0; j < 4; j++ {
			g.noteUnpriced("tenant-a", "flagship", fmt.Sprintf("INSTRUMENT-%d", i))
		}
	}

	lines := strings.Count(buf.String(), "no usable price")
	if lines != 3 {
		t.Fatalf("the gate warned %d times about 3 unpriced instruments, want 3", lines)
	}
	if spoke != lines {
		t.Fatalf("the observer was told 'first' %d times while the gate warned %d times — the two "+
			"hold different dedup policies again, which is the defect #883 filed", spoke, lines)
	}
}

// THE VERDICT IS PER TENANT, WHICH IS WHY IT IS PASSED AND NOT RECOMPUTED.
//
// The observer is handed a portfolio and an instrument and NO tenant. A ledger
// built on that side could only be keyed (portfolio, instrument), so the first
// tenant to hit an unpriced instrument would silence the announcement for every
// other tenant on the pod — #243, one seam out, on a shared-name portfolio like
// "flagship". The gate's ledger has the tenant in the key, so sharing its verdict
// is the only version of this that stays tenant-safe.
func TestUnpricedFirstSightingIsPerTenant(t *testing.T) {
	logger, buf := sayLog()
	var firsts []string
	g := NewPreTradeGate(nil, nil, nil, nil, nil, logger,
		WithUnpricedObserver(func(portfolioID, instrumentID string, first bool) {
			if first {
				firsts = append(firsts, portfolioID+"/"+instrumentID)
			}
		}))

	g.noteUnpriced("tenant-a", "flagship", "NO-QUOTES")
	g.noteUnpriced("tenant-b", "flagship", "NO-QUOTES")

	if len(firsts) != 2 {
		t.Fatalf("two tenants refusing the same instrument on identically-named portfolios produced "+
			"%d first sightings, want 2 — tenant B's outage is the one nobody would be told about "+
			"(#243)", len(firsts))
	}
	if got := strings.Count(buf.String(), "no usable price"); got != 2 {
		t.Fatalf("the gate warned %d times, want 2 — the observer and the gate must agree", got)
	}
}

// A NIL OBSERVER IS STILL A GATE THAT SPEAKS. The seam is optional, and the
// refusal must be audible on a deployment that wires nothing to it — that is the
// reason the gate keeps its own WARN rather than delegating the whole log to a
// caller-supplied hook.
func TestUnpricedIsAudibleWithNoObserverWired(t *testing.T) {
	logger, buf := sayLog()
	g := NewPreTradeGate(nil, nil, nil, nil, nil, logger)

	g.noteUnpriced("tenant-a", "flagship", "NO-QUOTES")

	if !strings.Contains(buf.String(), "no usable price") {
		t.Fatal("a gate with no unpriced observer said nothing about a refusal it made — an order " +
			"that was never evaluated must not look like one that passed compliance")
	}
}
