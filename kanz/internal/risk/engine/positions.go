package engine

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/dec"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/state"
)

const maxObservedPositions = 1024

// Positions reads one aggregate clone. Pagination revalidates the whole content
// identity, so a later page cannot silently mix positions from another version.
func (e *EngineImpl) Positions(ctx context.Context, req v1.PositionsRequest) (v1.PositionsResponse, error) {
	if err := ctx.Err(); err != nil {
		return v1.PositionsResponse{}, err
	}
	if req.PortfolioID == "" || req.PageSize > 100 || len(req.PageToken) > 128 {
		return v1.PositionsResponse{}, v1.ErrInvalidRequest
	}
	if err := e.refuseIfNotOwned(req.PortfolioID); err != nil {
		return v1.PositionsResponse{}, err
	}
	p, err := e.store.SnapshotOwned(req.PortfolioID)
	if errors.Is(err, state.ErrNotOwned) {
		return v1.PositionsResponse{}, v1.ErrPortfolioNotOwned
	}
	if err != nil {
		return v1.PositionsResponse{}, err
	}
	positions := p.Positions()
	if len(positions) > maxObservedPositions {
		return v1.PositionsResponse{}, v1.ErrPositionSnapshotBound
	}
	rows := make([]v1.ObservedPosition, 0, len(positions))
	for _, pos := range positions {
		if pos.InstrumentID == "" || len(pos.InstrumentID) > 256 {
			return v1.PositionsResponse{}, v1.ErrPositionObservationInvalid
		}
		var quantity *commonpb.Decimal
		if pos.Quantity != nil && dec.InDomain(pos.Quantity) {
			quantity = &commonpb.Decimal{Coefficient: pos.Quantity.GetCoefficient(), Exponent: pos.Quantity.GetExponent()}
		}
		rows = append(rows, v1.ObservedPosition{InstrumentID: pos.InstrumentID, Quantity: quantity, AsOf: pos.AsOf.UTC()})
	}
	_, flags := e.detector.Assess(p.AsOf())
	// JSON here is only an internal deterministic hash input. Decimal remains
	// integer coefficient/exponent; no floating conversion or source revaluation.
	bound, err := json.Marshal(struct {
		Portfolio v1.PortfolioID
		AsOf      string
		Rows      []v1.ObservedPosition
		Quality   []v1.QualityFlag
	}{req.PortfolioID, p.AsOf().UTC().Format("2006-01-02T15:04:05.999999999Z07:00"), rows, flags})
	if err != nil {
		return v1.PositionsResponse{}, err
	}
	digest := sha256.Sum256(bound)
	id := hex.EncodeToString(digest[:])
	offset := 0
	if req.PageToken != "" {
		raw, err := base64.RawURLEncoding.DecodeString(req.PageToken)
		if err != nil {
			return v1.PositionsResponse{}, v1.ErrInvalidRequest
		}
		prior, index, found := strings.Cut(string(raw), ":")
		n, parseErr := strconv.Atoi(index)
		if !found || len(prior) != 64 || parseErr != nil || n <= 0 || strconv.Itoa(n) != index {
			return v1.PositionsResponse{}, v1.ErrInvalidRequest
		}
		if prior != id {
			return v1.PositionsResponse{}, v1.ErrPositionSnapshotChanged
		}
		if n >= len(rows) {
			return v1.PositionsResponse{}, v1.ErrInvalidRequest
		}
		offset = n
	}
	size := int(req.PageSize)
	if size == 0 {
		size = 50
	}
	end := min(offset+size, len(rows))
	next := ""
	if end < len(rows) {
		next = base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%d", id, end)))
	}
	return v1.PositionsResponse{PortfolioID: req.PortfolioID, AsOf: p.AsOf(), Positions: rows[offset:end],
		TotalObserved: uint32(len(rows)), NextPageToken: next, SnapshotID: id, QualityFlags: flags}, nil
}
