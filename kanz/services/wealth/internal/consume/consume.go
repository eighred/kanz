// Package consume folds live account/holding state into the durable household
// book (PARITY-02e) — the seam WEALTH-01b carried forward. A household
// composition FACT decodes to a wealth.Household and is Put into the book.Store
// (last-write-wins), so householding aggregates run over live composition.
//
// # Decode seam
//
// The wealth.v1 wire SDK is generated-not-committed (EVT-15a): the
// internal/wealth package carries its own Go-native float shapes and the service
// imports no wealthpb. So the payload decode is a seam — the default DecodeJSON
// folds the Go-native Household as JSON (the shape the in-process producer emits
// today), and the concrete wealth.v1 proto decoder wires at the composition root
// once the SDK is generated.
package consume

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/wealth"
	"github.com/eighred/kanz/services/wealth/internal/book"
)

// Decoder turns a FACT payload into a household composition. The default is
// DecodeJSON; a composition root with the generated wealth.v1 SDK swaps in a
// proto decoder behind this seam.
type Decoder func(payload []byte) (wealth.Household, error)

// DecodeJSON is the default Decoder: the Go-native wealth.Household as JSON.
func DecodeJSON(payload []byte) (wealth.Household, error) {
	var h wealth.Household
	if err := json.Unmarshal(payload, &h); err != nil {
		return wealth.Household{}, err
	}
	return h, nil
}

// Folder is the bus.EventHandler that folds household composition FACTs into the
// book. Construct once and pass Handle to bus.Consumer.Subscribe.
type Folder struct {
	store  book.Store
	decode Decoder
}

// NewFolder wires a Folder to a durable book.Store. A nil decoder defaults to
// DecodeJSON.
func NewFolder(store book.Store, decode Decoder) (*Folder, error) {
	if store == nil {
		return nil, errors.New("consume: book store is nil")
	}
	if decode == nil {
		decode = DecodeJSON
	}
	return &Folder{store: store, decode: decode}, nil
}

// Handle decodes one composition FACT and Puts it into the book (last-write-wins
// on household id). A non-nil return nacks/DLQs the delivery.
func (f *Folder) Handle(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	h, err := f.decode(payload)
	if err != nil {
		return fmt.Errorf("consume: %s decode: %w", env.GetEventType(), err)
	}
	if h.HouseholdID == "" {
		return fmt.Errorf("consume: %s missing household_id", env.GetEventType())
	}
	return f.store.Put(ctx, h)
}
