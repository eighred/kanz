package store

import (
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/datamaster/internal/pricing"
)

// OverrideEventForTest exposes the unexported event builder to the external
// publish_test package, so the Tier-B envelope test runs the SAME builder
// PostgresExceptions.Override enqueues rather than a copy of it.
//
// A FUNCTION, NOT A VAR. A test-only mutable global is still a global: it is
// writable from any test in the package, and the resulting data race is
// undetectable here because -race needs cgo and this box has none.
func OverrideEventForTest(ex pricing.Exception, o pricing.Override) (bus.Event, error) {
	return overrideEvent(ex, o)
}
