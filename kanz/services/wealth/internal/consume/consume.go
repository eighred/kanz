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

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/pkg/bus"

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
	// tenant is the tenant this folder serves; Handle refuses any other (#223).
	tenant string
	store  book.Store
	decode Decoder
	// drift observes each folded household against its model portfolio. Nil ⇒ no
	// drift is evaluated at all, which is the state this service shipped in for
	// its whole existence (#1010) — so the composition root always supplies one
	// and the metrics it registers make an absent evaluation visible rather than
	// leaving "not checked" and "checked, and in band" indistinguishable.
	drift DriftObserver
}

// DriftObserver measures a folded household against the model portfolio its risk
// profile selects, and records the outcome. It is an interface rather than the
// concrete *drift.Monitor so this package does not import the service's drift
// package — the fold is the caller, not the owner, and the seam keeps a test
// folder from needing a Prometheus registry.
//
// It returns nothing the fold acts on, deliberately: the household book is the
// primary record and drift is derived from it, so a household whose drift cannot
// be computed must still be folded. Trading a missing drift number for a missing
// household would be strictly worse.
type DriftObserver interface {
	Observe(h wealth.Household)
}

// FolderOption customizes a Folder.
type FolderOption func(*Folder)

// WithDriftObserver attaches the target-weight drift evaluation to the fold, so a
// household that has moved past its model's band is noticed on the FACT that
// moved it rather than by a human remembering to look.
func WithDriftObserver(d DriftObserver) FolderOption {
	return func(f *Folder) {
		if d != nil {
			f.drift = d
		}
	}
}

// NewFolder wires a Folder to a durable book.Store. A nil decoder defaults to
// DecodeJSON.
func NewFolder(tenant string, store book.Store, decode Decoder, opts ...FolderOption) (*Folder, error) {
	if store == nil {
		return nil, errors.New("consume: book store is nil")
	}
	if decode == nil {
		decode = DecodeJSON
	}
	f := &Folder{tenant: tenant, store: store, decode: decode}
	for _, opt := range opts {
		opt(f)
	}
	return f, nil
}

// Handle decodes one composition FACT and Puts it into the book (last-write-wins
// on household id). A non-nil return nacks/DLQs the delivery.
func (f *Folder) Handle(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	// This folder writes through an RLS pool pinned to f.tenant, so an envelope
	// from another tenant would be folded into this tenant's book (#223).
	if err := bus.RequireTenantScope(env.GetTenantId(), f.tenant); err != nil {
		return err
	}
	h, err := f.decode(payload)
	if err != nil {
		return fmt.Errorf("consume: %s decode: %w", env.GetEventType(), err)
	}
	if h.HouseholdID == "" {
		return fmt.Errorf("consume: %s missing household_id", env.GetEventType())
	}
	if err := f.store.Put(ctx, h); err != nil {
		return err
	}
	// AFTER the Put, and only on success: drift is measured against the book of
	// record, so evaluating a household the store rejected would report a number
	// for state the service does not hold. Observe records rather than returns —
	// see DriftObserver for why a drift that cannot be computed must not fail the
	// delivery that carried the valuation.
	if f.drift != nil {
		f.drift.Observe(h)
	}
	return nil
}
