// Package stateread projects authenticated domain reads without importing any
// command-capable order/venue contract or a domain store.
package stateread

import (
	"context"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/agentgate"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/pkg/auth"
)

type Decimal struct {
	Coefficient string `json:"coefficient"`
	Exponent    int32  `json:"exponent"`
}

type Quantity struct {
	Status string   `json:"status"`
	Value  *Decimal `json:"value,omitempty"`
}

type Position struct {
	InstrumentID string   `json:"instrument_id"`
	Quantity     Quantity `json:"quantity"`
	AsOf         string   `json:"as_of,omitempty"`
}

type PositionPage struct {
	PortfolioID   string     `json:"portfolio_id"`
	Scope         string     `json:"scope"`
	AsOf          string     `json:"as_of,omitempty"`
	Positions     []Position `json:"positions"`
	TotalObserved uint32     `json:"total_observed"`
	NextPageToken string     `json:"next_page_token,omitempty"`
	SnapshotID    string     `json:"snapshot_id"`
	QualityFlags  []string   `json:"quality_flags"`
}

type OrderState struct {
	PortfolioID    string   `json:"portfolio_id"`
	OrderID        string   `json:"order_id"`
	InstrumentID   string   `json:"instrument_id"`
	Scope          string   `json:"scope"`
	RecordedStatus string   `json:"recorded_status"`
	Status         string   `json:"status"`
	Ordered        Quantity `json:"ordered_quantity"`
	Filled         Quantity `json:"filled_quantity"`
	Leaves         Quantity `json:"leaves_quantity"`
	AsOf           string   `json:"as_of,omitempty"`
	ReadAt         string   `json:"read_at,omitempty"`
	Version        string   `json:"version"`
	Quarantined    bool     `json:"quarantined"`
}

type Client struct {
	positions querypb.PositionQueryServiceClient
	orders    querypb.OrderStateReadServiceClient
	tenant    string
}

func New(positions querypb.PositionQueryServiceClient, orders querypb.OrderStateReadServiceClient, tenant string) (*Client, error) {
	if positions == nil || orders == nil || tenant == "" {
		return nil, errors.New("state reads require both domain clients and an owning tenant")
	}
	return &Client{positions: positions, orders: orders, tenant: tenant}, nil
}

func (c *Client) allowed(ctx context.Context, portfolio string) bool {
	p, ok := auth.PrincipalFromContext(ctx)
	return ok && p.Subject != "" && p.Tenant == c.tenant && auth.PortfolioInScope(p.Portfolios, portfolio)
}

func (c *Client) Positions(ctx context.Context, portfolio string, size uint32, token string) (PositionPage, error) {
	if !c.allowed(ctx, portfolio) {
		return PositionPage{}, agentgate.ErrResourceNotVisible
	}
	if size > 100 || len(token) > 128 {
		return PositionPage{}, errors.New("invalid bounded position page")
	}
	resp, err := c.positions.Positions(ctx, &querypb.PositionsRequest{PortfolioId: portfolio, PageSize: size, PageToken: token})
	if err != nil {
		return PositionPage{}, readError(err)
	}
	if resp.GetOwnerTenant() != c.tenant || resp.GetPortfolioId() != portfolio {
		return PositionPage{}, agentgate.ErrResourceNotVisible
	}
	limit := int(size)
	if limit == 0 {
		limit = 50
	}
	if len(resp.GetPositions()) > limit || resp.GetTotalObserved() > 1024 || uint32(len(resp.GetPositions())) > resp.GetTotalObserved() || len(resp.GetSnapshotId()) != 64 || len(resp.GetNextPageToken()) > 128 {
		return PositionPage{}, errors.New("domain position response violates the bounded snapshot contract")
	}
	if _, err := hex.DecodeString(resp.GetSnapshotId()); err != nil {
		return PositionPage{}, errors.New("invalid position snapshot identity")
	}
	out := PositionPage{PortfolioID: portfolio, Scope: "OBSERVED_RISK_STATE", AsOf: stamp(resp.GetAsOf()),
		Positions: make([]Position, 0, len(resp.GetPositions())), TotalObserved: resp.GetTotalObserved(),
		NextPageToken: resp.GetNextPageToken(), SnapshotID: resp.GetSnapshotId(), QualityFlags: []string{}}
	usable := out.AsOf != "" && len(resp.GetQualityFlags()) == 0
	for _, flag := range resp.GetQualityFlags() {
		out.QualityFlags = append(out.QualityFlags, flag.String())
	}
	previous := ""
	for _, row := range resp.GetPositions() {
		if row == nil || row.GetInstrumentId() == "" || row.GetInstrumentId() <= previous {
			return PositionPage{}, errors.New("domain position identity is unavailable")
		}
		previous = row.GetInstrumentId()
		asOf := stamp(row.GetAsOf())
		out.Positions = append(out.Positions, Position{InstrumentID: row.GetInstrumentId(), Quantity: quantity(row.GetQuantity(), usable && asOf != ""), AsOf: asOf})
	}
	return out, nil
}

