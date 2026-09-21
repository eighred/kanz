package grpcsrv

import (
	"context"
	"errors"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type feeReader interface {
	ReadFeeCorrection(context.Context, string, string) (*orderpb.GetExecutionFeeCorrectionResponse, error)
}

func (s *Server) GetExecutionFeeCorrection(ctx context.Context, req *orderpb.GetExecutionFeeCorrectionRequest) (*orderpb.GetExecutionFeeCorrectionResponse, error) {
	if req.GetPortfolioId() == "" || req.GetCaseId() == "" || len(req.GetCaseId()) > 256 {
		return nil, status.Error(codes.InvalidArgument, "portfolio_id and case_id are required")
	}
	r, ok := s.store.(feeReader)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "durable fee correction evidence is unavailable")
	}
	if s.ownerTenant == "" {
		return nil, status.Error(codes.FailedPrecondition, "OMS tenant is not configured")
	}
	result, err := r.ReadFeeCorrection(bus.WithTenantID(ctx, s.ownerTenant), req.PortfolioId, req.CaseId)
	if errors.Is(err, order.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "fee correction not found")
	}
	if err != nil {
		return nil, mapError(err)
	}
	result.OwnerTenant = s.ownerTenant
	return result, nil
}
