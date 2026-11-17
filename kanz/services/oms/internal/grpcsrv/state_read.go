package grpcsrv

import (
	"context"
	"errors"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/services/oms/internal/order"
)

// StateReader holds no write or approval method. The owning store still enforces
// tenant RLS; the separate listener admits only the declared MCP workload.
type StateReader interface {
	Load(context.Context, string) (*orderpb.OrderState, int64, error)
}

type StateReadServer struct {
	querypb.UnimplementedOrderStateReadServiceServer
	store  StateReader
	tenant string
	now    func() time.Time
}

func NewStateRead(store StateReader, tenant string, now func() time.Time) *StateReadServer {
	if now == nil {
		now = time.Now
	}
	return &StateReadServer{store: store, tenant: tenant, now: now}
}

// OrderStateRead is a singular recorded-state read. It never queries a venue or
// exposes proposals, actor identities, account bindings, fees or recovery data.
func (s *StateReadServer) OrderStateRead(ctx context.Context, req *querypb.OrderStateReadRequest) (*querypb.OrderStateReadResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if req.GetPortfolioId() == "" || req.GetOrderId() == "" || len(req.GetPortfolioId()) > 256 || len(req.GetOrderId()) > 256 {
		return nil, status.Error(codes.InvalidArgument, "portfolio_id and order_id are required and bounded")
	}
	if s.tenant == "" || s.store == nil {
		return nil, status.Error(codes.FailedPrecondition, "recorded state is unavailable")
	}
	row, version, err := s.store.Load(ctx, req.GetOrderId())
	if errors.Is(err, order.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "recorded state not found")
	}
	if err != nil {
		return nil, status.Error(codes.Unavailable, "recorded state is unavailable")
	}
	if row == nil || row.GetPortfolioId() != req.GetPortfolioId() {
		return nil, status.Error(codes.NotFound, "recorded state not found")
	}
	if row.GetOrderId() != req.GetOrderId() || row.GetInstrumentId() == "" || len(row.GetInstrumentId()) > 256 || version < 0 {
		return nil, status.Error(codes.DataLoss, "recorded state identity is invalid")
	}
	knowledge := querypb.StateReadKnowledge_STATE_READ_KNOWLEDGE_UNKNOWN
	label := "UNKNOWN"
	_, known := orderpb.OrderStatus_name[int32(row.GetStatus())]
	if known && row.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_UNSPECIFIED && row.GetQuarantine() == nil && row.GetAsOf() != nil && row.GetAsOf().CheckValid() == nil {
		knowledge = querypb.StateReadKnowledge_STATE_READ_KNOWLEDGE_SUPPORTED
		label = row.GetStatus().String()
	}
	quantity := func(value *commonpb.Decimal) *querypb.ObservedQuantity {
		out := &querypb.ObservedQuantity{Knowledge: querypb.StateReadKnowledge_STATE_READ_KNOWLEDGE_UNKNOWN}
		if knowledge == querypb.StateReadKnowledge_STATE_READ_KNOWLEDGE_SUPPORTED && value != nil && dec.InDomain(value) {
			out.Knowledge = querypb.StateReadKnowledge_STATE_READ_KNOWLEDGE_SUPPORTED
			out.Value = proto.Clone(value).(*commonpb.Decimal)
		}
		return out
	}
	return &querypb.OrderStateReadResponse{PortfolioId: req.GetPortfolioId(), OwnerTenant: s.tenant,
		OrderId: row.GetOrderId(), InstrumentId: row.GetInstrumentId(), RecordedStatus: label, StatusKnowledge: knowledge,
		OrderedQuantity: quantity(row.GetOrderedQuantity()), FilledQuantity: quantity(row.GetFilledQuantity()), LeavesQuantity: quantity(row.GetLeavesQuantity()),
		AsOf: row.GetAsOf(), ReadAt: timestamppb.New(s.now().UTC()), Version: version, Quarantined: row.GetQuarantine() != nil}, nil
}
