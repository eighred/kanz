package server

import (
	"context"
	"strings"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/mcp/internal/stateread"
)

// StateReader exposes only the two projected domain reads. In particular, an
// OrderState read is not the command-capable order.v1 client.
type StateReader interface {
	Positions(context.Context, string, uint32, string) (stateread.PositionPage, error)
	OrderState(context.Context, string, string) (stateread.OrderState, error)
}

// WithStateReads registers capabilities only after both backends were selected
// and validated at startup. Default construction retains the original tool set.
func WithStateReads(reader StateReader) Option {
	return func(s *Server) {
		if reader == nil {
			return
		}
		s.tools = append(s.tools, Tool{
			Name: "get_positions", Description: "Read a bounded snapshot of positions observed by the risk engine; not a complete accounting book.",
			Action:     auth.ActionRiskRead,
			properties: map[string]any{"page_size": map[string]any{"type": "integer", "minimum": 0, "maximum": 100}, "page_token": map[string]any{"type": "string", "maxLength": 128}},
			valid:      func(a Arguments) bool { return a.OrderID == "" && a.PageSize <= 100 && len(a.PageToken) <= 128 },
			invoke: func(ctx context.Context, _ Reader, a Arguments) (any, error) {
				return reader.Positions(ctx, a.PortfolioID, a.PageSize, a.PageToken)
			},
		}, Tool{
			Name: "get_order_state", Description: "Read one recorded OMS order state; not venue truth, an approval or an execution capability.",
			Action:     auth.Action("order.read"),
			properties: map[string]any{"order_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 256}},
			required:   []string{"order_id"},
			valid: func(a Arguments) bool {
				return a.OrderID != "" && len(a.OrderID) <= 256 && strings.TrimSpace(a.OrderID) == a.OrderID && a.PageSize == 0 && a.PageToken == ""
			},
			invoke: func(ctx context.Context, _ Reader, a Arguments) (any, error) {
				return reader.OrderState(ctx, a.PortfolioID, a.OrderID)
			},
		})
	}
}
