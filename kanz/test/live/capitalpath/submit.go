package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/gatewaysig"
	"github.com/eighred/kanz/internal/orderid"
)

const maxGatewayResponse = 64 << 10

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type gatewayClient struct {
	http httpDoer
}

func newGatewayClient(client httpDoer) *gatewayClient {
	return &gatewayClient{http: client}
}

// submit sends exactly one request. It deliberately has no retry loop: a lost
// HTTP response says nothing about whether the canonical command was accepted.
// Resolution belongs to the order FACT/query path under the same id.
func (c *gatewayClient) submit(ctx context.Context, cfg config) (string, error) {
	id := orderid.Mint()
	return id, c.submitKnown(ctx, cfg, id)
}

func (c *gatewayClient) submitKnown(ctx context.Context, cfg config, id string) error {
	if c == nil || c.http == nil {
		return errors.New("capitalpath: gateway client is unavailable")
	}
	if err := orderid.Valid(id); err != nil {
		return fmt.Errorf("capitalpath: invalid order id: %w", err)
	}
	qty, err := dec.ParseRat(cfg.quantity)
	if err != nil {
		return fmt.Errorf("capitalpath: parse quantity: %w", err)
	}
	price, err := dec.ParseRat(cfg.limitPrice)
	if err != nil {
		return fmt.Errorf("capitalpath: parse limit price: %w", err)
	}
	qtyProto, ok := dec.ToProtoScaled(qty)
	if !ok {
		return errors.New("capitalpath: quantity cannot be represented as common.v1.Decimal")
	}
	priceProto, ok := dec.ToProtoScaled(price)
	if !ok {
		return errors.New("capitalpath: limit price cannot be represented as common.v1.Decimal")
	}
	order := &orderpb.SubmitOrder{
		OrderId:      id,
		PortfolioId:  cfg.portfolio,
		InstrumentId: cfg.instrument,
		Side:         orderpb.Side_SIDE_BUY,
		Quantity:     qtyProto,
		OrderType:    orderpb.OrderType_ORDER_TYPE_LIMIT,
		LimitPrice:   priceProto,
		TimeInForce:  orderpb.TimeInForce_TIME_IN_FORCE_DAY,
		Venue:        cfg.venue,
	}
	body, err := protojson.Marshal(order)
	if err != nil {
		return fmt.Errorf("capitalpath: encode order: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(cfg.gatewayURL, "/")+"/v1/orders", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("capitalpath: build gateway request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.token)
	req.Header.Set("Idempotency-Key", id)
	gatewaysig.SignRequest(req, []byte(cfg.gatewaySigningKey), body)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("capitalpath: gateway submit outcome is ambiguous; do not resubmit order %s: %w", id, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxGatewayResponse))
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("capitalpath: gateway refused order %s with HTTP %d: %s",
			id, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

// cancel is the fail-closed cleanup path for a submitted certification order.
// One named cancel is enough: the canonical aggregate and venue clOrdId make it
// idempotent, while an internal retry loop could hide an unhealthy gateway.
func (c *gatewayClient) cancel(ctx context.Context, cfg config, id string) error {
	if c == nil || c.http == nil {
		return errors.New("capitalpath: gateway client is unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(cfg.gatewayURL, "/")+"/v1/orders/"+id+"/cancel", nil)
	if err != nil {
		return fmt.Errorf("capitalpath: build cancel request for %s: %w", id, err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.token)
	gatewaysig.SignRequest(req, []byte(cfg.gatewaySigningKey), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("capitalpath: cancel outcome for %s is ambiguous and requires operator reconciliation: %w", id, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxGatewayResponse))
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("capitalpath: gateway refused cancel for %s with HTTP %d: %s", id, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}
