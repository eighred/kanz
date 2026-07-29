// Package consume folds live commitment-lifecycle FACTs into the durable fund
// journal (PARITY-02d) — the seam ALT-01b carried forward. A CapitalCall /
// Distribution / NAVMark FACT decodes to an alternatives.Event and appends to
// the fund.Store; alternatives.Replay then reproduces the position, IRR, TVPI,
// and J-curve over live composition.
//
// # Decode seam
//
// The alternatives.v1 wire SDK is generated-not-committed (EVT-15a): the
// internal/alternatives package carries its own Go-native shapes and the service
// imports no alternativespb. So the payload decode is a seam — the default
// DecodeJSON folds the Go-native Event marshaled as JSON (the shape the
// in-process producer emits today), and the concrete alternatives.v1 proto
// decoder wires at the composition root once the SDK is generated. The fold,
// idempotency, and durability are identical either way.
package consume

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/alternatives"
	"github.com/eighred/kanz/services/alternatives/internal/fund"
)

// Decoder turns a FACT payload into a commitment lifecycle Event. The default is
// DecodeJSON; a composition root with the generated alternatives.v1 SDK swaps in
// a proto decoder behind this seam.
type Decoder func(payload []byte) (*alternatives.Event, error)

// DecodeJSON is the default Decoder: the Go-native alternatives.Event as JSON.
func DecodeJSON(payload []byte) (*alternatives.Event, error) {
	var e alternatives.Event
	if err := json.Unmarshal(payload, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// Folder is the bus.EventHandler that folds lifecycle FACTs into the fund
// journal. Construct once and pass Handle to bus.Consumer.Subscribe.
type Folder struct {
	store  fund.Store
	decode Decoder
}

// NewFolder wires a Folder to a durable fund.Store. A nil decoder defaults to
// DecodeJSON.
func NewFolder(store fund.Store, decode Decoder) (*Folder, error) {
	if store == nil {
		return nil, errors.New("consume: fund store is nil")
	}
	if decode == nil {
		decode = DecodeJSON
	}
	return &Folder{store: store, decode: decode}, nil
}

// Handle decodes one lifecycle FACT and appends it to the journal (idempotent on
// the event id). A non-nil return nacks/DLQs the delivery — a malformed or
// unappendable lifecycle event surfaces rather than being silently dropped.
func (f *Folder) Handle(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	e, err := f.decode(payload)
	if err != nil {
		return fmt.Errorf("consume: %s decode: %w", env.GetEventType(), err)
	}
	if e == nil || e.EventID == "" {
		return fmt.Errorf("consume: %s missing event_id", env.GetEventType())
	}
	return f.store.Append(ctx, e)
}
