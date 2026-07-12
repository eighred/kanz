package execution

import (
	"net/http"
	"time"

	"github.com/kanz-eng/kanz/internal/exchange/netdial"
)

// The DNS-bypass dialer moved to internal/exchange/netdial when market-ingest's
// depth websockets became its second consumer (the "second consumer ⇒ promote to
// top-level internal" rule). It stays stdlib-only, so promoting it put no vendor
// dependency into the default binary.

// NewExchangeHTTPClient builds the REST client the connectors use — DNS-cached,
// so a live placement or reconciliation call never blocks on resolution.
func NewExchangeHTTPClient(dnsTTL time.Duration) *http.Client {
	return netdial.NewHTTPClient(dnsTTL)
}
