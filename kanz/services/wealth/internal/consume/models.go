package consume

import (
	"context"
	"errors"
	"fmt"
	"strings"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/wealth"
	"github.com/eighred/kanz/pkg/bus"
)

// ModelDecoder turns a FACT payload into a domain model portfolio. A refused,
// decoded payload may return its model identity with the error; the folder binds
// that identity before recording rejection, never admitting partial policy.
// It is a seam
// for the same reason Decoder above is one — the wealth.v1 SDK is
// generated-not-committed (EVT-15a) — and the composition root wires
// DecodeModelProto behind it.
type ModelDecoder func(payload []byte) (wealth.ModelPortfolio, error)

// ModelFolder is the bus.EventHandler that folds wealth.v1.ModelPortfolio FACTs
// into the in-process catalogue household drift is measured against (WEALTH-01d).
//
// It is a SEPARATE handler from Folder rather than a second branch inside it,
// because the two fold different things with different failure semantics: a
// valuation without a household_id is undeliverable, a model portfolio has no
// household at all, and Folder.Handle rejects on exactly that field. Merging them
// would mean one handler whose validation depends on which subject delivered it —
// the per-subject-factory shape internal/alternatives needed and this service does
// not, because each subject here has one message type.
type ModelFolder struct {
	// tenant is the tenant this instance serves; Handle refuses any other.
	tenant   string
	registry *wealth.ModelRegistry
	decode   ModelDecoder
}

// NewModelFolder wires a ModelFolder to the catalogue it fills. A nil decoder
// defaults to DecodeModelProto — unlike Folder there is no JSON placeholder,
// because this fold was written after the SDK existed.
func NewModelFolder(tenant string, registry *wealth.ModelRegistry, decode ModelDecoder) (*ModelFolder, error) {
	if registry == nil {
		return nil, errors.New("consume: model registry is nil")
	}
	if decode == nil {
		decode = DecodeModelProto
	}
	return &ModelFolder{tenant: tenant, registry: registry, decode: decode}, nil
}

// Handle decodes one model portfolio FACT and admits it to the catalogue.
// SubscribeBroadcastReady NAKs a refusal and keeps initial replay unarmed; that
// path has no DLQ. The registry also invalidates attributable superseded policy
// so an already armed replica cannot keep measuring against stale targets.
func (f *ModelFolder) Handle(_ context.Context, env *envelopepb.Envelope, payload []byte) error {
	// The catalogue is keyed per tenant and this instance serves exactly one, so a
	// model from another tenant would become this tenant's target allocation —
	// the #223 cross-tenant fold, applied to the allocation every household of a
	// risk profile is measured against.
	if err := bus.RequireTenantScope(env.GetTenantId(), f.tenant); err != nil {
		return err
	}
	m, err := f.decode(payload)
	// The canonical publisher keys by the literal model_id, not its subject or
	// escaped subject token. Bind both accepted and refused policy before any
	// catalogue mutation; a decoder error alone cannot name an arbitrary victim.
	if env.GetEventType() != wealth.EventTypeModelPublished || strings.TrimSpace(m.ModelID) == "" || len(m.ModelID) > 256 || env.GetPartitionKey() != m.ModelID {
		return errors.Join(errors.New("consume: model event type, decoded model_id and exact partition_key must agree"), err)
	}
	if err != nil {
		return fmt.Errorf("consume: %s decode: %w", env.GetEventType(), f.registry.Reject(f.tenant, m, err))
	}
	return f.registry.Put(f.tenant, m)
}
