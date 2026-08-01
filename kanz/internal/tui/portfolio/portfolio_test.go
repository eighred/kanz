package portfolio

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/tui/gateway"
	"github.com/eighred/kanz/internal/tui/pane"
)

// The shell routes panes through the interface, so a widened contract would
// otherwise fail silently at the type assertion rather than at the build.
var _ pane.Pane = Model{}

func srcOver(t *testing.T, h http.HandlerFunc) (*Source, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	c, err := gateway.New(gateway.Config{BaseURL: srv.URL, Token: "tok"})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	return NewSource(c), srv.Close
}

func ctxWithDeadline(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

const measuresJSON = `{"portfolioId":"pf-1","set":{"portfolioId":"pf-1","measures":[
  {"name":"VaR99","value":{"coefficient":125000,"exponent":-2}},
  {"name":"GrossExposure","value":{"coefficient":80000,"exponent":0}}]}}`

func TestPositionsCarryPnLThrough(t *testing.T) {
	src, done := srcOver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/broker/accounts/fund-alpha/positions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"positions":[
		  {"instrument":"BTC-USD","side":"long","qty":"1.5","avgPrice":"50000.25",
		   "realizedPl":"120.50","unrealizedPl":"-45.25"}]}`))
	})
	defer done()

	got, err := src.Positions(ctxWithDeadline(t), "fund-alpha")
	if err != nil {
		t.Fatalf("Positions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d positions, want 1", len(got))
	}
	p := got[0]
	// EVERY NUMBER STAYS A STRING. The projection already rendered these exactly;
	// parsing them to display them would introduce the one representation this
	// platform bans, in the surface a human reads the book from.
	if p.Qty != "1.5" || p.AvgPrice != "50000.25" {
		t.Errorf("qty/avg = %q/%q, want 1.5/50000.25", p.Qty, p.AvgPrice)
	}
	if p.RealizedPnl != "120.50" || p.UnrealizedPnl != "-45.25" {
		t.Errorf("pnl = %q/%q — PnL must survive the read unaltered", p.RealizedPnl, p.UnrealizedPnl)
	}
}

func TestMeasuresRenderExactDecimals(t *testing.T) {
	src, done := srcOver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/portfolios/pf-1/measures" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(measuresJSON))
	})
	defer done()

	got, err := src.Measures(ctxWithDeadline(t), "pf-1")
	if err != nil {
		t.Fatalf("Measures: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d measures, want 2", len(got))
	}
	if got[0].Name != "VaR99" || got[0].Value != "1250" {
		t.Errorf("measure[0] = %q %q, want VaR99 1250", got[0].Name, got[0].Value)
	}
	if got[1].Value != "80000" {
		t.Errorf("measure[1] = %q, want 80000", got[1].Value)
	}
}

// A MEASURE WITH AN ABSURD EXPONENT MUST NOT HANG THE UI (#95).
//
// These Decimals arrive over the wire and their exponent is not this process's to
// trust. dec.FromProto materialises 10^abs(exponent), so {1, 2000000000} would
// never return — on the UI goroutine, making the whole shell look frozen rather
// than showing an error. The checked conversion refuses it instead, and the
// measure is SHOWN as unrepresentable rather than dropped: a risk figure that
// silently disappears reads as "not computed", which is a much calmer fact.
func TestAnAbsurdMeasureExponentIsRefusedNotRendered(t *testing.T) {
	src, done := srcOver(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"set":{"measures":[
		  {"name":"VaR99","value":{"coefficient":1,"exponent":2000000000}},
		  {"name":"Delta","value":{"coefficient":42,"exponent":0}}]}}`))
	})
	defer done()

	type res struct {
		ms  []Measure
		err error
	}
	done2 := make(chan res, 1)
	go func() {
		ms, err := src.Measures(ctxWithDeadline(t), "pf-1")
		done2 <- res{ms, err}
	}()

	select {
	case r := <-done2:
		if r.err != nil {
			t.Fatalf("Measures: %v", r.err)
		}
		if len(r.ms) != 2 {
			t.Fatalf("got %d measures, want 2 — the unusable one must be SHOWN, not dropped", len(r.ms))
		}
		if !r.ms[0].Unusable || r.ms[0].Value != "unrepresentable" {
			t.Errorf("measure[0] = %+v, want it marked unusable", r.ms[0])
		}
		// NON-VACUITY: the good measure beside it still renders.
		if r.ms[1].Unusable || r.ms[1].Value != "42" {
			t.Errorf("measure[1] = %+v, want 42 rendered normally", r.ms[1])
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Measures did not return within 4s — an out-of-domain exponent is being " +
			"materialised on the UI goroutine and the shell would appear frozen")
	}
}

