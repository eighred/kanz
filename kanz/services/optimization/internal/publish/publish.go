// Package publish is the optimization service's bus binding (#409): it turns the
// order commands a materialized proposal produces into envelopes on the spine,
// and records the materialization itself as a FACT.
//
// IT IS A SEPARATE PACKAGE FROM THE HANDLER ON PURPOSE. The bridge depends on a
// Publisher seam and nothing else (the DEBT-02 inject-the-side-effect stance), so
// the HTTP layer stays testable without a broker and this is the only place that
// knows what a bus envelope looks like. A handler that built envelopes itself
// would be a second place to get the event class wrong.
//
// THE COMMANDS GO THROUGH THE ORDINARY FRONT DOOR. They are published on
// order.order.submit as COMMANDs, exactly as the api-gateway's own order surface
// publishes them, so the OMS's admission path — dedup, the compliance gate, venue
// and order-type support, the outbox — applies unchanged. Auto-publish adds a
// producer; it does not add a second way into the capital path.
package publish

import (
	"context"
	"fmt"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	optimizationpb "github.com/eighred/kanz/kanz-schemas-go/optimization/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/pkg/bus"
)

const (
	// subjectSubmit is the OMS's own command subject — the same one the
	// api-gateway publishes to. There is one door into order admission.
	subjectSubmit = "order.order.submit"
	// subjectMaterialized carries the FACT that a proposal became commands.
	subjectMaterialized = "optimization.proposal.materialized"

	domain = "optimization"
)

// Bus is the publish surface, satisfied by *bus.Producer. Narrow on purpose: this
// package emits and never consumes.
type Bus interface {
	Publish(ctx context.Context, e bus.Event) error
}

// Orders publishes SubmitOrder commands for one tenant. It satisfies
// bridge.Publisher.
type Orders struct {
	bus    Bus
	tenant string
}

// NewOrders returns a publisher scoped to a tenant.
//
// THE TENANT IS A CONSTRUCTOR ARGUMENT, NOT A FIELD ON THE COMMAND, because it
// is the caller's — taken from the authenticated principal — and one publisher is
// built per request rather than shared. A process-wide producer with a fallback
// tenant is how a customer's capital command gets attributed to the platform's
// own tenant, which is valid, not theirs, and undetectable afterwards.
func NewOrders(b Bus, tenant string) *Orders { return &Orders{bus: b, tenant: tenant} }

// Publish emits one SubmitOrder as a COMMAND.
//
// The envelope matches the api-gateway's order surface exactly: the subject
// carries the tenant while event_type does not, the partition key is the order
// id, and the idempotency key is the order id too — ToOrders makes that
// deterministic per (portfolio, instrument), so re-materializing the same
// proposal repeats the ids and the OMS dedups rather than double-trading.
func (o *Orders) Publish(ctx context.Context, cmd *orderpb.SubmitOrder) error {
	if o == nil || o.bus == nil {
		return fmt.Errorf("publish: no broker configured")
	}
	return o.bus.Publish(ctx, bus.Event{
		Subject:        bus.TenantRoutedSubject(o.tenant, subjectSubmit),
		EventType:      subjectSubmit,
		EventClass:     envelopepb.EventClass_EVENT_CLASS_COMMAND,
		SchemaVersion:  1,
		Domain:         domain,
		EventTime:      time.Now().UTC(),
		PartitionKey:   cmd.GetOrderId(),
		IdempotencyKey: cmd.GetOrderId(),
		TenantID:       o.tenant,
		Payload:        cmd,
	})
}

