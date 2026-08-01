// Package portfolio is the TUI's positions, PnL and risk surface (#84).
//
// It is a GATEWAY-plane pane: it runs in the shell's own process and reuses the
// SSO token the operator already holds, so it grants nothing they did not already
// have. See internal/tui/pane for why that distinction is a security property
// rather than a display hint.
//
// It reads two surfaces, because the platform keeps them apart:
//
//	positions + PnL  /v1/broker/accounts/{id}/positions   tv-sync's projection
//	risk measures    /v1/portfolios/{id}/measures         the risk engine
//
// They are fetched together and reported SEPARATELY. A risk engine that is down
// must not blank the positions an operator is watching, and vice versa — one
// failure blanking both is how a partial outage reads as a total one.
package portfolio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"google.golang.org/protobuf/encoding/protojson"

	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/tui/gateway"
)

// Position is one holding as the broker projection reports it.
//
// EVERY NUMBER STAYS A STRING. The projection already rendered these as exact
// decimal strings, and the only thing this pane does with them is print them —
// so parsing them into a float to display them would introduce the one
// representation this platform bans, in the surface a human reads the book from.
type Position struct {
	Instrument    string `json:"instrument"`
	Side          string `json:"side"`
	Qty           string `json:"qty"`
	AvgPrice      string `json:"avgPrice"`
	RealizedPnl   string `json:"realizedPl"`
	UnrealizedPnl string `json:"unrealizedPl"`
}

// Measure is one risk measure, already rendered for display.
type Measure struct {
	Name  string
	Value string
	// Unusable marks a measure whose Decimal could not be safely converted. It
	// is displayed as such rather than dropped — see Source.Measures.
	Unusable bool
}

// Source reads the two surfaces over the shared gateway client.
type Source struct {
	c *gateway.Client
}

// NewSource builds the reader. The client is shared with every other TUI surface
// (internal/tui/gateway) so the token and the request signature cannot drift.
func NewSource(c *gateway.Client) *Source { return &Source{c: c} }

// Positions returns the account's holdings with their realized and unrealized
// PnL, as of now.
func (s *Source) Positions(ctx context.Context, account string) ([]Position, error) {
	path := "/v1/broker/accounts/" + url.PathEscape(account) + "/positions"
	payload, err := s.c.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, brokerError(err, account)
	}
	// The broker surface is plain JSON DTOs, not protobuf — unlike the control
	// plane, which is why the shared client carries no codec.
	var body struct {
		Positions []Position `json:"positions"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, fmt.Errorf("decode positions: %w", err)
	}
	return body.Positions, nil
}

// Measures returns the portfolio's latest risk measures.
func (s *Source) Measures(ctx context.Context, portfolio string) ([]Measure, error) {
	path := "/v1/portfolios/" + url.PathEscape(portfolio) + "/measures"
	payload, err := s.c.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, riskError(err, portfolio)
	}
	// This surface IS protobuf — the gateway forwards the risk engine's own
	// message — so it decodes with protojson into the generated type rather than
	// a hand-written struct that would be a second definition of the contract.
	var resp querypb.MeasuresResponse
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(payload, &resp); err != nil {
		return nil, fmt.Errorf("decode measures: %w", err)
	}

	out := make([]Measure, 0, len(resp.GetSet().GetMeasures()))
	for _, m := range resp.GetSet().GetMeasures() {
		// CHECKED, NOT dec.FromProto (#95). These Decimals arrive over the wire and
		// their exponent is not this process's to trust: dec.FromProto materialises
		// 10^abs(exponent), so a measure carrying {1, 2000000000} would not render a
		// wrong number — it would never return, on the UI goroutine, and the whole
		// shell would appear frozen rather than showing an error.
		//
		// An unusable measure is SHOWN as unusable rather than dropped. A risk
		// figure that silently disappears from the list reads as "not computed",
		// which is a different and much calmer fact than "computed, and this
		// display cannot represent it".
		r, ok := dec.FromProtoChecked(m.GetValue())
		if !ok {
			out = append(out, Measure{Name: m.GetName(), Value: "unrepresentable", Unusable: true})
			continue
		}
		out = append(out, Measure{Name: m.GetName(), Value: dec.Str(r)})
	}
	return out, nil
}

// brokerError renders a failure from the broker surface in ITS OWN words.
//
// The shared client returns a typed *gateway.StatusError precisely so this can
// happen: the control plane's 404 message ("this gateway fronts no control
// plane") is wrong here and would send an operator to check a setting that has
// nothing to do with the broker routes.
func brokerError(err error, account string) error {
	var se *gateway.StatusError
	if !errors.As(err, &se) {
		return err
	}
	switch se.Status {
	case http.StatusNotFound:
		return fmt.Errorf("no account %q on this gateway — either the id is wrong, or it belongs "+
			"to another tenant (a cross-tenant read is NOT FOUND rather than denied, so these "+
			"look identical on purpose)", account)
	case http.StatusUnauthorized:
		return fmt.Errorf("the gateway rejected the token — it is expired, malformed, or issued "+
			"by another authority (%s)", se.Detail)
	case http.StatusForbidden:
		return errors.New("your account is authenticated but may not read this book")
	case http.StatusBadGateway, http.StatusGatewayTimeout:
		return fmt.Errorf("the gateway could not reach tv-sync (%d): the projection service may "+
			"be down, so positions and PnL are unavailable while risk may still be fine", se.Status)
	}
	return se
}

// riskError does the same for the risk surface. Split from brokerError because
// the two failures point at DIFFERENT services, and saying so is the whole
// value: "positions are stale" and "risk is stale" are separate incidents.
func riskError(err error, portfolio string) error {
	var se *gateway.StatusError
	if !errors.As(err, &se) {
		return err
	}
	switch se.Status {
	case http.StatusNotFound:
		return fmt.Errorf("no portfolio %q, or this gateway fronts no risk engine: it was started "+
			"without the risk query address, so the /v1/portfolios routes answer nothing", portfolio)
	case http.StatusForbidden:
		return errors.New("your account is authenticated but may not read this portfolio's risk")
	case http.StatusBadGateway, http.StatusGatewayTimeout:
		return fmt.Errorf("the gateway could not reach the risk engine (%d): measures are stale or "+
			"absent, but the positions above are served by a different service and may still be current",
			se.Status)
	}
	return se
}