// THE TWO HALVES FAIL INDEPENDENTLY.
//
// A risk engine that is down must not blank the positions an operator is
// watching. Blanking both would report a partial outage as a total one.
func TestOneSurfaceFailingLeavesTheOtherOnScreen(t *testing.T) {
	src, done := srcOver(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/measures") {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"risk engine unreachable"}`))
			return
		}
		_, _ = w.Write([]byte(`{"positions":[{"instrument":"BTC-USD","side":"long","qty":"1","avgPrice":"50000","realizedPl":"0"}]}`))
	})
	defer done()

	m := Model{src: src, cfg: Config{Account: "fund-alpha", Portfolio: "pf-1"}.withDefaults()}
	msg, ok := m.fetch().(fetchMsg)
	if !ok {
		t.Fatal("fetch did not return a fetchMsg")
	}

	if msg.posErr != nil {
		t.Fatalf("positions failed while only risk was down: %v", msg.posErr)
	}
	if len(msg.positions) != 1 {
		t.Fatalf("got %d positions, want 1 — a risk outage blanked the book", len(msg.positions))
	}
	if msg.riskErr == nil {
		t.Fatal("a 502 from the risk surface was not reported")
	}
	// And the message must point at the RIGHT service, not the gateway generally.
	if !strings.Contains(msg.riskErr.Error(), "risk engine") {
		t.Errorf("risk error %q does not name the risk engine — an operator cannot tell which "+
			"service to look at", msg.riskErr)
	}
}

// The two 404s must not share wording: on the broker surface it means the
// account is wrong or belongs to another tenant; on the risk surface it means
// the gateway may front no risk engine at all.
func TestTheTwoSurfacesExplainA404Differently(t *testing.T) {
	src, done := srcOver(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	defer done()

	_, posErr := src.Positions(ctxWithDeadline(t), "fund-alpha")
	_, riskErr := src.Measures(ctxWithDeadline(t), "pf-1")
	if posErr == nil || riskErr == nil {
		t.Fatal("a 404 was not reported on one of the surfaces")
	}
	if !strings.Contains(posErr.Error(), "another tenant") {
		t.Errorf("broker 404 = %q; it should say a cross-tenant read looks identical to a wrong id", posErr)
	}
	if !strings.Contains(riskErr.Error(), "risk engine") {
		t.Errorf("risk 404 = %q; it should say the gateway may front no risk engine", riskErr)
	}
	if posErr.Error() == riskErr.Error() {
		t.Error("both surfaces gave the same 404 message — the point of the typed StatusError is " +
			"that each renders the status in its own words")
	}
}

// A pane that cannot build its source shows the remedy and keeps retrying, rather
// than taking the shell down — the shell opens before anyone has signed in.
func TestAnUnbuildableSourceRendersTheRemedyAndRetries(t *testing.T) {
	m := New(func() (*Source, error) { return nil, errors.New("not signed in") },
		Config{Account: "a", Portfolio: "p"})

	got, cmd := m.Update(startMsg{})
	if cmd == nil {
		t.Fatal("no retry was scheduled — the pane would never recover after a sign-in")
	}
	view := got.View(100, 20)
	if !strings.Contains(view, "not signed in") {
		t.Errorf("view %q does not show why the pane is empty", view)
	}
}

// NON-VACUITY for the view: a loaded pane renders its positions and measures.
func TestViewRendersBothTables(t *testing.T) {
	m := Model{
		loaded:    true,
		lastAt:    time.Unix(0, 0),
		positions: []Position{{Instrument: "BTC-USD", Side: "long", Qty: "1.5", AvgPrice: "50000", RealizedPnl: "10"}},
		measures:  []Measure{{Name: "VaR99", Value: "1250"}},
	}
	view := m.View(120, 30)
	for _, want := range []string{"POSITIONS", "BTC-USD", "1.5", "RISK", "VaR99", "1250"} {
		if !strings.Contains(view, want) {
			t.Errorf("view is missing %q:\n%s", want, view)
		}
	}
	// An absent unrealized PnL is a dash, not an empty column — absent means the
	// projection had no mark, which is not the same as zero.
	if !strings.Contains(view, "—") {
		t.Error("an absent unrealizedPl did not render as a dash")
	}
}
