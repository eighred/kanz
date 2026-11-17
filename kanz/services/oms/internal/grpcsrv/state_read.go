package grpcsrv

import (
	"context"
	"errors"
	"sync"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/services/oms/internal/order"
)

// StateReader holds no write or approval method. The owning store still enforces
// tenant RLS; the separate listener admits only the declared MCP workload.
// Load must honor its context and release acquired store resources before it
// returns. Cancellation alone is not evidence that those resources were joined.
type StateReader interface {
	Load(context.Context, string) (*orderpb.OrderState, int64, error)
}

type StateReadServer struct {
	querypb.UnimplementedOrderStateReadServiceServer
	store     StateReader
	tenant    string
	now       func() time.Time
	lifecycle context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	active    chan struct{}
}

// StateReadMaxDuration bounds the whole read, including pool acquisition and
// network work. It cannot be raised by a caller or inherited RPC deadline.
const StateReadMaxDuration = 5 * time.Second

// ErrStateReadNotJoined forbids treating transport shutdown as store cleanup.
// A reader that ignores cancellation may still be using the execution pool.
var ErrStateReadNotJoined = errors.New("OMS state read accepted Load did not join")

// NewStateRead requires the owning OMS lifecycle, as context constructors do.
// A nil lifecycle panics instead of creating an unowned background operation.
func NewStateRead(root context.Context, store StateReader, tenant string, now func() time.Time) *StateReadServer {
	if now == nil {
		now = time.Now
	}
	lifecycle, cancel := context.WithCancel(root)
	return &StateReadServer{store: store, tenant: tenant, now: now, lifecycle: lifecycle, cancel: cancel}
}

// CancelReads closes admission and cancels accepted reads before the transport
// is stopped. This does not assert that a reader has actually returned.
func (s *StateReadServer) CancelReads() { s.cancel() }

// JoinReads is separate from grpc.Stop, which does not establish that an
// application reader has released its pool connection. Admission must be closed
// first; otherwise an idle instant would not establish joined shutdown.
func (s *StateReadServer) JoinReads(ctx context.Context) error {
	if s.lifecycle.Err() == nil {
		return errors.New("OMS state read lifecycle is still active")
	}
	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	if active == nil {
		return nil
	}
	select {
	case <-active:
		return nil
	case <-ctx.Done():
		return ErrStateReadNotJoined
	}
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
	ctx, cancel := context.WithTimeout(ctx, StateReadMaxDuration)
	defer cancel()
	unlink := context.AfterFunc(s.lifecycle, cancel)
	defer unlink()
	s.mu.Lock()
	if s.lifecycle.Err() != nil {
		s.mu.Unlock()
		return nil, status.Error(codes.Unavailable, "recorded state read is stopping")
	}
	if s.active != nil {
		s.mu.Unlock()
		// The shared pg.Service pool has four connections. Optional MCP work
		// may borrow at most one; it must not queue replicas behind execution.
		return nil, status.Error(codes.ResourceExhausted, "recorded state read is busy")
	}
	active := make(chan struct{})
	s.active = active
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active = nil
		close(active)
		s.mu.Unlock()
	}()
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	row, version, err := s.store.Load(ctx, req.GetOrderId())
	// A late successful read cannot erase a server timeout or lifecycle stop.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, status.FromContextError(ctxErr).Err()
	}
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
			out.Value = &commonpb.Decimal{Coefficient: value.GetCoefficient(), Exponent: value.GetExponent()}
		}
		return out
	}
	var asOf *timestamppb.Timestamp
	if row.GetAsOf() != nil && row.GetAsOf().CheckValid() == nil {
		asOf = timestamppb.New(row.GetAsOf().AsTime())
	}
	return &querypb.OrderStateReadResponse{PortfolioId: req.GetPortfolioId(), OwnerTenant: s.tenant,
		OrderId: row.GetOrderId(), InstrumentId: row.GetInstrumentId(), RecordedStatus: label, StatusKnowledge: knowledge,
		OrderedQuantity: quantity(row.GetOrderedQuantity()), FilledQuantity: quantity(row.GetFilledQuantity()), LeavesQuantity: quantity(row.GetLeavesQuantity()),
		AsOf: asOf, ReadAt: timestamppb.New(s.now().UTC()), Version: version, Quarantined: row.GetQuarantine() != nil}, nil
}
