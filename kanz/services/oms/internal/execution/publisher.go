package execution

import (
	"context"

	"github.com/kanz-eng/kanz/pkg/bus"
)

// Publisher is the bus publish surface — satisfied by *bus.Producer. It is
// defined here (untagged) because the composition root's build-tag-split venue
// selector references it in both builds; the Binance workers that consume it are
// compiled only under -tags binance.
type Publisher interface {
	Publish(ctx context.Context, e bus.Event) error
}
