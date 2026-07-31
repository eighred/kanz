package ingest

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"

	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/signal/translate"
)

// ErrPositionsNotArmed: the position book has not been learned yet.
//
// It is deliberately an ERROR and not a zero. "I have not replayed the book" and "the fund
// holds nothing" are different facts, and rendering the first as the second is exactly how a
// CLOSE signal gets swallowed: a flat position sizes a flatten order at zero, the fan-out
// skips the leg, and the webhook answers 202 Accepted for an order it never sent.
//
// The pipeline maps this to a retryable failure and RELEASES the nonce (EXEC-M17), so the
// alert can be re-delivered rather than burned — and webhook-ingest's readiness holds the pod
// out of its Service until the cache is armed, so in practice no traffic arrives before then.
var ErrPositionsNotArmed = errors.New("ingest: position book not armed — the fund's holdings have not been learned yet")

// PositionCache is the fund's per-venue book, folded from the position FACTs the OMS
// publishes (EXEC-M19a) and armed at boot from the compacted POSITION stream (EXEC-M20).
//
// It replaces StaticPositions{} — an EMPTY MAP wired into production with the comment
// "M1 sim: flat by default; M3+ binds the OMS projection". M3+ never bound it, so
// translate.legSizeAndSide sized every CLOSE leg from a position of zero, the fan-out
// skipped every leg, and a `close` alert produced NO ORDERS AT ALL. The trading loop could
// open a position and could not close one.
//
// PER VENUE, and that is the whole point: you cannot sell 1 BTC on Binance if it is sitting
// at OKX, so a CLOSE must flatten what EACH VENUE actually holds.
type PositionCache struct {
	mu     sync.RWMutex
	armed  bool
	byKey  map[string]*big.Rat // fund/venue/instrument → signed quantity
	onceOn sync.Once
}

// NewPositionCache returns an UNARMED cache. It refuses to answer until it has replayed the
// book, because a cache that has learned nothing and a fund that holds nothing are not the
// same thing.
func NewPositionCache() *PositionCache {
	return &PositionCache{byKey: map[string]*big.Rat{}}
}

var _ translate.PositionSource = (*PositionCache)(nil)

// Arm marks the initial replay complete: from here the cache has seen the current state of
// every holding, so an instrument it has no entry for is genuinely not held. Idempotent —
// the bus calls it once, but a second call must not reopen the question.
func (c *PositionCache) Arm() {
	c.onceOn.Do(func() {
		c.mu.Lock()
		c.armed = true
		c.mu.Unlock()
	})
}

// Armed reports whether the book has been learned. webhook-ingest's readiness gates on it:
// a pod that does not know what the fund holds must not be handed a signal that closes it.
func (c *PositionCache) Armed() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.armed
}

// Handle folds one per-venue PositionState FACT. It is the bus.EventHandler for
// subject.VenuePositionAll.
//
// The FACT carries the ABSOLUTE holding, so this SETS rather than adds — including when the
// holding is flattened, where the OMS publishes a quantity of zero and the compacted stream
// retains it. Keeping the old size instead would have a CLOSE sell something the fund no
// longer has.
func (c *PositionCache) Handle(_ context.Context, _ *envelopepb.Envelope, payload []byte) error {
	var ps domainpb.PositionState
	if err := proto.Unmarshal(payload, &ps); err != nil {
		return err // malformed control state: nack rather than fold a position we cannot read
	}
	// Same reason as the nack above, one layer in (#95): a quantity whose exponent is
	// out of domain is a holding this cache cannot read. Folding it is strictly worse
	// than nacking — dec.FromProto would materialise 10^abs(exponent) and never return,
	// stalling the position subscription every CLOSE is sized from.
	if field, in := dec.InDomainDeep(&ps); !in {
		return fmt.Errorf("position %s/%s/%s carries an out-of-domain exponent at %s",
			ps.GetPortfolioId(), ps.GetVenue(), ps.GetInstrumentId(), field)
	}
	if ps.GetPortfolioId() == "" || ps.GetVenue() == "" || ps.GetInstrumentId() == "" {
		// A holding that cannot say whose it is, where it sits, or what it is, is not a
		// holding this cache can ever answer a CLOSE from.
		return nil
	}
	c.mu.Lock()
	c.byKey[positionKey(ps.GetPortfolioId(), ps.GetVenue(), ps.GetInstrumentId())] = dec.FromProto(ps.GetQuantity())
	c.mu.Unlock()
	return nil
}

// Position is the translate.PositionSource seam: the signed quantity this fund holds of this
// instrument AT THIS VENUE. A CLOSE leg is sized from it.
func (c *PositionCache) Position(_ context.Context, fundID, venue, instrumentID string) (*big.Rat, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.armed {
		return nil, ErrPositionsNotArmed
	}
	if q, ok := c.byKey[positionKey(fundID, venue, instrumentID)]; ok {
		return new(big.Rat).Set(q), nil
	}
	// Armed, and no entry: the compacted stream carries every holding, so this one does not
	// exist. The fund is flat here, and closing it is correctly a no-op.
	return new(big.Rat), nil
}

func positionKey(fundID, venue, instrumentID string) string {
	return fundID + "/" + venue + "/" + instrumentID
}
