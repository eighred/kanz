// Package readbudget owns the fixed domain-RPC budget shared by the MCP clients.
// It is server policy, not an agent argument or a configurable request ceiling.
package readbudget

import (
	"context"
	"time"
)

const RPC = 3 * time.Second

// Context clips to an earlier caller deadline and propagates its cancellation.
func Context(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, RPC)
}
