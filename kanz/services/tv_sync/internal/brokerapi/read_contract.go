package brokerapi

import (
	"net/http"

	"github.com/eighred/kanz/services/tv_sync/internal/projection"
)

type AccountsResponse struct {
	Accounts []projection.AccountDTO `json:"accounts"`
}
type PositionsResponse struct {
	Positions []projection.PositionDTO `json:"positions"`
}
type OrdersResponse struct {
	Orders       []projection.OrderDTO `json:"orders"`
	RetainedFrom string                `json:"retainedFrom,omitempty" broker:"horizon"`
}
type ExecutionsResponse struct {
	Executions   []projection.ExecutionDTO `json:"executions"`
	RetainedFrom string                    `json:"retainedFrom,omitempty" broker:"horizon"`
}

const (
	asOfQuery       = "as_of"
	instrumentQuery = "instrument"
)

// ReadOperation binds a registered read and its query parameters to its JSON response.
type ReadOperation struct {
	Name, Path string
	Query      []string
	Response   any
	serve      func(*Handler, http.ResponseWriter, *http.Request)
}

func AccountReads() []ReadOperation {
	const base = "/broker/accounts"
	return []ReadOperation{
		{"accounts", base, nil, AccountsResponse{}, (*Handler).accounts},
		{"state", base + "/{id}/state", []string{asOfQuery}, projection.StateDTO{}, (*Handler).state},
		{"positions", base + "/{id}/positions", []string{asOfQuery}, PositionsResponse{}, (*Handler).positions},
		{"orders", base + "/{id}/orders", []string{asOfQuery}, OrdersResponse{}, (*Handler).orders},
		{"executions", base + "/{id}/executions", []string{asOfQuery, instrumentQuery}, ExecutionsResponse{}, (*Handler).executions},
	}
}
