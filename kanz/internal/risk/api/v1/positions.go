package v1

import (
	"context"
	"errors"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

// PositionReader is optional: an engine without observable position state must
// refuse this read rather than synthesize positions from exposure totals.
type PositionReader interface {
	Positions(context.Context, PositionsRequest) (PositionsResponse, error)
}

type PositionsRequest struct {
	PortfolioID PortfolioID
	PageSize    uint32
	PageToken   string
}

// ObservedPosition is one risk-state observation, not an authoritative holding.
// Nil Quantity means UNKNOWN, including malformed source decimals.
type ObservedPosition struct {
	InstrumentID InstrumentID
	Quantity     *commonpb.Decimal
	AsOf         time.Time
}

type PositionsResponse struct {
	PortfolioID   PortfolioID
	AsOf          time.Time
	Positions     []ObservedPosition
	TotalObserved uint32
	NextPageToken string
	SnapshotID    string
	QualityFlags  []QualityFlag
}

var (
	ErrPositionSnapshotChanged    = errors.New("risk: position snapshot changed; restart pagination")
	ErrPositionSnapshotBound      = errors.New("risk: observed position snapshot exceeds the supported bound")
	ErrPositionObservationInvalid = errors.New("risk: observed position identity is invalid")
)