func (c *Client) OrderState(ctx context.Context, portfolio, orderID string) (OrderState, error) {
	if !c.allowed(ctx, portfolio) {
		return OrderState{}, agentgate.ErrResourceNotVisible
	}
	resp, err := c.orders.OrderStateRead(ctx, &querypb.OrderStateReadRequest{PortfolioId: portfolio, OrderId: orderID})
	if err != nil {
		return OrderState{}, readError(err)
	}
	if resp.GetOwnerTenant() != c.tenant || resp.GetPortfolioId() != portfolio || resp.GetOrderId() != orderID {
		return OrderState{}, agentgate.ErrResourceNotVisible
	}
	asOf, readAt := stamp(resp.GetAsOf()), stamp(resp.GetReadAt())
	usable := resp.GetStatusKnowledge() == querypb.StateReadKnowledge_STATE_READ_KNOWLEDGE_SUPPORTED && !resp.GetQuarantined() && asOf != "" && readAt != "" && resp.GetVersion() >= 0 && supportedStatus(resp.GetRecordedStatus())
	state, knowledge := "UNKNOWN", "UNKNOWN"
	if usable {
		state, knowledge = resp.GetRecordedStatus(), "SUPPORTED"
	}
	return OrderState{PortfolioID: portfolio, OrderID: orderID, InstrumentID: resp.GetInstrumentId(), Scope: "RECORDED_OMS_STATE",
		RecordedStatus: state, Status: knowledge, Ordered: quantity(resp.GetOrderedQuantity(), usable), Filled: quantity(resp.GetFilledQuantity(), usable),
		Leaves: quantity(resp.GetLeavesQuantity(), usable), AsOf: asOf, ReadAt: readAt, Version: strconv.FormatInt(resp.GetVersion(), 10), Quarantined: resp.GetQuarantined()}, nil
}

// The projection permits reviewed public lifecycle labels; future labels remain
// UNKNOWN until this read contract is reviewed, without importing order commands.
func supportedStatus(label string) bool {
	switch label {
	case "ORDER_STATUS_PENDING_NEW", "ORDER_STATUS_ROUTED", "ORDER_STATUS_PARTIALLY_FILLED", "ORDER_STATUS_FILLED", "ORDER_STATUS_CANCELLED", "ORDER_STATUS_REJECTED", "ORDER_STATUS_EXPIRED", "ORDER_STATUS_WORKING_SCHEDULED":
		return true
	default:
		return false
	}
}

func quantity(q *querypb.ObservedQuantity, usable bool) Quantity {
	out := Quantity{Status: "UNKNOWN"}
	if !usable || q == nil {
		return out
	}
	if q.GetKnowledge() == querypb.StateReadKnowledge_STATE_READ_KNOWLEDGE_UNSUPPORTED {
		out.Status = "UNSUPPORTED"
		return out
	}
	if q.GetKnowledge() == querypb.StateReadKnowledge_STATE_READ_KNOWLEDGE_SUPPORTED && q.GetValue() != nil && dec.InDomain(q.GetValue()) {
		out.Status = "SUPPORTED"
		out.Value = &Decimal{Coefficient: strconv.FormatInt(q.GetValue().GetCoefficient(), 10), Exponent: q.GetValue().GetExponent()}
	}
	return out
}

func stamp(t *timestamppb.Timestamp) string {
	if t == nil || t.CheckValid() != nil {
		return ""
	}
	return t.AsTime().UTC().Format(time.RFC3339Nano)
}

func readError(err error) error {
	if status.Code(err) == codes.NotFound {
		return agentgate.ErrResourceNotVisible
	}
	return err
}
