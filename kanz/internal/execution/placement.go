package execution

import (
	"context"
	"time"
)

// PlacementRecoveryTimeout bounds the one read that resolves an ambiguous
// placement. It is deliberately independent of the placement deadline: once a
// request may have reached an exchange, cancellation is no longer evidence that
// no order exists. The caller must ask by the deterministic client order id
// before it can safely return control to a retrying delivery.
const PlacementRecoveryTimeout = 5 * time.Second

// NewPlacementRecoveryContext preserves tracing and tenancy values from parent
// while detaching its cancellation and deadline. A placement timeout is the
// trigger for this query, so inheriting that timeout would cancel the recovery
// before it reached the venue. The new deadline keeps shutdown and a failed
// exchange from leaking a goroutine or holding an execution worker forever.
func NewPlacementRecoveryContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(parent), PlacementRecoveryTimeout)
}