// Materialized records that a proposal became commands, as a FACT.
//
// IT IS EMITTED WHETHER OR NOT ANYTHING WAS PUBLISHED, and `published` says
// which. A dry run is a real outcome — the commands were built and handed back to
// the caller — and a reader must be able to tell it from a live run without
// consulting the deployment's environment.
//
// BEST EFFORT, AND THE ORDER MATTERS. The commands are published first; this is
// published after. If the FACT fails, the orders are already in flight and
// nothing is served by failing the request — but it is never silent: the caller
// gets its result, the error is returned to the composition root's logger and
// counted. Losing the audit record of a capital action is a defect, and the one
// thing that must not happen is losing it quietly.
func Materialized(ctx context.Context, b Bus, tenant string, fact *optimizationpb.ProposalMaterialized) error {
	if b == nil {
		return nil
	}
	fact.AsOf = timestamppb.New(time.Now().UTC())
	return b.Publish(ctx, bus.Event{
		Subject:       bus.TenantRoutedSubject(tenant, subjectMaterialized),
		EventType:     subjectMaterialized,
		EventClass:    envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion: 1,
		Domain:        domain,
		EventTime:     time.Now().UTC(),
		PartitionKey:  fact.GetPortfolioId(),
		TenantID:      tenant,
		Payload:       fact,
	})
}

// Counter is the metric seam — satisfied by prometheus.Counter. Taking an
// interface keeps this package free of the metrics library and lets a test
// assert the count without a registry.
type Counter interface{ Inc() }

// Materializer publishes commands AND records the FACT. It is what
// server.WithAutoPublish takes when the switch is armed.
type Materializer struct {
	bus       Bus
	published Counter
	factsLost Counter
	// tenant is set per request by ForTenant; the zero value publishes nothing,
	// so a Materializer that was never scoped cannot emit an unattributed command.
	tenant string
}

// NewMaterializer returns the ARMED publisher: commands go to the bus.
func NewMaterializer(b Bus, published, factsLost Counter) *Materializer {
	return &Materializer{bus: b, published: published, factsLost: factsLost}
}

// NewRecorder returns the DISARMED publisher: it records the materialization FACT
// and sends no commands.
//
// IT EXISTS SO A DRY RUN IS STILL VISIBLE. Without it the default posture would
// be indistinguishable on the bus from nobody having called the route at all,
// and "nothing configured" and "checked, and fine" must never look the same.
func NewRecorder(b Bus, factsLost Counter) *Materializer {
	return &Materializer{bus: b, factsLost: factsLost}
}

// Armed reports whether this materializer sends commands. It is the handler's
// way of telling a caller which of the two things happened without reading the
// deployment's environment.
func (m *Materializer) Armed() bool { return m != nil && m.published != nil }

// Publish emits one command, or refuses when disarmed.
//
// A DISARMED MATERIALIZER MUST NEVER PUBLISH. It is handed to the server for its
// recording half only, and the server calls Publish solely through
// bridge.Materialize — so this is the last line of defence if that ever changes.
// It returns an error rather than silently succeeding, because a silent no-op
// here is a rebalance the caller believes went out.
func (m *Materializer) Publish(ctx context.Context, cmd *orderpb.SubmitOrder) error {
	if !m.Armed() {
		return fmt.Errorf("publish: auto-publish is not armed; this proposal was a dry run")
	}
	if m.tenant == "" {
		return fmt.Errorf("publish: no tenant on this materializer — a command with no tenant is " +
			"unattributable and the broker rejects it")
	}
	if err := NewOrders(m.bus, m.tenant).Publish(ctx, cmd); err != nil {
		return err
	}
	m.published.Inc()
	return nil
}

// Record emits the ProposalMaterialized FACT, counting a loss.
func (m *Materializer) Record(ctx context.Context, fact *optimizationpb.ProposalMaterialized) error {
	if m == nil || m.bus == nil {
		return nil
	}
	if err := Materialized(ctx, m.bus, m.tenant, fact); err != nil {
		if m.factsLost != nil {
			m.factsLost.Inc()
		}
		return err
	}
	return nil
}

// ForTenant returns a copy scoped to one tenant — the authenticated caller's.
//
// A COPY, NOT A MUTATION: one Materializer is shared by every request, and
// stamping the tenant onto the shared value would let two concurrent callers
// from different tenants publish under each other's. That is the shape of bug
// this platform's account boundary exists to prevent, arriving through a struct
// field instead of a manifest.
func (m *Materializer) ForTenant(tenant string) *Materializer {
	if m == nil {
		return nil
	}
	cp := *m
	cp.tenant = tenant
	return &cp
}
