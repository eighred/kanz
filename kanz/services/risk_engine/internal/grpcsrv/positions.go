package grpcsrv

import (
	"context"
	"errors"

	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
)

type positionServer struct {
	querypb.UnimplementedPositionQueryServiceServer
	engine      v1.PositionReader
	ownerTenant string
}

func (s *positionServer) Positions(ctx context.Context, req *querypb.PositionsRequest) (*querypb.PositionsResponse, error) {
	if s.engine == nil {
		return nil, status.Error(codes.Unimplemented, "position observations are unavailable")
	}
	resp, err := s.engine.Positions(ctx, v1.PositionsRequest{PortfolioID: v1.PortfolioID(req.GetPortfolioId()), PageSize: req.GetPageSize(), PageToken: req.GetPageToken()})
	if errors.Is(err, v1.ErrPositionSnapshotChanged) {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if errors.Is(err, v1.ErrPositionSnapshotBound) {
		return nil, status.Error(codes.ResourceExhausted, err.Error())
	}
	if errors.Is(err, v1.ErrPositionObservationInvalid) {
		return nil, status.Error(codes.DataLoss, err.Error())
	}
	if err != nil {
		return nil, mapError(err)
	}
	rows := make([]*querypb.ObservedPosition, 0, len(resp.Positions))
	for _, pos := range resp.Positions {
		quantity := &querypb.ObservedQuantity{Knowledge: querypb.StateReadKnowledge_STATE_READ_KNOWLEDGE_UNKNOWN}
		if pos.Quantity != nil {
			quantity.Knowledge = querypb.StateReadKnowledge_STATE_READ_KNOWLEDGE_SUPPORTED
			quantity.Value = pos.Quantity
		}
		var asOf *timestamppb.Timestamp
		if !pos.AsOf.IsZero() {
			asOf = timestamppb.New(pos.AsOf)
		}
		rows = append(rows, &querypb.ObservedPosition{InstrumentId: string(pos.InstrumentID), Quantity: quantity, AsOf: asOf})
	}
	var asOf *timestamppb.Timestamp
	if !resp.AsOf.IsZero() {
		asOf = timestamppb.New(resp.AsOf)
	}
	return &querypb.PositionsResponse{PortfolioId: string(resp.PortfolioID), OwnerTenant: s.ownerTenant, AsOf: asOf, Positions: rows,
		TotalObserved: resp.TotalObserved, NextPageToken: resp.NextPageToken, SnapshotId: resp.SnapshotID, QualityFlags: protoFlags(resp.QualityFlags)}, nil
}
