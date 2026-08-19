package order

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/bus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/services/oms/internal/approval"
	"github.com/eighred/kanz/services/oms/internal/compliance"

	"github.com/prometheus/client_golang/prometheus"
)

// Service is the OMS command handler (OMS-01b): the bus.EventHandler that
// drives the order aggregate. Per delivery it decodes the order command,
// validates it, runs the pre-trade compliance gate (OMS-01f), admits or refuses
// it, persists the resulting OrderState, and emits the lifecycle FACTs + the
// universal command outcome. Admission of a marketable order routes it to the
// EMS (OMS-01c) and folds the resulting fills.
//
// Error discipline mirrors the bus contract: a returned error nacks (retry/DLQ)
// and is reserved for TRANSIENT faults (store/publish failure). A bad command —
// validation or compliance rejection — is terminal: it is acked (nil) after a
// REJECTED outcome, so a poison command never blocks the partition.
type Service struct {
	// tenant is the tenant this OMS serves; Handle's cross-tenant refusal
	// compares the inbound envelope against it (#223).
	tenant  string
	store   Store
	gate    compliance.Gate
	emitter *Emitter
	router  *execution.Router
	closes  execution.CloseTracker
	now     func() time.Time
	logger  *slog.Logger

	// accounts binds each portfolio to the exchange account it may execute against.
	// Empty ⇒ nothing is bound, and every portfolio trades whatever account its
	// venue adapter happens to hold — one collateral pool, shared. See noteShared.
	// arrivalMarks stamps the decision-time benchmark on every admitted order
	// (#436). Nil ⇒ orders carry no arrival mark and are not measurable, which is
	// a stated absence rather than a zero.
	arrivalMarks ArrivalMarks

	accounts       *execution.AccountBindings
	requireAccount bool
	sharedOnce     sync.Map // "tenant/portfolio@MIC" → struct{}, so the warning is said once
	// working is the per-order lock table: it excludes the goroutines that drive
	// ONE order — the delivery working it, a resume re-driving it, and a cancel
	// or amend rewriting it. claim() probes it; awaitClaim() waits on it. See
	// orderlock.go.
	working orderLocks
	// claimWait bounds how long a cancel or an amend waits for the in-flight
	// work on an order to release it. Zero means defaultClaimWait.
	claimWait   time.Duration
	sharedCount prometheus.Counter
	// quarantined counts orders frozen because venue truth could not be
	// established. It is the alertable signal: a quarantine is a position whose
	// true size nobody knows, and it must not be discoverable only by reading logs.
	quarantined prometheus.Counter
	// claimTimeouts counts cancels and amends abandoned because the goroutine
	// working the order would not release it in time. Same reasoning as
	// quarantined: each one is an operator instruction that reached the DLQ
	// instead of the order, and a stalled venue must not be discoverable only by
	// reading the DLQ or an ERROR log.
	claimTimeouts prometheus.Counter
	// relay drains the outbox this service's store enqueues into (#292).
	//
	// THE SERVICE OWNS IT RATHER THAN THE COMPOSITION ROOT, deliberately. An
	// outbox nobody drains is worse than no outbox: the OMS admits orders,
	// commits their FACTs to a table and tells nobody, with the store, the
	// handler and every health check reporting success. Constructing the relay
	// here, from the store's own queue and the emitter's own bus, means a
	// composition that has a Service has a drain — there is no wiring step to
	// omit and no option to forget. cmd/oms/main.go still has to RUN it (see
	// Service.Outbox), which is the one part a guard has to cover.
	relay *outbox.Relay
	// relayOpts are collected from ServiceOptions and applied when the relay is
	// constructed at the end of NewService.
	relayOpts []outbox.RelayOption

	// acceptedReannounced counts orders whose ORDER_ACCEPTED FACT a compensator
	// had to publish because admission committed the row and the original
	// publish did not land (#238). Same reasoning as the two counters above, and
	// it is the ONLY signal for this failure: every increment is a window in
	// which the store held an order the rest of the estate had never heard of,
	// and nothing else — not a log line, not an error return, not a DLQ entry
	// once kanz-redrive has drained it — records that the window happened.
	acceptedReannounced prometheus.Counter

	// dualControl classifies each admitted order against
	// OMS_DUAL_CONTROL_MIN_NOTIONAL (#410). Nil ⇒ nothing is classified and
	// nothing is counted, which is the test default and never the production one:
	// cmd/oms/main.go always builds a gate, unarmed and thresholdless if that is
	// how the deployment is configured, so posture="absent" is an ANSWER rather
	// than a silence.
	dualControl *approval.Gate
}

// ServiceOption customizes the handler.
type ServiceOption func(*Service)

// WithDualControl supplies the maker-checker gate for order submission (#410).
//
// It classifies every parent order against the configured notional threshold.
// UNARMED it only counts and logs; ARMED (OMS_REQUIRE_DUAL_CONTROL) an order at
// or above the threshold is HELD in the proposals table by hold() and never
// reaches store.Create — so it is not an order until a second, different
// authenticated subject approves it.
func WithDualControl(g *approval.Gate) ServiceOption {
	return func(s *Service) { s.dualControl = g }
}

// WithAccountBindings gives the OMS the portfolio→exchange-account bindings and the
// posture to take when an order's portfolio has none.
//
// require=true REFUSES an order whose portfolio is bound to no account at the target
// venue. That is the deny-by-default posture and it is the only one under which
// "Basket Alpha's drawdown cannot touch Basket Beta's collateral" is a guarantee
// rather than an intention — but turning it on refuses every order for every portfolio
// nobody has bound yet, which is a trading outage dressed as a control. So it is a
// deliberate switch, defaulted OFF, and the OMS states its posture at startup.
//
// require=false still STAMPS the account the order will actually hit, and says once,
// loudly, per portfolio that its collateral is shared.
func WithAccountBindings(b *execution.AccountBindings, require bool, shared prometheus.Counter) ServiceOption {
	return func(s *Service) {
		s.accounts = b
		s.requireAccount = require
		s.sharedCount = shared
	}
}

// WithQuarantineCounter gives the OMS the counter it increments when an order is
// frozen because venue truth could not be established. Without it a quarantine
// is still persisted and logged at ERROR, but nothing is alertable — and an
// order nobody is watching is exactly what this whole path exists to prevent.
func WithQuarantineCounter(c prometheus.Counter) ServiceOption {
	return func(s *Service) { s.quarantined = c }
}

// WithClaimWait bounds how long a cancel or an amend waits for the goroutine
// currently working an order before giving up and nacking. It exists to be
// LOWERED in tests; raising it in production trades a longer stall of the whole
// cancel subject for a slightly better chance of catching a slow venue, and past
// JetStream's AckWait (60s on order subjects, pkg/bus/tuning.go) the broker
// starts redelivering the cancel while the first copy is still blocked here —
// see defaultClaimWait, which explains what that costs.
func WithClaimWait(d time.Duration) ServiceOption {
	return func(s *Service) { s.claimWait = d }
}

// WithClaimTimeoutCounter gives the OMS the counter it increments when a cancel
// or an amend gives up waiting for the goroutine working an order. Without it
// the timeout is still logged at ERROR and the command still parks in the DLQ,
// but nothing is alertable — and a cancel sitting unnoticed in a DLQ while the
// order it was meant to withdraw is live at an exchange is precisely the
// outcome this path exists to make loud.
func WithClaimTimeoutCounter(c prometheus.Counter) ServiceOption {
	return func(s *Service) { s.claimTimeouts = c }
}

// WithAcceptedReannounceCounter gives the OMS the counter it increments when a
// compensator has to publish an ORDER_ACCEPTED FACT that admission committed
// but never announced (#238).
//
// Without it the repair happens silently, and silence is the wrong outcome
// twice over: the repair is proof that a publish failed, and the interval
// between the failed publish and the repair is an interval in which risk,
// compliance and the audit log were all short one order. A counter is the only
// place that interval is recorded — the store shows the finished order and the
// bus shows the late FACT, and neither says the gap existed.
func WithAcceptedReannounceCounter(c prometheus.Counter) ServiceOption {
	return func(s *Service) { s.acceptedReannounced = c }
}

// abandonClaim records a cancel or amend that could not establish exclusivity.
// It is one function so the cancel and amend paths cannot drift into reporting
// the same failure two different ways.
func (s *Service) abandonClaim(command, orderID string, err error) error {
	if s.claimTimeouts != nil {
		s.claimTimeouts.Inc()
	}
	s.logger.Error("oms: a "+command+" could not take the per-order lock before its deadline — "+
		"the order is still being worked, so this command is going to the DLQ rather than acting "+
		"on state it cannot trust. If this repeats, a venue call is hanging",
		"order_id", orderID, "err", err)
	return err
}

// NewService wires the handler. gate defaults to deny-nothing (compliance.AllowAll)
// when nil; router may be nil to admit orders without working them (they rest).
// closes is the in-flight-close registry the venue-close dispatch path writes to
// and the reconcilers' healing watchdogs drain; nil disables venue-side cancel
// dispatch (the cancel stays ledger-only).
// tenant is the tenant THIS OMS serves. Required, and refused when empty for the
// same reason pg.NewTenantPool refuses one: a service that cannot say whose book
// it is writing must not write. It is the input to the cross-tenant refusal in
// Handle (#223).
func NewService(tenant string, store Store, emitter *Emitter, gate compliance.Gate, router *execution.Router, closes execution.CloseTracker, logger *slog.Logger, opts ...ServiceOption) (*Service, error) {
	if store == nil || emitter == nil {
		return nil, errors.New("oms: store and emitter required")
	}
	if tenant == "" {
		return nil, errors.New("oms: empty tenant — this service could not tell its own " +
			"events from another tenant's, and every order it stored would be scoped to nothing")
	}
	if gate == nil {
		gate = compliance.AllowAll{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	svc := &Service{
		tenant: tenant,
		store:  store, gate: gate, emitter: emitter, router: router, closes: closes,
		now: time.Now, logger: logger, claimWait: defaultClaimWait,
	}
	for _, opt := range opts {
		opt(svc)
	}
	// THE RELAY IS BUILT LAST, from this store's own outbox and this emitter's
	// own bus, so a Service always has a drain for the FACTs its store commits
	// (#292). Options are applied first because they carry the relay's interval
	// and counters.
	relay, err := outbox.NewRelay(store.Outbox(), emitter.publisher(), logger, svc.relayOpts...)
	if err != nil {
		return nil, err
	}
	svc.relay = relay
	return svc, nil
}

// Outbox is the relay draining the FACTs this service's store commits. The
// composition root must Run it; see Service.relay and
// test/arch/oms_outbox_test.go, which fails if cmd/oms/main.go stops doing so.
func (s *Service) Outbox() *outbox.Relay { return s.relay }

// WithOutboxRelay passes options through to the relay NewService constructs.
// The relay itself is not injectable — see Service.relay for why.
func WithOutboxRelay(opts ...outbox.RelayOption) ServiceOption {
	return func(s *Service) { s.relayOpts = append(s.relayOpts, opts...) }
}

// Handle is the bus.EventHandler. It dispatches by the command subject/type.
func (s *Service) Handle(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	// ONE CHECK FOR ALL THREE COMMANDS. handleSubmit resolves the venue account
	// from env.GetTenantId() while the store writes
	// current_setting('app.tenant_id') — the GUC pinned to THIS service's tenant.
	// Nothing compared them, so an acme order was admitted from an acme envelope
	// and stored as __system__ (#223). Checking at the dispatch point rather than
	// in each handler is deliberate: handleCancel and handleAmend do not even
	// receive the envelope, so a per-handler check would have to thread it and
	// would be forgotten by the next command added here.
	if err := bus.RequireTenantScope(env.GetTenantId(), s.tenant); err != nil {
		return err
	}
	switch env.GetEventType() {
	case SubjectSubmit:
		return s.handleSubmit(ctx, env, payload)
	case SubjectCancel:
		return s.handleCancel(ctx, payload)
	case SubjectAmend:
		return s.handleAmend(ctx, payload)
	case SubjectApprove:
		return s.handleApprove(ctx, env, payload)
	default:
		// Subscription scope matched but the type is unknown — surface so the
		// operator sees the misconfiguration (same stance as ingest).
		return errors.New("oms: unknown command type: " + env.GetEventType())
	}
}

// handleSubmit is the bus entry for a first-signature submission. It offers no
// approval, so an order that needs one is held rather than admitted.
func (s *Service) handleSubmit(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	return s.submit(ctx, env, payload, nil)
}

// submit is THE ONE PATH AN ORDER COMES INTO EXISTENCE ON, and appr is the only
// thing that distinguishes a first signature from a second.
//
// A nil appr is a plain submission: an order at or above the dual-control
// threshold reaches the gate below and is HELD. A non-nil appr is a release —
// handleApprove has already proved it against the stored proposal, and the
// order replays every gate from here as if freshly submitted.
//
// THE REPLAY IS THE POINT, NOT AN INEFFICIENCY. A proposal may rest for
// dualcontrol.DefaultTTL before anybody signs it, and compliance, the mandate,
// the venue's supported order types and the portfolio's exchange account can all
// move inside a day. Admitting on the clearance the order got when it was
// PROPOSED would let a signature launder yesterday's answer into today's book.
func (s *Service) submit(ctx context.Context, env *envelopepb.Envelope, payload []byte, appr *dualcontrol.Approval) error {
	var cmd orderpb.SubmitOrder
	if err := proto.Unmarshal(payload, &cmd); err != nil {
		// A malformed command body is a permanent defect; reject-and-ack rather
		// than redeliver forever. order_id is unknown, so nothing to correlate.
		s.logger.Error("oms: malformed SubmitOrder", "err", err)
		return nil
	}
	// The command's quantity and prices reach the order aggregate and the position
	// book, which convert them with the unbounded dec.FromProto (#95). An
	// unvalidated wire exponent there does not misprice an order — it never
	// returns, and the OMS stops consuming commands entirely. Same class of
	// permanent defect as a malformed body, so reject-and-ack rather than
	// redeliver forever.
	if field, in := dec.InDomainDeep(&cmd); !in {
		s.logger.Error("oms: SubmitOrder carries an out-of-domain exponent — refusing",
			"field", field, "portfolio_id", cmd.GetPortfolioId())
		return nil
	}
	now := s.now().UTC()

	// ENTITLEMENT, BEFORE ANY SIDE EFFECT — the same rule handleCancel and
	// handleAmend run, and its absence here was the open half of #225. Cancel and
	// amend checked it; submit did not, so a caller scoped to `research` could
	// OPEN a position in `flagship` and then be refused when they tried to close
	// it. It sits above the resume fast path deliberately: resume queries the
	// venue, and an unentitled command must not reach a venue.
	//
	// refuse, not outcomeReject: no order exists yet, the portfolio id is the
	// caller's own, and every other admission refusal on this path emits
	// ORDER_REJECTED. Nothing is disclosed that the caller did not send.
	if !delegatedAndEntitled(cmd.GetMetadata(), cmd.GetPortfolioId()) {
		return s.refuse(ctx, cmd.GetOrderId(), ReasonNotEntitled,
			"principal is not entitled to this order's portfolio", now)
	}

	// AN ORDER WE ALREADY KNOW IS NOT AUTOMATICALLY A DUPLICATE.
	//
	// This used to return nil on sight, which is right for a genuinely duplicated
	// command and catastrophic for a redelivery of one whose work was interrupted:
	// delivery 1 created the order and died at the venue, delivery 2 acked it, and
	// the order sat at ROUTED forever with nothing working it. resume() establishes
	// what the venue actually did and acts on that — or freezes the order when it
	// cannot. Admission itself is still enforced atomically by store.Create below.
	if existing, _, err := s.store.Load(ctx, cmd.GetOrderId()); err == nil {
		// The check above cleared the portfolio the COMMAND named; this clears
		// the one the STORED order belongs to, and they are not the same
		// question. A submit naming an order_id that already exists is how an
		// unentitled caller would otherwise reach resume() — and resume queries
		// the venue and can re-drive or close somebody else's live order. A
		// genuine redelivery carries identical bytes and cannot fail this.
		//
		// outcomeReject, not refuse: this order exists and may be working at a
		// venue. Emitting ORDER_REJECTED for it would tell every downstream fold
		// that a live order was refused.
		if !delegatedAndEntitled(cmd.GetMetadata(), existing.GetPortfolioId()) {
			return s.outcomeReject(ctx, cmd.GetOrderId(), ReasonNotEntitled,
				"principal is not entitled to this order's portfolio", now)
		}
		return s.resume(ctx, existing)
	} else if !errors.Is(err, ErrNotFound) {
		return err // transient store failure
	}

	// THE PARENT/CHILD RELATION, DECIDED BEFORE THE COMPLIANCE GATE (#435),
	// because which of these two an order is changes what that gate should do.
	//
	// A PARENT's schedule is validated here, at admission, so a schedule that
	// cannot be worked is the caller's answer to their own command. Admitted and
	// then refused on every driver tick would be an order shown working on every
	// screen while nothing ever traded it, and nobody watching the log.
	if rej := validateSchedule(&cmd); rej != nil {
		return s.refuse(ctx, cmd.GetOrderId(), rej.Code, rej.Msg, now)
	}
	// A CHILD is authorized by its parent's own schedule — see authorizeChild.
	var parent *orderpb.OrderState
	if cmd.GetParentOrderId() != "" {
		var rej *RejectError
		var aerr error
		parent, rej, aerr = s.authorizeChild(ctx, &cmd)
		if aerr != nil {
			return aerr // transient store failure ⇒ redeliver, never refuse
		}
		if rej != nil {
			return s.refuse(ctx, cmd.GetOrderId(), rej.Code, rej.Msg, now)
		}
	}

	// MAKER-CHECKER CLASSIFICATION, BESIDE THE COMPLIANCE GATE AND NEVER INSIDE IT
	// (#410).
	//
	// WHY IT IS NOT A RULE IN THE GATE BELOW. internal/compliance/gate.go
	// short-circuits BEFORE it ever resolves a price: a portfolio with no mandate
	// returns Allowed (OMS_REQUIRE_MANDATE defaults to false, config.go), and a
	// mandate with zero rules returns Allowed — both without valuing the order at
	// all. A dual-control threshold evaluated inside that gate would inherit the
	// short-circuit exactly, so THE LARGEST ORDERS ON UNGOVERNED PORTFOLIOS would
	// skip the control entirely. That is the population a maker-checker rule
	// exists for. This resolves its own price, unconditionally, and answers for
	// every order the gate answers for and for the ones it declines to value.
	//
	// A CHILD IS NOT CLASSIFIED, for the same reason it is not re-checked for
	// compliance (the note below): the decision was made once, for the whole
	// notional, at the parent. Counting each slice would report one decision as
	// slice_count single-signed orders and mis-scale the number the arming
	// decision rests on.
	//
	// DECIDED HERE, COUNTED ONLY AFTER ADMISSION SUCCEEDS — see Gate.Count. An
	// order refused for compliance, an unroutable venue or an unsupported order
	// type never went through at all, single-signed or otherwise.
	var dual approval.Decision
	if parent == nil {
		dual = s.dualControl.Decide(&cmd)
		if dual.Posture == approval.PostureAtOrAbove && !dual.Required {
			// UNARMED IS NOT UNRECORDED — the stance #495 took on the override
			// path, applied here. The act and the digest a second signature would
			// have had to cover are recorded now, so arming the control later does
			// not leave today's large orders ambiguous, and nobody has to
			// reconstruct from configuration history which ones were above the line.
			s.logger.Warn("order at or above the dual-control threshold is being ADMITTED ON ONE "+
				"SIGNATURE — the control is not armed (#410)",
				"order_id", cmd.GetOrderId(), "portfolio_id", cmd.GetPortfolioId(),
				"act", string(dual.Act), "digest", dual.Digest, "digest_err", dual.DigestErr)
		}
	}

	// Pre-trade compliance gate (OMS-01f).
	//
	// THE ENVELOPE'S TENANT, NOT s.tenant. SubmitOrder carries no tenant of its
	// own and portfolio_id is a caller-chosen string, so the mandate registry used
	// to key on "growth" alone and hand tenant B's order tenant A's concentration
	// limits (#243). env.GetTenantId() is the authenticated caller's tenant — the
	// api-gateway stamps it and bus.Validate requires it non-empty on the live
	// path. s.tenant is this OMS's serving tenant, which ships as __system__ and
	// would ask for the platform's mandate rather than the customer's.
	//
	// A CHILD IS NOT RE-CHECKED, AND THAT IS A DECISION WITH A REASON (#435).
	//
	// The parent was checked ONCE, for the WHOLE notional, and admitted. Its
	// children are fractions of a quantity a mandate has already approved. Running
	// the gate again per child is not a stronger control — it is a weaker one, in
	// two directions at once:
	//
	//   - IT DOES NOT CATCH WHAT IT LOOKS LIKE IT CATCHES. Every limit here is
	//     evaluated against a book that only moves on FILLS, plus a cash view and
	//     a risk view with their own freshness bounds. N children submitted inside
	//     one window each see the same unchanged book, so each passes on its own
	//     and the sum is never tested. Checking the parent's full notional once is
	//     the only place the sum IS tested.
	//   - IT BREAKS ORDERS THE MANDATE ALLOWED. As the early slices fill, the book
	//     moves toward whatever limit the parent cleared. A later child refused by
	//     that limit strands the parent part-filled — half a position, taken
	//     deliberately, that the platform will not let anybody finish. A
	//     part-executed order nobody can complete is worse than one never started.
	//
	// So the gate answers "may this fund take this position", which is a question
	// about the decision; the schedule answers "how is it worked", which is not.
	// The control over a schedule already in flight is CANCELLING THE PARENT, which
	// stops every unsent child — #435's clause (c), enforced in schedule.Due and
	// again in authorizeChild.
	if parent == nil {
		breach, err := s.gate.Check(ctx, env.GetTenantId(), &cmd)
		if err != nil {
			return err // transient gate failure ⇒ retry
		}
		if breach != nil {
			return s.refuse(ctx, cmd.GetOrderId(), "COMPLIANCE_"+breach.Code, breach.Reason, now)
		}
	}

	// An order that names a venue this OMS has no adapter for can NEVER be executed
	// — not now, not on a retry, not ever. Refuse it HERE, at admission, alongside
	// the compliance and validation rejections, because that is what it is: an order
	// this OMS cannot work. It used to be admitted and left to "rest", which told
	// the strategy its order was working while nothing on this platform would ever
	// route it anywhere, and nothing would ever say so (EXEC-M8).
	//
	// This is NOT the "no venues configured at all" case — that is a deliberate
	// paper/observation deployment, and its orders still rest.
	if target := cmd.GetVenue(); target != "" && s.router != nil && !s.router.Supports(target, "") {
		return s.refuse(ctx, cmd.GetOrderId(), "VENUE_NOT_CONFIGURED",
			fmt.Sprintf("target venue %q is not configured on this OMS", target), now)
	}

	// AN ORDER TYPE THE VENUE CANNOT PLACE IS REFUSED HERE TOO, and for exactly
	// the reason above it (#405).
	//
	// order.v1 declares MARKET, LIMIT, STOP and STOP_LIMIT and Accept validates
	// all four, but the spot adapters implement the first two and refuse the rest
	// inside Execute. So a stop-loss passed admission, was stored, and had its
	// ORDER_ACCEPTED FACT committed and published — and only then failed at the
	// venue. The order existed everywhere: risk carried exposure for it, tv-sync
	// showed it working, the caller had been told it was accepted. It just never
	// went anywhere.
	//
	// That is the EXEC-M8 defect on a different field. For a stop in particular
	// it is the worst possible shape: the whole point of one is to act when
	// nobody is watching, so "accepted and inert" is indistinguishable from
	// "armed" until the moment it was supposed to fire.
	//
	// Refused with the TYPE and the VENUE named, because the operator's next
	// question is which of the two to change.
	if target := cmd.GetVenue(); target != "" && s.router != nil &&
		!s.router.SupportsOrderType(target, cmd.GetOrderType()) {
		return s.refuse(ctx, cmd.GetOrderId(), "ORDER_TYPE_NOT_SUPPORTED",
			fmt.Sprintf("venue %q cannot place a %s order — this OMS will not admit an order it "+
				"cannot route", target, cmd.GetOrderType()), now)
	}

	// AND A TIME-IN-FORCE THE VENUE CANNOT EXPRESS IS REFUSED HERE TOO (#486) —
	// the same gate, one field over, for a defect that was worse than the one
	// above it.
	//
	// The order type cases produced an order that did NOTHING: accepted, inert,
	// never placed. time_in_force produced an order that did the WRONG THING.
	// Both spot connectors sent a hard-coded good-til-cancelled whatever the
	// trader asked for, so an IMMEDIATE-OR-CANCEL order RESTED at the exchange —
	// a trader who asked to hold no exposure was holding it, indefinitely, with
	// nothing anywhere saying so — and a FILL-OR-KILL could rest PARTIALLY
	// FILLED, the single outcome that instruction exists to forbid.
	//
	// The connectors now refuse what they cannot express, which stopped the wrong
	// trade. This stops the wrong ADMISSION: without it the order is stored, its
	// ORDER_ACCEPTED FACT committed and published, and only then refused by the
	// exchange — which is #405's complaint verbatim, one field over.
	if target := cmd.GetVenue(); target != "" && s.router != nil &&
		!s.router.SupportsTimeInForce(target, cmd.GetTimeInForce()) {
		return s.refuse(ctx, cmd.GetOrderId(), "TIME_IN_FORCE_NOT_SUPPORTED",
			fmt.Sprintf("venue %q cannot express %s — placing it as good-til-cancelled would rest "+
				"an order the trader asked to expire, so this OMS will not admit it",
				target, cmd.GetTimeInForce()), now)
	}

	// WHOSE COLLATERAL DOES THIS ORDER SPEND? Resolve the exchange account before the
	// order exists, because it is not a routing detail — it is the answer to that
	// question, and an order admitted without one is an order that will margin against
	// whichever account its adapter happens to hold.
	//
	// A CHILD INHERITS ITS PARENT'S ANSWER RATHER THAN ASKING AGAIN (#435), and
	// inheriting is the safer of the two, not merely the cheaper.
	//
	// The parent resolved this at ITS admission, under the authenticated caller's
	// tenant. Re-resolving per child would ask under a different tenant — the
	// driver ticks on a timer and has no inbound envelope to inherit one from —
	// and, worse, it would ask AGAIN: a binding changed midway through a schedule
	// would silently move the remaining slices onto different collateral than the
	// ones already filled. One order, one collateral answer, decided once.
	var account string
	if parent != nil {
		account = parent.GetVenueAccountId()
	} else {
		var rej *RejectError
		account, rej = s.resolveAccount(env.GetTenantId(), cmd.GetPortfolioId(), cmd.GetVenue())
		if rej != nil {
			return s.refuse(ctx, cmd.GetOrderId(), rej.Code, rej.Msg, now)
		}
	}

	// Validate + admit.
	st, err := Accept(&cmd, now)
	if err != nil {
		var re *RejectError
		if errors.As(err, &re) {
			return s.refuse(ctx, cmd.GetOrderId(), re.Code, re.Msg, now)
		}
		return err
	}

	// THE HOLD (#410). AN ORDER THAT NEEDS TWO PEOPLE DOES NOT BECOME AN ORDER.
	//
	// It is here — after every refusal above and before store.Create below — and
	// both halves of that placement are deliberate.
	//
	// AFTER THE REFUSALS, because an approver's attention is the scarcest thing
	// this control spends. An order that breaches the mandate, names a venue this
	// OMS cannot route, or asks for an order type the exchange will not place is
	// refused NOW, exactly as it is today. Holding it first would queue orders
	// that can never trade whatever anybody signs, and the pending list — the
	// thing somebody has to watch, or the control stops being a control (#495) —
	// would fill with them. Accept() has also run, so a held order is a VALID
	// order: an approver is never asked to sign something that would be rejected
	// the moment they did.
	//
	// BEFORE store.Create, because that is the admission gate. A held order must
	// be absent from `orders` — not present with a pending status — or the
	// routing gate in work(), the startup sweep, the position projector, tv-sync
	// and kanz-web each become one more place that has to know not to work it.
	// See migrations/0009 for the whole argument.
	//
	// A CHILD NEVER REACHES HERE. dual is the zero Decision when parent != nil,
	// so Required is false: the parent was decided once, for the whole notional,
	// and refusing a late slice would strand it part-filled — the same rule the
	// compliance gate follows above.
	// THE HOLD, AND THE ONE THING THAT RELEASES IT (#539).
	signatures := approval.SingleSigned
	if dual.Required {
		if appr == nil {
			return s.hold(ctx, &cmd, dual, now)
		}
		// THE SECOND LINE, NOT THE FIRST. handleApprove already checked this
		// approval against the stored proposal; re-deriving the digest HERE from
		// the terms of the command actually being admitted is what stops a future
		// caller from releasing one held order with a signature collected for
		// another. Covers refuses the zero Approval outright, so the value every
		// other path passes cannot satisfy it.
		if err := approval.Covers(*appr, approval.TermsOfSubmit(&cmd)); err != nil {
			// Loud, not a hold. Falling through to hold() here would hit
			// ErrProposalExists and ack silently — an approved order that never
			// trades and never reports why, which is the failure #539 exists to end.
			return fmt.Errorf("oms: approval does not cover order %s: %w", cmd.GetOrderId(), err)
		}
		signatures = approval.DualSigned
	}
	// THE ARRIVAL MARK, STAMPED HERE AND NOWHERE ELSE (#436).
	//
	// Transaction-cost analysis compares what an order paid against what the
	// market showed WHEN THE DECISION WAS MADE. That mark cannot be recovered
	// afterwards: a price read at analysis time is a price from after this order
	// moved the market, which biases every cost measure flatter and always in the
	// flattering direction. A cost number that understates cost is worse than
	// none, because it is the only number on the page and it will be believed.
	//
	// So it is captured at admission — the earliest point the OMS knows both the
	// instrument and the time — even though nothing reads it yet. Every order
	// admitted without it is permanently unmeasurable, which is the same
	// retroactive damage #432 fixed one field over.
	//
	// It is NOT stamped inside Accept: that function is documented pure — no I/O,
	// identical on the live path and on replay — and a lookup against a live
	// mark cache is neither. Replaying an order must not re-stamp it with today's
	// price.
	//
	// A CHILD INHERITS ITS PARENT'S MARK RATHER THAN TAKING ITS OWN (#435), AND
	// THIS IS THE LINE THAT KEEPS TWAP HONEST.
	//
	// Implementation shortfall is BY DEFINITION measured against the mark at the
	// moment somebody decided to trade. The decision to trade this quantity was
	// made when the PARENT was admitted; a child is not a decision, it is a
	// consequence of one. Stamping a child with the market as it stood twenty
	// minutes into the window would benchmark it against a price that already
	// contains the impact of the earlier slices — so every child would report
	// roughly zero shortfall, and the parent's real cost would vanish from the
	// measure entirely.
	//
	// That failure is invisible and it flatters: the platform would report that
	// working orders on a schedule costs nothing, which is precisely the claim
	// #435 exists to let somebody TEST. A measure that cannot show the feature
	// failing cannot show it working either.
	if parent != nil {
		st.ArrivalPrice = parent.GetArrivalPrice()
		st.ArrivalAt = parent.GetArrivalAt()
	} else {
		s.stampArrival(st)
	}
	// THE RELATION AND THE SCHEDULE COME OFF THE COMMAND, having been validated
	// above — a parent carries a schedule and no parent id; a child carries a
	// parent id and no schedule; validateSchedule refuses anything holding both.
	st.ParentOrderId = cmd.GetParentOrderId()
	st.ExecutionSchedule = cmd.GetExecutionSchedule()
	if st.GetExecutionSchedule() != nil {
		// A PARENT RESTS. It has passed every gate and is deliberately not going
		// to a venue — which is why this is its own status rather than a flag on
		// PENDING_NEW, where it would be indistinguishable from an order nothing
		// has got to yet. work() refuses to route it; the driver works its
		// children.
		st.Status = orderpb.OrderStatus_ORDER_STATUS_WORKING_SCHEDULED
	}
	// The account is stamped by the OMS, never by the caller. A caller that could name
	// the account could name ANY portfolio's account — it would be choosing whose
	// collateral to spend — which is why SubmitOrder has no such field to set.
	st.VenueAccountId = account

	// THE ORDER AND ITS ANNOUNCEMENT ARE NOW ONE WRITE (#292).
	//
	// This used to be store.Create followed by emitter.EmitAccepted: two
	// independent writes, and when the second failed the fund held a durable
	// PENDING_NEW order that no downstream service had ever heard of. Risk
	// carried no exposure for it; tv-sync never admitted it and therefore also
	// DROPPED the ORDER_ROUTED a later re-drive emitted, so the order stayed
	// invisible even after it started working. #238 made that recoverable — a
	// marker column and a sweep that finds the strays — and this makes it
	// unreachable: the FACT is enqueued by the same COMMIT that admits the
	// order, so there is no instant at which one exists without the other.
	//
	// THE FACT IS BUILT BEFORE THE MARKER IS STAMPED, deliberately. The
	// announcement carries the order's admitted state, which is what every
	// consumer folds; accepted_announced_at is an OMS-internal recovery marker
	// and putting it on the wire would change the payload of an existing FACT
	// for no downstream benefit. This is the same content EmitAccepted published
	// before this change.
	accepted, err := s.emitter.AcceptedFact(ctx, st)
	if err != nil {
		// The FACT cannot be captured — no tenant on the delivery, or a payload
		// that will not marshal. Returning BEFORE the store write is the whole
		// discipline: an order whose announcement cannot be made must not be
		// admitted either.
		return err
	}

	// THE MARKER MOVES INTO THE TRANSACTION, AND SO ITS MEANING CHANGES.
	//
	// accepted_announced_at used to mean "EmitAccepted returned nil" and cost an
	// extra UPDATE on the admission path to record it (#238). It now means "the
	// ACCEPTED FACT is committed for delivery", which is strictly stronger — a
	// promise the relay keeps rather than a fact about a call that already
	// returned — and it costs nothing, because it rides the INSERT that was
	// happening anyway.
	//
	// STAMPING IT HERE IS ALSO WHAT KEEPS THE RELAY AND #238'S SWEEP FROM
	// FIGHTING. Both can publish an ACCEPTED FACT. The sweep republishes exactly
	// when this marker is unset; the relay publishes exactly what is in the
	// outbox. Because the marker and the outbox row land in the SAME
	// transaction, an order that has an outbox record always has the marker, so
	// the sweep never looks at it — and an order the sweep DOES look at (a row
	// admitted before this migration, marker unset, no record behind it) has
	// nothing in the outbox for the relay to duplicate. The two compensators
	// partition the population rather than overlapping on it. Leaving the marker
	// to a Save after the commit would have inverted that: for up to
	// OMS_SWEEP_MIN_AGE every admitted order would look unannounced, and the
	// sweep would re-announce orders the relay had already sent — routinely, on
	// the counter that exists to make a LOST FACT alertable.
	admitted := cloneState(st)
	admitted.AcceptedAnnouncedAt = timestamppb.New(now)

	// THE ADMISSION GATE. Create is atomic: exactly one concurrent delivery of this
	// order_id can insert it, and every other gets ErrExists. Losing the race means
	// another delivery already owns this order and is working it — so this one acks
	// and stops, HERE, before s.work() below routes it to a venue. The old
	// Load()-then-Save() let both deliveries through this point and both reached the
	// venue: the state converged (Save upserts) while the fund traded twice.
	if err := s.store.Create(ctx, admitted, []outbox.Record{accepted}); err != nil {
		if errors.Is(err, ErrExists) {
			// Lost the admission race — the winner works the order. Deliberately NOT
			// a resume: the winner is mid-flight by construction, and the interrupted
			// case is reached through the Load fast path above (on redelivery) or the
			// startup sweep (after a crash), both of which take the per-order claim.
			//
			// The loser's outbox record rolled back with its INSERT (see
			// Postgres.Create), so this delivery announces nothing. Without that
			// the order would be announced once per redelivery.
			return nil
		}
		return err
	}
	st = admitted

	// THE ORDER WENT THROUGH, AND THIS IS HOW MANY PEOPLE SIGNED IT (#410).
	//
	// Counted at the point admission is durable, not at the gate: everything above
	// this line can still refuse the order, and an order that was refused did not
	// go through on one signature. Only the PARENT of a schedule is counted; its
	// children inherit its decision, exactly as they inherit its compliance
	// clearance and its arrival mark.
	//
	// signatures is DualSigned only on the approve path, where a second subject's
	// approval was proved against the stored proposal above. This counter is the
	// number the arming decision rests on — how much of the order flow went
	// through on one signature — so counting an approved order as single_signed
	// would make the control look ineffective exactly as it started working.
	if parent == nil {
		s.dualControl.Count(dual, signatures)
	}

	// PUBLISH IT, HERE, BEFORE ANYTHING ELSE HAPPENS TO THIS ORDER.
	//
	// The FACT is durable now, which is the change. It is not yet ON THE BUS,
	// and everything below — s.work's ORDER_ROUTED, the fills, the outcome — is
	// still published directly. Letting those go out first would hand tv-sync an
	// ORDER_ROUTED for an order it never admitted, which transition() DROPS: the
	// projection stays blind to a live order while the OMS believes it announced
	// everything. So the sequence on the bus is exactly what it was before this
	// change; only its durability differs.
	//
	// THE ERROR BEHAVIOUR IS ALSO EXACTLY WHAT EmitAccepted's WAS: return, do
	// not work the order, nack. What is different is what happens next. Before,
	// the FACT was gone and a compensator had to notice the row and reconstruct
	// it. Now the record is in the table: the relay's tick publishes the
	// original, and the redelivery that follows finds an order already admitted
	// and already announced.
	if _, err := s.relay.Flush(ctx, st.GetOrderId()); err != nil {
		return err
	}

	// TAKE THE PER-ORDER CLAIM before working the order. Admission (this delivery)
	// and resume() (a redelivery of the same order_id) must never drive one order
	// concurrently, and until now nothing enforced that: claim() had exactly one
	// caller, resume, and this path called s.work directly. The break this closes:
	// delivery A reaches here, Creates the order, and blocks inside venue.Execute
	// below. Delivery B — a second copy of this same command, redelivered before A
	// finishes — takes the Load-found-it branch above and calls resume(). Without
	// this claim, resume finds the claim free, queries the venue, and because the
	// venue has not yet recorded A's in-flight Execute it truthfully answers
	// UNKNOWN — so resume re-drives it. That is a concurrent second Execute of one
	// order: a double trade, and it happens precisely because the order already
	// existed and looked interrupted, not because anything was malformed. Taking
	// the claim here, at the same call site resume() takes it for itself, makes
	// the two mutually exclusive: whichever gets here first drives the order, and
	// the other finds the claim held and stops. defer release() so every return
	// below — the ErrUnpriced branch and the final outcome emission — is covered.
	//
	// TRY, NOT WAIT, IS DELIBERATE HERE. "Somebody already owns this order" is a
	// complete answer for a submit or a resume, because the holder carries the
	// order to completion on this delivery's behalf. Cancel and amend cannot back
	// off on that reasoning — nobody cancels an order on their behalf — so they
	// take the same lock through awaitClaim and wait for it. See orderlock.go.
	release, ok := s.claim(st.GetOrderId())
	if !ok {
		// Another goroutine in THIS process already holds the claim and is driving
		// this order. That can only be a resume: store.Create above is the
		// admission gate, so at most one delivery of a NEW order_id ever reaches
		// this line, and this order_id was ours to create. The order exists and
		// its ACCEPTED fact is already committed to the outbox, so there is
		// nothing left for this delivery to do — the claim holder will carry it
		// to completion. This is the same reasoning as the ErrExists branch
		// above: the order exists and something in this process already owns it.
		return nil
	}
	defer release()

	// VERSION 0, AND THERE IS NO LONGER A WRITE BETWEEN HERE AND work().
	//
	// store.Create landed this order at version 0 (migrations/0005 default) and
	// this delivery won the admission gate, so 0 is the version nobody else can
	// have written past yet. #238 spent an extra UPDATE here stamping
	// accepted_announced_at, because the FACT had already gone out on its own
	// and the marker was the only record that it had. The marker now rides the
	// INSERT (see the admission block above), so that UPDATE is gone: one fewer
	// round trip on the admission path, and one fewer window in which the order
	// exists at a version the next writer has to guess at.
	var ver int64

	// Work the order if a router is wired; otherwise it rests at PENDING_NEW.
	st, ver, err = s.work(ctx, st, ver)
	// A venue that cannot price this order will NEVER price it, so this is
	// terminal, not transient. Returning the error here would nack the command
	// and retry it forever; refuse() emits the FACT + outcome and acks. Same
	// code COMP-M1's gate uses for an order it cannot value — one name for one
	// failure, discovered at two different points.
	if errors.Is(err, execution.ErrUnpriced) {
		rejectedAt := s.now().UTC()
		// Persist the terminal state BEFORE announcing it. The order passed
		// admission and is stored as ROUTED; leaving it there would have the
		// ledger say rejected while the OMS's own truth says working, and a
		// later cancel would act on a live-looking order.
		rejected := Reject(st, rejectedAt)
		// TRANSACTIONAL (#292). This was the last pair whose FACT nothing could
		// rebuild: outcome_announced_at drives completeTerminalOutcome, which
		// reconstructs the CommandOutcome from stored state and says in its own
		// comment that it cannot reconstruct the ORDER_REJECTED FACT. So a crash
		// between the Save and the publish left the order durably REJECTED, the
		// caller eventually answered, and the FACT gone — the ledger and every
		// downstream projection never learning the order died.
		//
		// Both records are built BEFORE the Save, so a record that could never be
		// published stops the rejection instead of committing half of it.
		rejFact, ferr := s.emitter.RejectedFact(ctx, st.GetOrderId(), "PRICE_UNAVAILABLE", err.Error(), rejectedAt)
		if ferr != nil {
			return ferr
		}
		outFact, ferr := s.emitter.OutcomeFact(ctx, st.GetOrderId(),
			commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED,
			err.Error(), "PRICE_UNAVAILABLE", "", rejectedAt)
		if ferr != nil {
			return ferr
		}
		// outcome_announced_at is stamped in the SAME write, not a second one.
		// The marker's job is to send a redelivery to resume()'s
		// genuine-duplicate branch (order_events.proto:19); committing it
		// alongside the records it describes is what makes it true the instant
		// it is durable, instead of after two more writes that can each fail.
		rejected.OutcomeAnnouncedAt = timestamppb.New(rejectedAt)
		if serr := s.store.Save(ctx, rejected, ver, []outbox.Record{rejFact, outFact}); serr != nil {
			return serr
		}
		// Flushed here because the submitter is waiting on this outcome; the
		// records are durable either way, so this only decides whether the answer
		// arrives now or on the relay's next pass.
		_, ferr = s.relay.Flush(ctx, st.GetOrderId())
		return ferr
	}
	if err != nil {
		return err
	}

	// THE TRAILING OUTCOME, AND WHICH TRANSACTION OWNS IT (#292).
	//
	// This was the last pair the issue named and the only one that was a DECISION
	// rather than a mechanical conversion, because it is not a pair in the usual
	// shape: the state this outcome reports was committed by work(), in
	// transactions that closed before control returned here. There is no pending
	// write to hand it to. So the question was never "convert it" but "which write
	// owns it", and the honest answer differs by branch — because only one of the
	// two branches has a write at all.
	//
	// It could NOT be pushed down into work(). work() is also called by resume()'s
	// PENDING_NEW and re-drive branches, which deliberately publish no command
	// outcome: a broker redelivery is not a second command to answer. Giving
	// work()'s Saves this record would answer commands that were never asked, and
	// stamping outcome_announced_at from there would tell a later resume() that a
	// terminal order's outcome had been announced when nothing had announced it.
	if st.GetStatus() == orderpb.OrderStatus_ORDER_STATUS_FILLED {
		// FILLED — IT RIDES THE MARKER SAVE, which is a write this branch was
		// already doing. Two writes become one: the outcome the submitter is
		// waiting on and the marker recording that it was answered now commit
		// together, so the marker cannot claim an announcement that was never
		// made, and the announcement cannot be lost while the marker says it
		// happened. Before this, a publish that failed here left a FILLED order
		// with the marker unset and the caller unanswered until resume() ran
		// completeTerminalOutcome — which re-derives a generic outcome rather than
		// this one.
		//
		// The fill's own FACT went out inside work()'s loop, flushed there, so the
		// bus order is unchanged: fills first, then the outcome that closes the
		// command.
		fact, ferr := s.emitter.OutcomeFact(ctx, st.GetOrderId(),
			commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED, "order filled", "", "", now)
		if ferr != nil {
			return ferr
		}
		if err := s.markOutcomeAnnounced(ctx, st, ver, now, []outbox.Record{fact}); err != nil {
			return err
		}
		// Flushed where EmitOutcome stood — the submitter is waiting on this
		// answer, and the record is durable either way.
		_, ferr = s.relay.Flush(ctx, st.GetOrderId())
		return ferr
	}
	// NOT FILLED — IT STAYS A DIRECT PUBLISH, and that is a decision, not an
	// omission. The order is resting or working at a venue; nothing terminal has
	// happened, so there is no state change accompanying this outcome and no
	// marker it would be truthful to stamp (outcome_announced_at means a TERMINAL
	// outcome was announced — setting it here would make a later fill's outcome
	// look already-announced to resume(), and orderOutcomeAnnounced would send a
	// redelivery of a genuinely unannounced FILLED order away). Enqueuing it would
	// mean opening a transaction whose only purpose is to carry it — an UPDATE
	// that changes no state and bumps the version every other writer is comparing
	// against. That is the same trade EmitOutcome's own comment refuses for
	// refuse() and outcomeReject(), and it is refused here for the same reason.
	//
	// WHAT THAT LEAVES, SAID RATHER THAN LEFT TO BE FOUND: if this publish fails,
	// the order is durable and correctly announced — its ACCEPTED and ROUTED FACTs
	// rode their own transactions — but the submitter never receives the answer to
	// its command, and no compensator covers a NON-terminal outcome. The command
	// nacks to the DLQ, where a redrive reaches resume(), which finds a live order
	// and re-drives or leaves it without re-answering. That is a gap in command
	// ANSWERING, not in the estate's record of the order, and closing it needs a
	// place to record "this command was answered" for a non-terminal order —
	// a fourth marker, which is the instalment pattern #292 exists to stop.
	return s.emitter.EmitOutcome(ctx, st.GetOrderId(),
		commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_ACCEPTED, "order admitted", "", "", now)
}

// resolveAccount answers: which exchange account may this portfolio spend from at this
// venue?
//
// Three outcomes, and the difference between them is the whole point:
//
//   - BOUND — the portfolio has an account at this venue. Stamp it. The router will
//     only reach the adapter holding that credential, so this order cannot touch any
//     other portfolio's collateral. This is the guarantee.
//
//   - UNBOUND, and the OMS REQUIRES a binding — refuse, under a code of its own:
//     VENUE_ACCOUNT_UNBOUND. Nothing is broken and no rule was breached; nobody has
//     said which collateral this portfolio may spend, and the platform will not
//     guess.
//
//   - UNBOUND, and the OMS does not require one — the order still executes, against
//     whatever account the adapter holds, TOGETHER WITH every other unbound portfolio
//     on that venue. That is a shared collateral pool, and it is the default state of
//     a platform that has never configured a binding. It gets stamped with the REAL
//     account (not left empty), so the ledger records where the cash actually went,
//     and it is said out loud once per portfolio. Silence here would be the ledger
//     reporting segregated books over a pool the exchange will liquidate as one.
//
// A resting order (no venue) and a paper deployment (no router) have no account,
// because they touch no collateral.
func (s *Service) resolveAccount(tenant, portfolio, mic string) (string, *RejectError) {
	if mic == "" || s.router == nil {
		return "", nil
	}
	if account, ok := s.accounts.Account(tenant, portfolio, mic); ok {
		if !s.router.Supports(mic, account) {
			// The binding names an account no adapter here holds. Executing anyway
			// would put the order on somebody else's collateral, which is precisely
			// what the binding forbids — so it can never be worked, and it is refused
			// at admission rather than left resting forever.
			return "", &RejectError{
				Code: "VENUE_ACCOUNT_UNREACHABLE",
				Msg: fmt.Sprintf("portfolio %s/%s is bound to account %q at %s, and no adapter on this OMS holds it",
					tenant, portfolio, account, mic),
			}
		}
		return account, nil
	}
	if s.requireAccount {
		return "", &RejectError{
			Code: "VENUE_ACCOUNT_UNBOUND",
			Msg: fmt.Sprintf("no venue account is bound to portfolio %s/%s at %s — nothing says whose collateral this order may spend",
				tenant, portfolio, mic),
		}
	}
	account, ok := s.router.AccountFor(mic)
	if !ok {
		return "", nil // no adapter at this MIC; the VENUE_NOT_CONFIGURED check owns that
	}
	s.noteShared(tenant, portfolio, mic, account)
	return account, nil
}

// noteShared says, once per (portfolio, venue), that this portfolio's orders are
// margining against an account nobody bound to it — which means against an account
// other portfolios are using too.
func (s *Service) noteShared(tenant, portfolio, mic, account string) {
	if s.sharedCount != nil {
		s.sharedCount.Inc()
	}
	key := tenant + "/" + portfolio + "@" + mic
	if _, seen := s.sharedOnce.LoadOrStore(key, struct{}{}); seen {
		return
	}
	s.logger.Warn("COLLATERAL IS SHARED — this portfolio is bound to no venue account, so its orders margin against whatever account the adapter holds, alongside every other unbound portfolio. An exchange liquidates per ACCOUNT: a drawdown in one of them consumes the margin of all of them, and the ledger will still show each portfolio's cash intact",
		"tenant", tenant,
		"portfolio", portfolio,
		"venue", mic,
		"account", account,
		"fix", fmt.Sprintf("bind it: OMS_VENUE_ACCOUNTS=%s/%s@%s=<account>", tenant, portfolio, mic),
	)
}

// work routes an admitted order to a venue and folds the resulting fills. A nil
// router leaves the order resting. Transient routing/publish errors are
// returned (retry); a venue that returns no fills leaves the order working.
func (s *Service) work(ctx context.Context, st *orderpb.OrderState, ver int64) (*orderpb.OrderState, int64, error) {
	// A SCHEDULED PARENT NEVER REACHES A VENUE (#435), and this is the one line
	// that guarantees it.
	//
	// It is HERE rather than at the single call site in admission because work()
	// has three callers — admission, resume()'s PENDING_NEW branch, and its
	// re-drive branch — and the sweep reaches the last two for any open order it
	// finds. A guard at admission would hold until the first pod restart, and then
	// the sweep would route every resting parent to a venue as one whole order:
	// the exact defect #435 exists to end, reintroduced by the machinery that
	// exists to recover from crashes.
	//
	// The parent's quantity is worked by its CHILDREN, which are ordinary orders
	// and reach a venue through this same function.
	if st.GetStatus() == orderpb.OrderStatus_ORDER_STATUS_WORKING_SCHEDULED {
		return st, ver, nil
	}
	if s.router == nil {
		return st, ver, nil
	}
	venue, err := s.router.Route(st)
	if errors.Is(err, execution.ErrNoVenue) {
		return st, ver, nil // nothing wired at all ⇒ rest (a paper deployment)
	}
	// A named-but-unconfigured venue is refused at admission and cannot reach here.
	// If it ever does, it is an error — never a silent rest. An order nobody can
	// execute must not look like an order that is working.
	if err != nil {
		return st, ver, err
	}
	routed := Route(st, s.now().UTC())
	// THE ROUTED FACT COMMITS WITH THE ROUTED STATE (#292).
	//
	// This pair had no compensator — the three *_announced_at markers cover
	// admission, cancellation and terminal outcomes, and ROUTED is none of
	// them. So a crash between the Save and the publish left an order stored as
	// working at a venue with nothing downstream told and nothing able to
	// notice. Built BEFORE the Save, like the fill fold: outbox.From refuses a
	// record with no tenant, and that refusal has to stop the transition rather
	// than commit a state whose FACT could never be published.
	fact, ferr := s.emitter.RoutedFact(ctx, routed.GetOrderId(), venue.MIC(), "", routed.GetAsOf().AsTime())
	if ferr != nil {
		return st, ver, ferr
	}
	// A conflict HERE is safe to abort on: nothing has reached the venue yet, so
	// another replica winning the race means it is working this order and this
	// delivery has nothing to contribute. Returning the error redelivers.
	if err := s.store.Save(ctx, routed, ver, []outbox.Record{fact}); err != nil {
		return st, ver, err
	}
	ver++
	// FLUSHED WHERE EmitRouted STOOD, so the ORDER OF FACTS ON THE BUS IS
	// UNCHANGED — only their durability is. ORDER_ROUTED must precede the fills
	// this loop is about to publish; a record left for the relay's tick would be
	// overtaken by them, and a consumer folding this order would see it working
	// at a venue after it had already filled.
	//
	// The error behaviour is exactly what EmitRouted's was: return, nack. What
	// differs is that the record is already durable, so the redelivery finds the
	// order routed and the relay publishes the original rather than nothing
	// having been announced at all.
	if _, err := s.relay.Flush(ctx, routed.GetOrderId()); err != nil {
		return st, ver, err
	}
	st = routed

	fills, err := venue.Execute(ctx, st)
	if err != nil {
		return st, ver, err
	}

	// THE VENUE HAS IT. Record that BEFORE folding any fill.
	//
	// This single timestamp is what makes a later resume safe. Without it, an
	// order stored as ROUTED is indistinguishable from an order the venue never
	// received — and the recovery action for those two is opposite: work it, or
	// freeze it. A crash between here and the fold leaves the ack recorded and
	// the fills unfolded, which reconciliation adopts from venue truth; a crash
	// before here leaves no ack, which is exactly right, because the venue never
	// confirmed anything.
	acked := cloneState(st)
	acked.VenueAckAt = timestamppb.New(s.now().UTC())
	// THE VENUE ALREADY HAS THE ORDER. A conflict here is not a benign loss: this
	// process holds the only record that the venue acknowledged, and another
	// replica has moved the order without it. Quarantine rather than return —
	// a redelivery would re-Execute against a venue that already has it.
	if err := s.store.Save(ctx, acked, ver, nil); err != nil {
		if errors.Is(err, ErrConflict) {
			return st, ver, s.quarantine(ctx, st, ver,
				"another replica moved this order while this delivery held an unrecorded venue ack; "+
					"the venue has the order and the store does not say so")
		}
		return st, ver, err
	}
	ver++
	st = acked

	// THE FOLD BELOW READS st, NOT THE STORE, AND THAT IS ONLY SAFE BECAUSE OF
	// THE PER-ORDER LOCK. Every caller of work() holds it (handleSubmit, resume),
	// and cancel and amend now WAIT for it (awaitClaim), so within this process
	// nothing can move this order between the read above and the Saves below.
	// ApplyFill's IsTerminal guard is therefore evaluated against state that is
	// still current — which is precisely what it was NOT before the per-order
	// lock reached cancel, and why a cancel landing inside venue.Execute could be
	// overwritten by a FILLED Save it never saw.
	//
	// ACROSS REPLICAS the version predicate on Store.Save now arbitrates (#122).
	// It does not make the fold's read of st current — it makes a stale write
	// REFUSED instead of silently applied, which is the honest half. Re-reading
	// here would only shrink the window, and a probabilistic fix on the capital
	// path is how the partition_key serialization claim got written in the first
	// place.
	for _, fill := range fills {
		next, aerr := ApplyFill(st, fill, fill.GetExecutedAt().AsTime())
		if aerr != nil {
			// An over-fill from the venue is a bug, not a transient fault; log
			// and stop working this order rather than loop.
			s.logger.Error("oms: venue fill rejected by aggregate", "order_id", st.GetOrderId(), "err", aerr)
			break
		}
		// THE FILL AND ITS FACT ARE NOW ONE WRITE (#292), AND THIS IS THE PAIR
		// THAT MOST NEEDED IT.
		//
		// This used to be store.Save followed by emitter.EmitFill: two independent
		// writes, and when the second failed the fund held a durable record of an
		// execution the estate had never heard of. Every other transition in this
		// service has a `*_announced_at` marker and a compensator behind it; this
		// one has neither, and it cannot have one — completeTerminalOutcome
		// explains why in its own comment. The individual Fill (fill_id, price,
		// venue_execution_id) lives ONLY in this loop variable and in the FACT
		// built from it; the stored OrderState keeps just the cumulative
		// aggregate. So a lost ORDER_FILLED was not "late", it was gone, and
		// tv-sync, accounting and audit never learned that money moved.
		//
		// THE FACT IS BUILT BEFORE THE WRITE, deliberately. If it cannot be
		// captured — no tenant on the delivery, a payload that will not marshal —
		// the fold must not be committed either: a fill whose announcement can
		// never be made must not be recorded as announced-by-construction. Same
		// discipline, same ordering, as admission's AcceptedFact.
		fact, ferr := s.emitter.FillFact(ctx, fill, next)
		if ferr != nil {
			return st, ver, ferr
		}
		if err := s.store.Save(ctx, next, ver, []outbox.Record{fact}); err != nil {
			// A CONFLICT HERE IS THE ONE THAT MATTERS, and quarantine is the only
			// honest action (#122, and the decision #117 declined to invent
			// without CAS to make it meaningful).
			//
			// This process holds a REAL FILL from the venue — the fund's money has
			// moved — and another replica has advanced the order without seeing
			// it. Retrying would fold the fill onto state we have not read;
			// dropping it would lose an execution that actually happened. Neither
			// is defensible, so the order is frozen and a human is told.
			//
			// The refused CAS took the FACT down with it (Store.Save writes
			// nothing at all on ErrConflict), so the quarantine below is not
			// freezing an order with an announcement queued for a transition that
			// never happened.
			if errors.Is(err, ErrConflict) {
				return st, ver, s.quarantine(ctx, st, ver, fmt.Sprintf(
					"another replica moved order %s while this delivery held an unfolded venue fill; "+
						"the fill is real and this process cannot safely apply it", st.GetOrderId()))
			}
			return st, ver, err
		}
		ver++
		// PUBLISH IT HERE, BEFORE ANYTHING ELSE HAPPENS TO THIS ORDER — the same
		// rule admission follows, for the same reason. handleSubmit publishes the
		// trailing CommandOutcome directly once this returns, and the next
		// iteration of this loop publishes the next fill; a queued FACT overtaken
		// by either is a consumer folding this order's history out of sequence.
		//
		// THE FAILURE BEHAVIOUR IS EXACTLY WHAT EmitFill's WAS: return, stop
		// folding, nack. What is different is that the record is already durable,
		// so the relay's next pass publishes the ORIGINAL fill rather than a
		// compensator having to invent one it cannot. An error path that returns
		// before reaching here (the quarantine above) leaves earlier fills queued,
		// which is the designed outcome: late, and on the relay's tick.
		if _, err := s.relay.Flush(ctx, next.GetOrderId()); err != nil {
			return st, ver, err
		}
		st = next
		if IsTerminal(st) {
			break
		}
	}
	return st, ver, nil
}

func (s *Service) handleCancel(ctx context.Context, payload []byte) error {
	var cmd orderpb.CancelOrder
	if err := proto.Unmarshal(payload, &cmd); err != nil {
		s.logger.Error("oms: malformed CancelOrder", "err", err)
		return nil
	}

	// TAKE THE PER-ORDER LOCK, AND WAIT FOR IT.
	//
	// This handler used to read the order and act on that read with NOTHING
	// excluding the goroutine that was, at that moment, inside venue.Execute for
	// the same order. The interleaving that breaks, all of it reachable in
	// production because submit, amend and cancel are three separate durables
	// with three cursors and three dispatch goroutines (pkg/bus/nats.go:183,
	// 202-206) — the partition_key does NOT serialize them, whatever the older
	// comments in postgres.go and store.go used to claim:
	//
	//   work() reads the order, calls the exchange, and blocks there.
	//   This handler loads the same order, sees it live, dispatches the
	//   withdrawal to the venue, Saves it CANCELLED and announces the FACT.
	//   work() returns holding a fill and folds it into the state it captured
	//   BEFORE the cancel. That snapshot is not terminal, so ApplyFill's
	//   IsTerminal guard (aggregate.go) — a real guard, evaluated against the
	//   wrong state — passes. Save is a blind upsert, so FILLED is written over
	//   CANCELLED and the ledger's last word contradicts the FACT the world was
	//   already told.
	//
	// WAIT, do not skip. claim()'s try semantics are correct for submit and
	// resume, where the holder finishes the job on the loser's behalf. Nobody
	// finishes a cancel on this delivery's behalf: giving up here is an
	// operator's withdrawal silently discarded.
	//
	// ACT AFTER, not before. Everything below reads order state, and order state
	// read alongside a running work() is a guess. `now` is taken after the wait
	// too — a timestamp captured before a multi-second block would date the
	// cancellation and its FACTs EARLIER than the fill they follow, inverting
	// the order of events for every downstream projection.
	release, err := s.awaitClaim(ctx, cmd.GetOrderId())
	if err != nil {
		// Exclusivity could not be established in time, so nothing about this
		// cancel has been decided — and saying nothing is the only honest
		// report. Do NOT fall through onto state that is about to change, and do
		// NOT emit an outcome: a REJECTED here would tell the caller the ledger
		// refused a cancel the ledger never even looked at. Returning the error
		// nacks; MaxAttempts is 1 and the DLQ is wired (cmd/oms/main.go), so the
		// command parks in dlq.order.order.cancel where an operator can see and
		// replay it, and the counter makes the stall alertable rather than
		// discoverable only by reading that queue.
		return s.abandonClaim("cancel", cmd.GetOrderId(), err)
	}
	defer release()

	// THE ORDER'S OWN BACKLOG GOES OUT BEFORE ITS CANCELLATION DOES (#292).
	//
	// A cancel can arrive on an order still sitting at PENDING_NEW whose
	// ORDER_ACCEPTED is committed to the outbox and not yet published — the
	// admission flush failed, or this cancel simply beat the relay. Publishing
	// ORDER_CANCELLED first would tell every downstream fold about the end of an
	// order it was never told the beginning of. Same inversion as
	// routed-before-accepted, same fix, and it is here rather than inside
	// completeCancelAnnouncement because the venue withdrawal below happens
	// first and must not be dispatched for an order this pod cannot announce.
	if _, ferr := s.relay.Flush(ctx, cmd.GetOrderId()); ferr != nil {
		return ferr
	}

	now := s.now().UTC()
	st, ver, err := s.store.Load(ctx, cmd.GetOrderId())
	if errors.Is(err, ErrNotFound) {
		return s.outcomeReject(ctx, cmd.GetOrderId(), "UNKNOWN_ORDER", "no such order", now)
	}
	if err != nil {
		return err
	}
	// Entitlement, BEFORE any side effect: tenant isolation proves this order
	// belongs to this tenant, not that this CALLER may touch it.
	if !entitledTo(cmd.GetMetadata().GetPrincipalPortfolios(), st.GetPortfolioId()) {
		return s.outcomeReject(ctx, cmd.GetOrderId(), ReasonNotEntitled,
			"principal is not entitled to this order's portfolio", now)
	}
	// A QUARANTINED ORDER IS NOT CANCELLABLE. Quarantine means the platform could
	// not establish what the venue did with this order — that is the whole reason
	// it froze instead of guessing. IsTerminal does not cover this: a quarantined
	// order commonly sits at ROUTED or PARTIALLY_FILLED, both non-terminal, so
	// without this check Cancel() below would happily withdraw it, closeAtVenue
	// would dispatch a REAL cancel to the exchange, and the order would be saved
	// CANCELLED — which IS terminal, so resume() and SweepInterrupted skip it
	// forever after. That converts "we do not know what the venue did with this
	// order" into a confident, possibly wrong, terminal answer, and silently
	// undoes the freeze a human was supposed to have to resolve. Refuse instead,
	// with the stored reason, so the operator does not need a second lookup to
	// learn what to do next.
	if q := st.GetQuarantine(); q != nil {
		return s.outcomeReject(ctx, cmd.GetOrderId(), "ORDER_QUARANTINED", fmt.Sprintf(
			"order is frozen: the platform could not establish what the venue did with it, so "+
				"acting on it now would be a guess. It must be resolved against the venue's own "+
				"order history before it can be cancelled. quarantine reason: %s (frozen %s ago, at %s)",
			q.GetReason(), quarantineAge(q, now), q.GetAt().AsTime().UTC().Format(time.RFC3339)), now)
	}
	// AN ALREADY-CANCELLED ORDER IS NOT AUTOMATICALLY A DUPLICATE.
	//
	// cancel_announced_at (order_events.proto:18) is what tells the two apart,
	// exactly as venue_ack_at tells a routed-but-unconfirmed order apart from a
	// venue's contradiction. Unset means the FIRST cancel got as far as Save but
	// its announcement (the ORDER_CANCELLED FACT, the outcome, or both) never
	// went out — a broker blip between the Save and the emit. Without this
	// branch the code below falls through to Cancel(), whose IsTerminal guard
	// fires on the already-CANCELLED status, and outcomeReject reports REJECTED
	// for a cancel that in fact already succeeded — while the FACT it never
	// published stays never published forever.
	//
	// Complete the announcement here instead. Do NOT call closeAtVenue: the
	// venue withdrawal was already dispatched by the delivery that got this
	// order to CANCELLED in the first place, and closeAtVenue is not idempotent
	// from the exchange's point of view — a second CancelOrder call is a second
	// venue request for an order the exchange may already have closed.
	// LeavesQuantity is still on the loaded record (Cancel does not zero it —
	// see aggregate.go), so the announcement carries the same cancelled
	// quantity the interrupted delivery would have.
	if st.GetStatus() == orderpb.OrderStatus_ORDER_STATUS_CANCELLED && st.GetCancelAnnouncedAt() == nil {
		return s.completeCancelAnnouncement(ctx, st, ver, st.GetLeavesQuantity(), now)
	}

	// Validate the withdrawal against the aggregate first (the terminal guard), so
	// a cancel the ledger refuses never reaches the exchange.
	next, cancelledQty, cerr := Cancel(st, now)
	if cerr != nil {
		var re *RejectError
		if errors.As(cerr, &re) {
			return s.outcomeReject(ctx, cmd.GetOrderId(), re.Code, re.Msg, now)
		}
		return cerr
	}
	// A WORKING PARENT IS WITHDRAWN FROM ITS CHILDREN, NOT FROM A VENUE (#435).
	//
	// The parent is at no venue — that is what WORKING_SCHEDULED means — so
	// closeAtVenue would route it and send the exchange a cancel for an order it
	// has never seen. Its quantity is at the venue in the shape of children, and
	// those are what have to be withdrawn.
	//
	// THE CHILDREN GO FIRST, AND THE ORDER OF THE TWO IS THE WHOLE SAFETY
	// ARGUMENT. If the parent were marked CANCELLED first and this failed, the
	// operator would be told the order was withdrawn while its slices were still
	// live and filling at an exchange — the exact lie closeAtVenue is dispatched
	// first to avoid, one level up. Doing it this way round, a failure leaves the
	// parent still WORKING_SCHEDULED and the cancel redelivers; and re-cancelling
	// a child that was already cancelled is safe, because the redelivery finds it
	// terminal and skips it.
	//
	// It is also why nothing is re-created behind us: the driver asks which
	// children EXIST, and a cancelled child still exists.
	if st.GetStatus() == orderpb.OrderStatus_ORDER_STATUS_WORKING_SCHEDULED {
		unsent, uerr := s.cancelChildren(ctx, st, now)
		if uerr != nil {
			return uerr
		}
		// THE QUANTITY THIS CANCEL WITHDRAWS IS THE QUANTITY THAT WAS NEVER SENT.
		//
		// Cancel() derives it from the parent's own leaves, which is the FULL
		// order — a parent never fills, so its aggregate never moves. Announcing
		// that would tell every consumer that 60 units were withdrawn when 30 had
		// already traded through children, and a fund's cancelled quantity would
		// exceed what it ever had working.
		cancelledQty = unsent
	} else {
		// Withdraw the order AT the venue before recording the cancellation.
		// Without this the ledger calls the order CANCELLED while it is still
		// resting — and still fillable — on the exchange. st (not next) carries
		// the pre-cancel quantities the close intent is built from.
		s.closeAtVenue(ctx, st, now)
	}

	// THE CANCELLATION AND ITS ANNOUNCEMENT ARE NOW ONE WRITE (#292).
	//
	// This used to be Save(CANCELLED), then completeCancelAnnouncement's two
	// publishes, then a third Save stamping cancel_announced_at. Four steps, three
	// of which could fail after the ledger had already committed the cancellation
	// — leaving an order the store calls CANCELLED that the world still believes
	// is live and fillable. The marker plus this handler's own already-CANCELLED
	// branch made that recoverable rather than lost, but recovery is not the same
	// as it not happening: until the next redelivery arrived, risk carried
	// exposure for a withdrawn order and tv-sync served it as working.
	//
	// BOTH RECORDS ARE BUILT BEFORE THE SAVE, the same discipline as admission and
	// the fill folds: outbox.From refuses a record with no tenant, and that
	// refusal has to stop the cancellation rather than commit one whose
	// announcement could never be made. The venue withdrawal above has already
	// happened either way — it is dispatched first on purpose, so the ledger never
	// calls an order cancelled while it is still resting on the exchange — so a
	// failure here nacks and the redelivery finds an order still live at the
	// ledger and re-runs the whole transition.
	cancelled, ferr := s.emitter.CancelledFact(ctx, next.GetOrderId(), cancelledQty, now)
	if ferr != nil {
		return ferr
	}
	outFact, ferr := s.emitter.OutcomeFact(ctx, next.GetOrderId(),
		commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED, "order cancelled", "", "", now)
	if ferr != nil {
		return ferr
	}
	// cancel_announced_at IS STAMPED IN THE SAME WRITE, and its meaning changes
	// with it — exactly as accepted_announced_at's did at admission. It used to
	// mean "both publishes returned nil"; it now means "the announcement is
	// committed for delivery", which is stronger, and it costs nothing because it
	// rides the UPDATE that was happening anyway.
	//
	// THAT IS ALSO WHAT KEEPS THIS HANDLER'S OWN COMPENSATOR FROM FIRING ON ITS
	// OWN WORK. The already-CANCELLED branch above republishes exactly when the
	// marker is unset; because marker and outbox record land together, a cancel
	// written by this code always has both, so that branch can only ever see a row
	// from before this change. The two partition the population rather than
	// overlapping on it — the same property admission has, and the reason the
	// duplicate-FACT window completeCancelAnnouncement documents is no longer
	// reachable from the live path.
	next.CancelAnnouncedAt = timestamppb.New(now)
	if err := s.store.Save(ctx, next, ver, []outbox.Record{cancelled, outFact}); err != nil {
		return err
	}
	// Flushed where the publishes stood: the operator issuing the cancel is
	// waiting on this outcome, so leaving it to the relay's tick would turn a
	// synchronous withdrawal into one that appears to hang. The records are
	// durable either way — this only decides whether the answer arrives now or on
	// the next pass.
	_, ferr = s.relay.Flush(ctx, next.GetOrderId())
	return ferr
}

// completeCancelAnnouncement publishes the ORDER_CANCELLED FACT and the
// EXECUTED command outcome for an order already saved CANCELLED, then stamps
// cancel_announced_at so a further redelivery hits the terminal-refusal branch
// instead of resuming again.
//
// IT IS NOW A COMPENSATOR FOR A CLOSED POPULATION (#292), and nothing else. The
// live cancel above enqueues both FACTs in the same Save that writes the
// CANCELLED state and stamps the marker with them, so no cancellation performed
// by this code can reach here: the marker is set the instant the state is. What
// CAN reach here is a row written by the previous code — CANCELLED, marker
// unset, and nothing in the outbox behind it, because the announcement was two
// independent publishes that failed. For those a direct publish is the only
// thing that can announce them, which is why this stays.
//
// # THE RETIREMENT CONDITION, STATED SO IT IS NOT GUESSED AT LATER
//
// This function, the already-CANCELLED branch in handleCancel that calls it, and
// eventually field 18 of order/v1/order_events.proto can go when NO CANCELLED
// ROW WITH cancel_announced_at UNSET CAN STILL EXIST. Unlike
// reannounceAccepted's condition that is a bounded wait rather than an open one:
// a CANCELLED order is terminal, so the population cannot grow once every pod
// runs this build, and it is drained by redelivery rather than sitting forever
// the way a resting PENDING_NEW order does. It is still an operational claim
// about the data, not a code condition — `SELECT count(*) FROM orders WHERE
// status = 5 /* CANCELLED */` with the marker unset is the query, and it is a
// human's to run. The proto field goes LAST and separately: delete the code
// first, ship it, then retire the field.
//
// A DUPLICATE ORDER_CANCELLED FACT IS AN ACCEPTED TRADE-OFF, ON THIS PATH ONLY.
// If EmitCancelled below succeeds and the Save at the end of this function then
// fails, the next redelivery re-enters here (cancel_announced_at is still unset)
// and re-emits both EmitCancelled and EmitOutcome. That folds "order X is
// cancelled" twice, which every downstream projection treats idempotently —
// versus the alternative this replaces, which lost the FACT entirely and told
// the caller REJECTED for a cancel that had already succeeded. Do not "fix" this
// into exactly-once without a fill_id-style dedup on the FACT itself; that is a
// larger change than this one, and it is now confined to a population that only
// shrinks.
//
// It is always called under the per-order lock — handleCancel is its only
// caller and takes that lock through awaitClaim before loading anything — so the
// duplicate this documents is a REDELIVERY duplicate only, never two goroutines
// announcing one cancellation concurrently.
func (s *Service) completeCancelAnnouncement(ctx context.Context, st *orderpb.OrderState, ver int64, cancelledQty *commonpb.Decimal, now time.Time) error {
	if err := s.emitter.EmitCancelled(ctx, st.GetOrderId(), cancelledQty, now); err != nil {
		return err
	}
	if err := s.emitter.EmitOutcome(ctx, st.GetOrderId(),
		commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED, "order cancelled", "", "", now); err != nil {
		return err
	}
	announced := cloneState(st)
	announced.CancelAnnouncedAt = timestamppb.New(now)
	return s.store.Save(ctx, announced, ver, nil)
}

// closeAtVenue withdraws a working order at the exchange holding it, recording
// the close in the in-flight registry BEFORE it races the venue so the
// reconciler's healing watchdog owns the acknowledgement window (the In-Flight
// Certainty seam). A venue with nothing resting externally (SimVenue) does not
// implement execution.Closer and is skipped — its cancel is ledger-only.
//
// It deliberately reports no error: the ledger must never freeze on a venue that
// will not answer. A failed or hung cancel stays TRACKED and the watchdog
// force-resolves it against venue truth. Returning it as a transient fault would
// instead nack the command and redeliver a cancel the venue may already have
// applied.
// It takes no version: it dispatches to the venue and writes nothing to the
// store, so there is no CAS for a version to govern.
func (s *Service) closeAtVenue(ctx context.Context, st *orderpb.OrderState, now time.Time) {
	if s.router == nil || s.closes == nil {
		return
	}
	venue, err := s.router.Route(st)
	if err != nil {
		return // nothing is working this order at a venue
	}
	closer, ok := venue.(execution.Closer)
	if !ok {
		return // no order resting at an exchange to withdraw
	}

	// Track BEFORE dispatch: if the call hangs, times out ambiguously, or the
	// process dies mid-flight, the watchdog still sees the close. Tracking after
	// the ack would cover none of those windows.
	//
	// A cancelled resting order carries NO residual exposure to sweep — it has not
	// traded, so withdrawing it opens nothing (see execution.CloseIntent). Leaves
	// and SweepSide stay zero and the watchdog force-clears without sweeping. If
	// the cancel raced a fill, the watchdog's venue query returns the order
	// terminal and StateHealed carries that truth: we learn the fill, we never
	// invent an offsetting trade.
	// A SelfHealing venue (an out-of-process adapter) tracks and heals its own
	// closes. Tracking it here too would leave an entry nobody resolves in a
	// registry the in-process OKX reconciler drains indiscriminately — and it would
	// then try to heal another venue's order. Hands off.
	if _, selfHealing := closer.(execution.SelfHealing); !selfHealing {
		s.closes.Track(execution.CloseIntent{
			OrderID:      st.GetOrderId(),
			InstrumentID: st.GetInstrumentId(),
			RequestedAt:  now,
		})
	}
	if err := closer.CancelOrder(ctx, st); err != nil {
		s.logger.Error("oms: venue cancel unconfirmed — left to the healing watchdog",
			"order_id", st.GetOrderId(), "venue", venue.MIC(), "err", err)
		return
	}
	s.closes.Resolve(st.GetOrderId()) // venue confirmed the withdrawal — nothing to heal
}

func (s *Service) handleAmend(ctx context.Context, payload []byte) error {
	var cmd orderpb.AmendOrder
	if err := proto.Unmarshal(payload, &cmd); err != nil {
		s.logger.Error("oms: malformed AmendOrder", "err", err)
		return nil
	}

	// TAKE THE PER-ORDER LOCK, AND WAIT FOR IT — the same reasoning as
	// handleCancel, and the same defect pointed the other way. An amend racing a
	// fill is worse than it looks: Amend() clones the state it was handed and
	// rewrites ordered/leaves quantity on it, carrying that snapshot's STATUS
	// along, so an amend built on a pre-fill read Saves ROUTED with a positive
	// leaves quantity over a FILLED order — a completed trade un-filled in the
	// ledger, with an open quantity the exchange does not have. Its
	// "new quantity below filled quantity" guard (aggregate.go) is equally
	// compromised: it compares against a filled_quantity that a fold in flight
	// has already moved, so it can also shrink an order below what has actually
	// traded.
	release, err := s.awaitClaim(ctx, cmd.GetOrderId())
	if err != nil {
		// Same posture as the cancel path: nothing was decided, so nothing is
		// announced, and the command parks in the DLQ rather than rewriting the
		// size or price of an order whose true state it cannot read.
		return s.abandonClaim("amend", cmd.GetOrderId(), err)
	}
	defer release()

	// Same reasoning as handleCancel's flush: an amend on an order whose
	// ORDER_ACCEPTED is still in the outbox would publish its outcome before the
	// order was ever announced (#292).
	if _, ferr := s.relay.Flush(ctx, cmd.GetOrderId()); ferr != nil {
		return ferr
	}

	now := s.now().UTC()
	st, ver, err := s.store.Load(ctx, cmd.GetOrderId())
	if errors.Is(err, ErrNotFound) {
		return s.outcomeReject(ctx, cmd.GetOrderId(), "UNKNOWN_ORDER", "no such order", now)
	}
	if err != nil {
		return err
	}
	// Entitlement, BEFORE the aggregate mutates: an amend rewrites the size or
	// price of a live order, so it carries the same exposure as a cancel.
	if !entitledTo(cmd.GetMetadata().GetPrincipalPortfolios(), st.GetPortfolioId()) {
		return s.outcomeReject(ctx, cmd.GetOrderId(), ReasonNotEntitled,
			"principal is not entitled to this order's portfolio", now)
	}
	// A QUARANTINED ORDER IS NOT AMENDABLE, for the same reason it is not
	// cancellable (see handleCancel). Amend rewrites the size or price the
	// aggregate believes is resting at the venue; if that belief is exactly what
	// quarantine says cannot be trusted, amending it is a guess dressed up as an
	// instruction. IsTerminal does not catch this — a quarantined order is
	// typically non-terminal — so without this check Amend() below would apply
	// and persist, again with no venue confirmation that the venue agrees an
	// order even exists to amend. Refuse, with the stored reason inline.
	if q := st.GetQuarantine(); q != nil {
		return s.outcomeReject(ctx, cmd.GetOrderId(), "ORDER_QUARANTINED", fmt.Sprintf(
			"order is frozen: the platform could not establish what the venue did with it, so "+
				"acting on it now would be a guess. It must be resolved against the venue's own "+
				"order history before it can be amended. quarantine reason: %s (frozen %s ago, at %s)",
			q.GetReason(), quarantineAge(q, now), q.GetAt().AsTime().UTC().Format(time.RFC3339)), now)
	}
	next, aerr := Amend(st, &cmd, now)
	if aerr != nil {
		var re *RejectError
		if errors.As(aerr, &re) {
			return s.outcomeReject(ctx, cmd.GetOrderId(), re.Code, re.Msg, now)
		}
		return aerr
	}
	// THE AMENDED STATE AND ITS OUTCOME COMMIT TOGETHER (#292). Like ROUTED,
	// this pair had no marker: nothing re-announces an amend whose outcome was
	// lost, so the caller waits on an outcome that will never arrive while the
	// amendment itself is durable. Built before the Save so a record that could
	// never be published stops the amend instead of committing half of it.
	fact, ferr := s.emitter.OutcomeFact(ctx, next.GetOrderId(),
		commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED, "order amended", "", "", now)
	if ferr != nil {
		return ferr
	}
	if err := s.store.Save(ctx, next, ver, []outbox.Record{fact}); err != nil {
		return err
	}
	// Flushed where EmitOutcome stood: the caller is waiting on this outcome, so
	// deferring it to the relay's tick would turn a synchronous amend into one
	// that appears to hang. The record is durable either way — this only decides
	// whether the answer arrives now or on the next pass.
	_, ferr = s.relay.Flush(ctx, next.GetOrderId())
	return ferr
}

// refuse emits an ORDER_REJECTED FACT and a REJECTED command outcome, then acks
// (returns nil) — a refused command is terminal.
// ReasonUnsignable is the outcome code for an order that requires a second
// signature but carries nothing an approval could be bound to — no
// authenticated proposer, or terms that cannot be digested.
//
// IT IS A REFUSAL AND NOT A HOLD. Holding such an order would put it in a queue
// where every approval attempt fails for a reason the approver cannot fix, which
// is a silent drop with extra steps. The caller is told now, while the person
// who could correct the command is still there.
const ReasonUnsignable = "DUAL_CONTROL_UNSIGNABLE"

// ReasonApprovalExpired is the outcome code for a held order that nobody signed
// before its deadline.
//
// IT IS A REJECTION, AND THAT IS THE HONEST ANSWER. The order was never
// admitted — it is not in the order book, it has no status to move, and a ninth
// ORDER_STATUS would have made it visible to every consumer as work. What
// happened is what happens to an order that breaches compliance or names an
// unroutable venue: it will not trade, and the estate is told so on the FACT it
// already folds. The CODE is what separates "nobody signed it" from "compliance
// refused it", which is the distinction an examiner actually asks for.
const ReasonApprovalExpired = "DUAL_CONTROL_EXPIRED"

// ExpireProposals announces every held order whose deadline has passed (#539).
//
// # The failure this ends
//
// OrderPendingApproval's own doc promises a proposal "EXPIRES rather than
// resting forever looking like a decision that was made". Nothing delivered on
// that: Pending filtered the row out and no FACT went anywhere, so a trader saw
// ORDER_PENDING_APPROVAL and then silence — permanently, with no CommandOutcome
// ever arriving. That is the exact shape #410's acceptance names as the failure
// it exists to prevent, reintroduced by the control at the far end of its own
// timeout.
//
// # Why it is safe on two replicas without a lease
//
// It takes no lock and elects no leader. AnnounceExpiry is a conditional UPDATE
// whose rows-affected is the verdict, so both sweepers may find the same row and
// exactly one announces it — the same argument Claim makes on the approval path,
// and the same one the schedule driver makes for needing no lease at all.
//
// # A failure is not fatal
//
// One proposal that cannot be announced must not stop the rest, and a sweep that
// fails must not take the OMS down: the orders it would announce are already
// held, and the next tick tries again. The caller counts failures.
func (s *Service) ExpireProposals(ctx context.Context) error {
	now := s.now().UTC()
	due, err := s.store.Proposals().ExpiredUnannounced(ctx, now, 0)
	if err != nil {
		return err
	}
	var failed int
	for _, p := range due {
		if err := s.expireOne(ctx, p, now); err != nil {
			// Logged and counted, never returned: a poison row must not wedge the
			// queue behind it for every other trader waiting on an answer.
			s.logger.Error("oms: could not announce a proposal's expiry",
				"order_id", p.ID, "proposer", p.Proposer, "expires_at", p.ExpiresAt, "err", err)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("oms: %d of %d proposal expiries could not be announced", failed, len(due))
	}
	return nil
}

// expireOne announces one expiry, with the FACTs riding the same transaction as
// the marker.
//
// BUILT BEFORE THE WRITE, so an expiry that cannot be captured — no tenant on
// the context, a payload that will not marshal — stops here rather than marking
// a proposal announced that nobody will ever hear about. The same discipline
// hold() follows, and the reason the marker is a column rather than a log line.
func (s *Service) expireOne(ctx context.Context, p OrderProposal, now time.Time) error {
	reason := "no second signature was given before " + p.ExpiresAt.UTC().Format(time.RFC3339)
	rejected, err := s.emitter.RejectedFact(ctx, p.ID, ReasonApprovalExpired, reason, now)
	if err != nil {
		return err
	}
	// THE OUTCOME IS WHAT THE SUBMITTER IS WAITING ON. service.go's own note on
	// the held path says a held order's outcome FACT is the one its approval or
	// its expiry produces; without this half the caller waits forever on a command
	// that was answered by a timeout nobody published.
	outcome, err := s.emitter.OutcomeFact(ctx, p.ID,
		commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED, reason, ReasonApprovalExpired, "", now)
	if err != nil {
		return err
	}

	announced, err := s.store.Proposals().AnnounceExpiry(ctx, p.ID, now, []outbox.Record{rejected, outcome})
	if err != nil {
		return err
	}
	if !announced {
		// The other replica got it, or it was signed between the read and here.
		// Either way this pod announces nothing.
		return nil
	}
	s.logger.Warn("oms: a held order EXPIRED without a second signature and will not trade (#539)",
		"order_id", p.ID, "portfolio_id", p.PortfolioID, "proposer", p.Proposer,
		"proposed_at", p.CreatedAt, "expires_at", p.ExpiresAt)

	// Flushed for the same reason hold() flushes: a trader whose order just died
	// should not wait a relay tick to find out. The record is durable either way.
	if _, err := s.relay.Flush(ctx, p.ID); err != nil {
		s.logger.Warn("oms: expiry announced but not yet on the bus", "order_id", p.ID, "err", err)
	}
	return nil
}

// handleApprove releases a held order on a SECOND SUBJECT'S SIGNATURE (#539).
//
// # Why this is not a query, and not a store method
//
// Approving CAUSES AN ADMISSION. It ends by replaying the proposed command
// through submit — the one path an order comes into existence on — so the
// compliance gate, the venue checks and the outbox are all still in front of it.
// That is also why the approval arrives as a bus command rather than an RPC on
// OrderQueryService: a read surface that could cause a submission would be a
// second door into the capital path with none of that behind it.
//
// # The ordering, and both halves of it are load-bearing
//
// The rule is checked BEFORE the claim, and the claim BEFORE the admission.
// datamaster settled the first half on the override path: consuming the
// proposal on a refused approval would let one person destroy a colleague's
// pending decision simply by attempting their own. The second half is stronger
// here than it is there — Claim is the serialisation point across the OMS's two
// replicas, and admitting without winning it means one approval becomes two
// deliveries to a live venue.
//
// # A refusal acks, AND IT IS NOW RECORDED ON THE QUEUE (#558)
//
// Every refusal below returns nil after a WARN naming the reason. A refused
// approval is not a defect in the command that can be fixed by redelivering it,
// and the ORDER is not rejected either — it is still pending, awaiting a
// signature somebody else may legitimately give. The order stays exactly as it
// was.
//
// THE REFUSAL IS NOW ALSO WRITTEN ONTO THE PROPOSAL, and ListPendingApprovals
// carries it: an approver who returns to the queue sees state = "refused", the
// reason code, who was refused and when — instead of an order that looks
// identical to one nobody has touched. This corrects what this doc used to call
// "a stated gap, not an oversight". The gap was real; the ruling on #558 was to
// close it HERE rather than with a new vocabulary.
//
// WHY NOT A CommandOutcome, AND WHY NOT A FACT. Both were the obvious answers
// and both are worse:
//
//   - A new COMMAND_OUTCOME_STATUS_REFUSED is a command.v1 change every consumer
//     folds. All four existing statuses are TERMINAL, so none of them can be sent
//     — ACCEPTED and REJECTED would both lie about an order that is still
//     pending — and adding a fifth migrates the meaning of the enum for every
//     consumer that assumed four were exhaustive, to say something only the
//     approver needs.
//   - A dedicated OrderApprovalRefused FACT is a new subject, a new tenancy
//     grant and a new message with no consumer. #563 settled exactly this on the
//     sibling act: datamaster's lapsed override proposal is LISTED on the surface
//     the proposer already uses, carrying a state, precisely because a new
//     subject would have been published to nobody. Read handlePendingOverrides
//     and proposalJSON there; this is the same shape.
//
// The queue became a usable answer when #557 mounted
// GET /v1/orders/pending-approvals, gated on authz.Approve. When #558 was filed
// that route did not exist, which is why "poll the queue" was dismissed then and
// is the ruling now.
//
// # WHAT THE APPROVER SEES, PER REFUSAL, INCLUDING THE ONES WITH NOWHERE TO WRITE
//
// Recording needs a proposal to write on, and only three of the six refusals
// #558 enumerates end up as a REFUSED entry on the queue. Saying what each of the
// other three does instead is the point — a branch nobody wrote down is what this
// whole issue was:
//
//   - self-approval, a respelling of the proposer, a digest covering other terms:
//     the proposal exists and stays PENDING. Recorded, and the queue entry the
//     approver reloads says state = "refused" with the reason. THIS IS THE CASE
//     #558 IS ABOUT.
//   - expired: recorded, but Pending deliberately excludes expired work so the
//     entry is NOT on the queue. The approver's answer is the terminal
//     ORDER_REJECTED that #547's sweeper publishes for exactly this — expiry is
//     terminal, so a FACT works there and is already shipped.
//   - no pending proposal under that id: THERE IS NOTHING TO WRITE ON, and
//     inventing a row would let a subject holding only the approve authority
//     create a proposal. The approver sees the order absent from the queue, which
//     is the honest answer: either it was never held, or it was already decided
//     and is now a live order. Logged at WARN with the id, and that is the whole
//     of what this branch can offer.
//   - lost the claim race: attempted, and RecordRefusal reports false because the
//     proposal is already decided. Its row is the OMS's only durable evidence
//     that two people signed and a refusal must not overwrite it. The approver
//     sees the order gone from the queue and live in the book — somebody else's
//     signature released it, which is a legible answer and not a silence.
//
// The four checks ABOVE the proposal lookup — a malformed command, no order id,
// a target_id that names another aggregate, no authenticated issuer — cannot
// record either, and deliberately: they are refusals of the COMMAND, before any
// proposal or any trustworthy identity is in hand. Writing a refusal attributed
// to an unauthenticated issuer would put a name on the approver's queue that
// nobody proved. They stay operator-facing at ERROR.
func (s *Service) handleApprove(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	var cmd orderpb.ApproveOrder
	if err := proto.Unmarshal(payload, &cmd); err != nil {
		s.logger.Error("oms: malformed ApproveOrder", "err", err)
		return nil
	}
	// Uniform with the other three command decoders. ApproveOrder carries no
	// Decimal today; the check costs nothing and means nobody has to ask why this
	// one decoder is the exception when a field is added.
	if field, in := dec.InDomainDeep(&cmd); !in {
		s.logger.Error("oms: ApproveOrder carries an out-of-domain exponent — refusing", "field", field)
		return nil
	}
	now := s.now().UTC()

	orderID := cmd.GetOrderId()
	if orderID == "" {
		s.logger.Error("oms: ApproveOrder names no order — refusing")
		return nil
	}
	// The command-class contract: metadata.target_id MUST equal order_id. A
	// mismatch means the signed target and the released order are different
	// aggregates, which is the whole attack the digest exists to stop arriving
	// one field earlier.
	if tid := cmd.GetMetadata().GetTargetId(); tid != "" && tid != orderID {
		s.logger.Error("oms: ApproveOrder target_id does not match order_id — refusing",
			"target_id", tid, "order_id", orderID)
		return nil
	}
	// THE APPROVER IS THE AUTHENTICATED ISSUER, never self-asserted. The gateway
	// binds it from the verified principal and the producer checks it again
	// (AUTH-01c); an unauthenticated approval would make the self-approval check
	// pass vacuously, because every approver differs from "".
	approver := strings.TrimSpace(cmd.GetMetadata().GetIssuer())
	if approver == "" {
		s.logger.Error("oms: ApproveOrder carries no authenticated issuer — refusing",
			"order_id", orderID)
		return nil
	}

	p, ok, err := s.store.Proposals().Get(ctx, orderID)
	if err != nil {
		return err // transient: let it redeliver
	}
	if !ok {
		// Nothing is held under this id. An approval is a RELEASE, never an
		// origin — admitting here would let a subject holding only the approve
		// authority originate an order outright, which is precisely the person
		// who must not be able to.
		//
		// AND THERE IS NOWHERE TO RECORD THE REFUSAL (#558). Creating a row to
		// carry it would be creating a proposal, which is the same defect one
		// step removed. The approver's answer is the order's ABSENCE from the
		// queue — never held, or already decided — and this line is the
		// operator's.
		s.logger.Warn("oms: approval names an order with no pending proposal — nothing released, "+
			"and no refusal could be recorded because there is no proposal to record it on",
			"order_id", orderID, "approver", approver)
		return nil
	}

	// THE RULE, BEFORE THE CLAIM. Approve refuses self-approval (normalised for
	// case and surrounding space), a digest covering other terms, and an expired
	// proposal — and on every one of those the proposal is left PENDING.
	appr, err := p.Approve(approver, cmd.GetDigest(), now)
	if err != nil {
		reason := approvalRefusal(err)
		s.logger.Warn("oms: approval REFUSED — the order remains held",
			"order_id", orderID, "approver", approver, "proposer", p.Proposer,
			"reason", reason, "err", err)
		// THE REFUSAL GOES ON THE QUEUE, AND A FAILURE TO WRITE IT NACKS (#558).
		// Returning nil here would ack a delivery whose only visible trace is a
		// log line the approver cannot read — the exact silence this branch was
		// filed for, restored by an unchecked error. Redelivery re-refuses and
		// re-records, which is idempotent: the columns are overwritten with the
		// same values.
		if _, rErr := s.store.Proposals().RecordRefusal(ctx, orderID, approver, reason, now); rErr != nil {
			return fmt.Errorf("oms: record refusal on order %s: %w", orderID, rErr)
		}
		return nil
	}

	// THE CLAIM, BEFORE THE ADMISSION. This is the serialisation point: the OMS
	// ships two replicas, so a mutex here would say nothing. Losing it means
	// somebody else decided this proposal between the read and now, and admitting
	// anyway would send one order to a venue twice.
	claimed, err := s.store.Proposals().Claim(ctx, orderID, approver, now)
	if err != nil {
		return err // transient
	}
	if !claimed {
		// TWO SHAPES REACH HERE, and they are different answers to the approver.
		// Usually somebody else's signature landed between the read and now: the
		// proposal is decided, RecordRefusal declines to touch the row that is the
		// OMS's only evidence two people signed, and the approver sees the order
		// gone from the queue and live in the book.
		//
		// The other shape is a proposal STILL PENDING that Claim's SQL predicate
		// refused where dualcontrol.Approve's Go comparison did not — the two
		// normalise a subject independently, and a divergence between them would
		// otherwise be the most invisible refusal of the lot. There the refusal is
		// recorded and reaches the queue like any other.
		recorded, rErr := s.store.Proposals().RecordRefusal(ctx, orderID, approver, RefusalLostTheClaim, now)
		if rErr != nil {
			return fmt.Errorf("oms: record refusal on order %s: %w", orderID, rErr)
		}
		s.logger.Warn("oms: approval lost the claim — this proposal was already decided",
			"order_id", orderID, "approver", approver, "refusal_recorded", recorded)
		return nil
	}

	// REPLAYED AS BYTES, THROUGH THE SAME DECODER THE BUS FEEDS. The stored
	// command is re-validated exactly as a fresh submission would be rather than
	// trusted because it once passed; the round trip is what makes "the approved
	// path and the submit path are one path" true rather than nearly true.
	blob, err := proto.Marshal(p.Command)
	if err != nil {
		return fmt.Errorf("oms: re-encode approved order %s: %w", orderID, err)
	}
	return s.submit(ctx, env, blob, &appr)
}

// The refusal vocabulary. It is a CLOSED SET and a client branches on it, so the
// values are constants rather than literals scattered across a switch, a log
// call and a doc comment: an approver refused for self-approval needs a different
// affordance (find another approver) from one refused for a changed payload (the
// terms moved; the order must be re-proposed).
//
// ONE VALUE PER internal/dualcontrol SENTINEL, plus the claim race, which is not
// a dualcontrol verdict at all — it is this store's arbitration between two
// replicas. test/arch/approval_refusal_vocabulary_test.go fails the build if
// dualcontrol grows a sentinel this switch does not name, because the default arm
// would silently label it "malformed" and tell the approver something untrue.
const (
	RefusalSelfApproval  = "self_approval"
	RefusalExpired       = "expired"
	RefusalPayloadChange = "payload_changed"
	RefusalMalformed     = "malformed"
	RefusalLostTheClaim  = "lost_the_claim"
	// RefusalUnclassified is the default arm, and it exists so that "this
	// approval was refused for a reason we can name" and "this approval was
	// refused and the OMS cannot say why" are DIFFERENT answers on the approver's
	// queue. Folding the second into "malformed" would tell an approver their
	// proposal is not well-formed when the truth is that a refusal arrived
	// carrying an error this switch has never seen — which is a defect in the
	// OMS, not in their order, and points at a different person to go and ask.
	RefusalUnclassified = "unclassified"
)

// approvalRefusal maps a dualcontrol refusal to a stable label — for the log, so
// "who is being refused and why" is greppable without parsing prose, and for the
// proposal row, so the approver's queue says the same thing the operator's log
// says rather than a second wording of it.
func approvalRefusal(err error) string {
	switch {
	case errors.Is(err, dualcontrol.ErrSelfApproval):
		return RefusalSelfApproval
	case errors.Is(err, dualcontrol.ErrExpired):
		return RefusalExpired
	case errors.Is(err, dualcontrol.ErrPayloadChanged):
		return RefusalPayloadChange
	case errors.Is(err, dualcontrol.ErrMalformed):
		return RefusalMalformed
	default:
		// NOT "malformed". Everything dualcontrol can return is named above, and
		// the arch guard keeps that true, so reaching here means an error from
		// somewhere else entirely. Naming it as a malformed proposal would send
		// the approver to fix an order that is fine.
		return RefusalUnclassified
	}
}

// hold puts an order that requires a second signature into the proposals table
// instead of admitting it (#410).
//
// # The two writes are one transaction, and that is the whole shape
//
// The proposal row and the ORDER_PENDING_APPROVAL record commit together
// (ProposalStore.Put). Publishing inline instead would have forced a choice
// between failing the order when the broker is down and dropping the FACT — and
// dropping it is the exact failure this control exists to end, because a held
// order is invisible everywhere else: it is not in the book, so no blotter, no
// projection and no outcome consumer would ever hear of it. #498 made the same
// argument for the override path.
//
// # What the caller is told
//
// Nothing else. There is deliberately NO CommandOutcome here, and that is a
// stated deviation from event-class-rules §2 rather than an oversight:
// CommandOutcomeStatus has four values and every one of them is TERMINAL. This
// command's disposition is genuinely not decided yet. ACCEPTED would tell every
// downstream fold the order was admitted — it was not, and nothing will route
// it; REJECTED would tell them it is dead — it is not, and an approval will
// bring it back under the same order_id. A held order's outcome FACT is the one
// its approval or its expiry produces, and the pending FACT above is what says
// so in the meantime. The alternative — a lie in a field consumers switch on —
// is worse than a late outcome.
//
// # The proposer is the command's ISSUER, and for an automated order that is not a person
//
// The gateway binds Issuer from the authenticated principal and the producer
// re-checks it (AUTH-01c), so for a human order this is a real, unforgeable
// subject. TWO SERVICES PUBLISH ORDERS WITH NO HUMAN BEHIND THEM —
// webhook-ingest fans out a strategy signal, optimization materializes a
// rebalance — and their issuers are "strategy:{id}" and the caller's id. Held
// under this control, such an order requires ONE human to approve a machine's
// proposal. That is a weaker property than four-eyes and it is the correct
// behaviour for the population: refusing to hold them would exempt exactly the
// orders no person ever looked at. It is recorded here because the audit trail
// will show a proposer that is not a person, and an auditor must not read that
// as two humans.
func (s *Service) hold(ctx context.Context, cmd *orderpb.SubmitOrder, d approval.Decision, now time.Time) error {
	// AN ORDER WHOSE TERMS CANNOT BE DIGESTED IS NEVER ADMITTED QUIETLY. Decide
	// records the failure rather than swallowing it, and an order no approval
	// could ever cover must not sit in a queue pretending to be approvable.
	if d.DigestErr != nil {
		return s.refuse(ctx, cmd.GetOrderId(), ReasonUnsignable, d.DigestErr.Error(), now)
	}
	proposer := strings.TrimSpace(cmd.GetMetadata().GetIssuer())
	if proposer == "" {
		// dualcontrol.Propose refuses this too. Refusing HERE gives the caller a
		// rejection with a reason instead of a nacked delivery that redelivers
		// forever, and it is the same defect class #444 fixed: an identity that
		// is not really there makes the self-approval check pass vacuously,
		// because every approver differs from "".
		return s.refuse(ctx, cmd.GetOrderId(), ReasonUnsignable,
			"an order requiring dual control carries no authenticated issuer, so no approver "+
				"could ever be shown to differ from its proposer", now)
	}

	// ONE PRODUCER OF dualcontrol.ActOrderSubmission — approval.Propose. The act
	// constant, the subject and the digest are decided in one place so they
	// cannot drift from what the approve side re-derives.
	base, err := approval.Propose(cmd.GetOrderId(), approval.TermsOfSubmit(cmd), proposer, now, dualcontrol.DefaultTTL)
	if err != nil {
		// Malformed enough that no approval could ever cover it. Same refusal as
		// the digest failure above, and for the same reason.
		return s.refuse(ctx, cmd.GetOrderId(), ReasonUnsignable, err.Error(), now)
	}
	prop := OrderProposal{Proposal: base, Command: cmd, PortfolioID: cmd.GetPortfolioId()}

	// BUILT BEFORE THE WRITE, so a FACT that cannot be captured — no tenant on
	// the delivery, a payload that will not marshal — stops the hold rather than
	// leaving an order held that nobody will ever hear about. Exactly the
	// discipline the AcceptedFact call on the admission path follows.
	fact, err := s.emitter.PendingApprovalFact(ctx, prop)
	if err != nil {
		return err
	}
	if err := s.store.Proposals().Put(ctx, prop, []outbox.Record{fact}); err != nil {
		if errors.Is(err, ErrProposalExists) {
			// A REDELIVERY OF A COMMAND ALREADY HELD. Ack and announce nothing:
			// the first delivery's proposal owns this order, and its FACT went
			// out with it. Without this the same held order would be announced
			// once per redelivery. The loser's outbox record rolled back with
			// its INSERT, so there is nothing to flush either.
			return nil
		}
		return err
	}

	// NOT COUNTED ON kanz_oms_order_signatures_total, deliberately. That counter
	// measures orders that WENT THROUGH and how many people signed them; a held
	// order went through on nobody's signature yet. Counting it as single_signed
	// would inflate the exact number the arming decision rests on with orders the
	// control caught — the metric would get worse as the control started working.
	s.logger.Warn("order at or above the dual-control threshold is HELD for a second signature "+
		"and has NOT been admitted (#410)",
		"order_id", cmd.GetOrderId(), "portfolio_id", cmd.GetPortfolioId(),
		"proposer", proposer, "act", string(prop.Act), "digest", prop.Digest,
		"expires_at", prop.ExpiresAt)

	// Publish now, for the same reason admission does: the FACT is durable, but
	// a held order nobody hears about until the next relay tick is a trader
	// staring at a blotter with nothing on it.
	if _, err := s.relay.Flush(ctx, cmd.GetOrderId()); err != nil {
		return err
	}
	return nil
}

func (s *Service) refuse(ctx context.Context, orderID, code, reason string, t time.Time) error {
	if err := s.emitter.EmitRejected(ctx, orderID, code, reason, t); err != nil {
		return err
	}
	return s.outcomeReject(ctx, orderID, code, reason, t)
}

func (s *Service) outcomeReject(ctx context.Context, orderID, code, reason string, t time.Time) error {
	return s.emitter.EmitOutcome(ctx, orderID,
		commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED, reason, code, "", t)
}

// ReasonNotEntitled is the outcome code for a command whose issuer holds no
// entitlement to the target order's portfolio.
const ReasonNotEntitled = "NOT_ENTITLED"

// entitledTo reports whether a principal scoped to allowed may act on an order
// belonging to portfolio.
//
// EMPTY DENIES, and the rule now lives in pkg/auth beside the READ-path rule it
// deliberately contradicts (#225). It is one line here so that the two cannot be
// "unified" by someone who has only read one of them: the shortest fix for a
// NOT_ENTITLED outage is to make this permissive on empty, and that converts a
// trading-control outage into an authorization bypass across every portfolio in
// the tenant. auth.PortfolioEntitled carries the argument.
func entitledTo(allowed []string, portfolio string) bool {
	return auth.PortfolioEntitled(allowed, portfolio)
}

// delegatedIssuerPrefix marks a command the api-gateway minted on behalf of an
// authenticated human. bindMetadata stamps Issuer AND PrincipalPortfolios from
// the same verified principal, overriding whatever the client sent, so the two
// fields cannot disagree on anything that came through the gateway.
const delegatedIssuerPrefix = "user:"

// delegatedAndEntitled is the entitlement gate for SubmitOrder, and it is NOT
// simply entitledTo — because not every order on this bus has a human behind it.
//
// TWO SERVICES PUBLISH order.order.submit BESIDES THE GATEWAY, and neither has a
// portfolio claim to carry: webhook-ingest fans a strategy signal out per venue
// (internal/signal/translate, Issuer "strategy:{id}") and optimization
// materializes a rebalance proposal (services/optimization/internal/bridge,
// Issuer from the caller). Both are admitted to the subject by name in
// infra/nats/tenancy.yaml. Running the human rule over them would refuse every
// automated order on the platform NOT_ENTITLED — a bigger outage than the one
// #225 reports, and one no operator could clear, because there is no claim to
// populate. Their authorization is the SPIFFE identity that let them publish at
// all, plus the pre-trade compliance gate below; portfolio entitlement is a
// statement about a person's token and they have none.
//
// SO THE DISCRIMINATOR IS THE ISSUER, AND IT IS AN ALLOW-LIST, NOT A
// FALLTHROUGH: only a "user:" issuer is entitlement-checked, and for it an empty
// list denies. A client cannot reach the non-delegated branch — the gateway
// overwrites Issuer on every command it publishes — so the only way to take it
// is to already hold a NATS publish grant for this subject, which is a caller
// that could equally have written any allow-list it liked into the metadata. The
// check is therefore exactly as strong against the callers it is meant for, and
// no weaker against the ones it is not.
//
// IF A SERVICE EVER NEEDS TO ACT FOR A NAMED HUMAN, it must issue "user:{sub}"
// and carry that person's real allow-list. Self-stamping the portfolio it is
// about to trade would make this check pass by construction — "nothing
// configured" and "checked, and fine" looking the same, which is the one thing
// CLAUDE.md forbids outright.
//
// handleCancel AND handleAmend STAY ON THE STRICT RULE (plain entitledTo) and
// should: the api-gateway is the only producer of those two subjects, and
// kanz-redrive replays a parked one with its original metadata intact. A machine
// issuer on a cancel would be a command from nowhere against a live order.
func delegatedAndEntitled(md *commandpb.CommandMetadata, portfolio string) bool {
	if !strings.HasPrefix(md.GetIssuer(), delegatedIssuerPrefix) {
		return true
	}
	return entitledTo(md.GetPrincipalPortfolios(), portfolio)
}

// resume decides what to do with an order that already exists when a SubmitOrder
// for it arrives again — a broker redelivery, or the startup sweep replaying an
// order the previous process was working when it died.
//
// THIS REPLACES ACKING ON SIGHT. The handler used to load the record, take it
// for a duplicate, and return nil. That is correct for a genuinely duplicated
// command and catastrophic for a redelivery of a command whose work was
// interrupted: the order sits at ROUTED, nothing works it, nothing mentions it
// again, and over the API it is indistinguishable from a limit order resting
// normally at the exchange.
//
// The one thing it must never do is guess. Every path below either establishes
// what the venue did, or freezes the order.
//
// IT TAKES NO VERSION, DELIBERATELY. st is only read here — the terminal and
// quarantine checks — and every write below derives from the re-read under the
// claim. Accepting the caller's version would imply it governs those writes when
// it does not, and the first person to thread it through would be threading a
// value guaranteed to be stale.
func (s *Service) resume(ctx context.Context, st *orderpb.OrderState) error {
	// A terminal order is genuinely finished — UNLESS its terminal outcome was
	// persisted but never announced (see outcome_announced_at,
	// order_events.proto:19): a fill that made the order FILLED, or a permanent
	// reject, Saved and then interrupted before its FACT/outcome went out.
	// orderOutcomeAnnounced tells the two apart; only that check may return nil
	// here without doing anything.
	if IsTerminal(st) && orderOutcomeAnnounced(st) {
		return nil
	}
	// An already-quarantined order is frozen and stays frozen. Re-running the
	// policy on every redelivery would just re-derive the same freeze, and the
	// venue answer that resolves it is a human's to obtain.
	if !IsTerminal(st) && st.GetQuarantine() != nil {
		return nil
	}
	release, ok := s.claim(st.GetOrderId())
	if !ok {
		// Another goroutine in this process owns the order right now. It is being
		// worked; this delivery must not also work it.
		return nil
	}
	defer release()

	// Re-read under the claim: the holder we just raced may have finished and
	// changed the order between our Load and our claim.
	fresh, ver, err := s.store.Load(ctx, st.GetOrderId())
	if err != nil {
		return err
	}
	// The two branches that decide this delivery has nothing to say, decided
	// AGAIN under the claim because the goroutine we just raced for it may have
	// finished between our first read and this one. They come before the flush
	// below so a genuine duplicate — much the commonest redelivery — does not pay
	// for a queue read it has no use for.
	if IsTerminal(fresh) && orderOutcomeAnnounced(fresh) {
		return nil
	}
	if !IsTerminal(fresh) && fresh.GetQuarantine() != nil {
		return nil
	}

	// THE ORDER'S OWN BACKLOG GOES OUT BEFORE THIS DELIVERY PUBLISHES ANYTHING
	// (#292) — the same rule, in the same place, as handleCancel's and
	// handleAmend's flush after their claim.
	//
	// Every branch below either publishes directly (reannounceAccepted's
	// ORDER_ACCEPTED, work()'s ORDER_ROUTED, adopt()'s refuse(),
	// completeTerminalOutcome's CommandOutcome) or calls something that does. An
	// order reaching here can hold committed-but-unpublished FACTs: the delivery
	// that got it into this state is by definition one that failed partway, and
	// since the fill fold rides the outbox those records can now be ORDER_FILLED
	// FACTs. Publishing anything ahead of them hands a consumer this order's
	// history out of sequence, and that is the one failure that is not merely
	// late. It used to sit inside the PENDING_NEW branch only, covering the
	// ACCEPTED record; hoisting it here covers every branch with one call rather
	// than four.
	//
	// FLUSHED IS EVIDENCE, NOT A COUNTER. It is what tells the terminal branch
	// below whether the ORDER_FILLED FACT it cannot rebuild was in fact recovered
	// from the table.
	flushed, ferr := s.relay.Flush(ctx, fresh.GetOrderId())
	if ferr != nil {
		return ferr
	}

	// TERMINAL BUT UNANNOUNCED: the state was persisted before the interrupted
	// delivery could announce it. Complete the announcement rather than ack a
	// redelivery of an order the world was never told about.
	if IsTerminal(fresh) {
		return s.completeTerminalOutcome(ctx, fresh, ver, flushed)
	}
	st = fresh

	// PENDING_NEW: admitted but never routed. Nothing reached a venue, so there
	// is nothing to reconcile and nothing to be careful about — work it. This sits
	// UNDER the claim, not before it, because working an order is exactly the
	// thing two goroutines must not do at once.
	if st.GetStatus() == orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW {
		// FIRST, SAY THE ORDER EXISTS — before working it (#238).
		//
		// An unset marker here means admission committed the row and the
		// ORDER_ACCEPTED FACT never reached the bus, so no downstream service
		// knows this order. Working it now would emit ORDER_ROUTED for an order
		// tv-sync never admitted, and transition() DROPS that: the projection
		// would stay blind to a live order while the OMS believed it had
		// announced everything. Ordering matters for the same reason — accepted
		// before routed, so a consumer folds them in the sequence the lifecycle
		// actually happened in.
		//
		// This is the single repair point for BOTH recovery paths. A SubmitOrder
		// redelivered by the broker and one redriven off the DLQ by kanz-redrive
		// (#220) both land in handleSubmit, find the row already present, and
		// arrive HERE through resume(); so does the sweep. Putting the repair in
		// resume rather than in any one caller is what keeps them from drifting.
		if st.GetAcceptedAnnouncedAt() == nil {
			var rerr error
			if st, ver, rerr = s.reannounceAccepted(ctx, st, ver); rerr != nil {
				return rerr
			}
		}
		// An order admitted through the outbox needs no re-announcement — its
		// marker IS set, stamped in the admission transaction, so the branch
		// above correctly leaves it alone — but its ACCEPTED FACT may still be
		// queued, which is precisely the state an order is in when handleSubmit's
		// own flush failed and the command was redelivered. work() below
		// publishes ORDER_ROUTED directly, and the flush above has already sent
		// the ACCEPTED ahead of it.
		_, _, err := s.work(ctx, st, ver)
		return err
	}

	if s.router == nil {
		return nil // a paper deployment: the order rests, and nothing routed it
	}
	venue, err := s.router.Route(st)
	if err != nil {
		// Nothing here can execute this order. It is not resumable and it is not
		// safely abandonable either — freeze it and say so.
		return s.quarantine(ctx, st, ver, fmt.Sprintf(
			"order cannot be routed to any configured venue (%v), so nothing can be asked "+
				"what happened to it", err))
	}

	q, ok := venue.(execution.Querier)
	if !ok {
		return s.quarantine(ctx, st, ver, fmt.Sprintf(
			"venue %s implements no Querier, so nothing can establish whether it holds this "+
				"order. Re-driving might trade the fund twice and abandoning might strand a "+
				"live exchange order; neither is a guess this platform will make",
			venue.MIC()))
	}

	view, qerr := q.QueryOrder(ctx, st)
	if qerr != nil {
		// The QUESTION could not be asked — unreachable, rate-limited, timed out.
		// That is transient: nack and let the broker redeliver. It is emphatically
		// NOT an UNKNOWN answer, and collapsing the two would turn a network blip
		// into a re-driven order.
		return fmt.Errorf("oms: could not query venue %s about order %s: %w",
			venue.MIC(), st.GetOrderId(), qerr)
	}

	action, reason := Reconcile(st, view)
	s.logger.Info("oms reconciling an interrupted order",
		"order_id", st.GetOrderId(), "venue", venue.MIC(),
		"venue_view", view.State.String(), "action", action.String())

	switch action {
	case ActionLeave:
		// The venue holds a live order. Record the acknowledgement if we did not
		// already have it — the venue just proved it has the order.
		if st.GetVenueAckAt() == nil {
			acked := cloneState(st)
			acked.VenueAckAt = timestamppb.New(s.now().UTC())
			return s.store.Save(ctx, acked, ver, nil)
		}
		return nil
	case ActionRedrive:
		_, _, err := s.work(ctx, st, ver)
		return err
	case ActionAdopt:
		return s.adopt(ctx, st, ver, view)
	default:
		return s.quarantine(ctx, st, ver, reason)
	}
}

// orderOutcomeAnnounced reports whether a terminal order's outcome has already
// been told to the world, so resume() can tell a genuine duplicate apart from
// an interrupted announcement.
//
// CANCELLED is handled specially: its announcement is owned entirely by
// cancel_announced_at and the cancel command's OWN redelivery path
// (handleCancel/completeCancelAnnouncement), not by outcome_announced_at or by
// a SubmitOrder redelivery. A SubmitOrder redelivery that finds an order
// already cancelled has nothing of its own left to announce — the order was
// validly admitted (that already happened) and whatever became of it since is
// the cancel command's story to finish, not this one's. Treating it as
// unannounced here would have resume() invent a FILLED-or-REJECTED-shaped
// CommandOutcome for an order that is in fact cancelled.
func orderOutcomeAnnounced(st *orderpb.OrderState) bool {
	if st.GetStatus() == orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		return true
	}
	return st.GetOutcomeAnnouncedAt() != nil
}

// markAcceptedAnnounced stamps accepted_announced_at on an order whose
// ORDER_ACCEPTED FACT has just been published successfully, and returns the
// stamped state with the version the caller must use for its next write.
//
// It is the admission-time sibling of markOutcomeAnnounced. ONE function, called
// from both the live path and the compensator, so the two cannot drift into
// disagreeing about what "announced" means — which is the whole basis on which a
// compensator decides whether to republish.
func (s *Service) markAcceptedAnnounced(ctx context.Context, st *orderpb.OrderState, ver int64, t time.Time) (*orderpb.OrderState, int64, error) {
	announced := cloneState(st)
	announced.AcceptedAnnouncedAt = timestamppb.New(t.UTC())
	if err := s.store.Save(ctx, announced, ver, nil); err != nil {
		return st, ver, err
	}
	return announced, ver + 1, nil
}

// reannounceAccepted republishes the ORDER_ACCEPTED FACT for an order that was
// admitted and committed but whose acceptance the world was never told about,
// and records that it did.
//
// THIS IS THE REPAIR FOR THE ADMISSION-SIDE DIVERGENCE (#238), AND IT IS NOW A
// REPAIR FOR A CLOSED POPULATION (#292).
//
// It exists because store.Create and EmitAccepted USED TO BE two independent
// writes with no outbox between them: when the publish failed, Postgres held a
// durable PENDING_NEW order that no downstream service had heard of. Risk
// carried no exposure for it. tv-sync never admitted it, so it also DROPPED the
// ORDER_ROUTED that a later re-drive emitted — transition() ignores a FACT for
// an order the projection never saw — which is why re-driving the order alone
// does not repair the estate's view of it. The accepted FACT itself has to go
// out.
//
// Admission now enqueues that FACT in the same transaction as the row and
// stamps accepted_announced_at with it, so no order admitted by this code can
// reach here: the marker is set the instant the row exists. What CAN reach here
// is a row admitted by the previous code — marker unset, and no outbox record
// behind it, because there was no outbox. For those the direct publish below is
// the only thing that can announce them, which is why this stays.
//
// # THE RETIREMENT CONDITION, STATED SO IT IS NOT GUESSED AT LATER
//
// This function, the accepted_announced_at branch in resume() that calls it, and
// eventually field 20 of order/v1/order_events.proto can go when NO PENDING_NEW
// ROW PREDATING MIGRATION 0006 CAN STILL EXIST. Concretely:
//
//  1. Every OMS pod is running a build with the outbox (so nothing new is
//     admitted without a record), AND
//  2. `SELECT count(*) FROM orders WHERE status = 1 /* PENDING_NEW */` shows no
//     row whose state carries accepted_announced_at unset. A resting order in a
//     deployment with no venue wired stays PENDING_NEW forever, so this is a
//     query an operator has to run, not a duration anybody can wait out.
//
// The proto field goes LAST and separately: removing it is a wire change, and
// the Go code stopping reading it is not the same event as the field ceasing to
// exist. Delete the code first, ship it, then retire the field.
//
// Do not retire any of it on the grounds that the outbox "should" have made it
// unreachable. The outbox is new; this compensator has run in production. The
// order is: prove the first, then remove the second.
//
// PENDING_NEW ONLY, AND THE CALLER MUST ENFORCE IT. accepted_announced_at is an
// ADDITIVE field, so every order written before it existed reads back unset. For
// PENDING_NEW that is safe: the FACT carries the order's CURRENT state, whose
// status is PENDING_NEW, so a consumer that already saw the original folds an
// identical snapshot and a consumer that never saw it finally learns the order
// exists. For ROUTED or PARTIALLY_FILLED it is not: the same re-announcement
// would hand tv-sync a fresh revision at a pre-routing status and walk its view
// of a working order BACKWARDS. This is the same trap SweepInterrupted's status
// list documents for FILLED/REJECTED, in the same shape, and the answer is the
// same — do not select those states.
//
// The duplicate case is deliberately tolerated rather than eliminated: an order
// whose EmitAccepted succeeded and whose process then died before the marker
// Save is indistinguishable from one whose publish failed, and republishing an
// identical snapshot is the harmless half of that pair. tv-sync's upsertOrder
// appends a revision with the same state and the same status, so the current
// view it serves is unchanged.
func (s *Service) reannounceAccepted(ctx context.Context, st *orderpb.OrderState, ver int64) (*orderpb.OrderState, int64, error) {
	if err := s.emitter.EmitAccepted(ctx, st); err != nil {
		return st, ver, err
	}
	st, ver, err := s.markAcceptedAnnounced(ctx, st, ver, s.now().UTC())
	if err != nil {
		return st, ver, err
	}
	if s.acceptedReannounced != nil {
		s.acceptedReannounced.Inc()
	}
	// ERROR, not Info. The order is repaired, but the repair is evidence that a
	// FACT the estate depends on was lost, and for however long this order sat
	// unannounced every downstream calculation was short one order's worth of
	// risk. That is an incident with a resolved symptom, not a routine event.
	s.logger.Error("oms: re-announced an order whose ORDER_ACCEPTED FACT was committed to the store "+
		"but never published — until now no downstream service knew this order existed",
		"order_id", st.GetOrderId(), "portfolio_id", st.GetPortfolioId(),
		"admitted_at", st.GetAsOf().AsTime().UTC().Format(time.RFC3339))
	return st, ver, nil
}

// markOutcomeAnnounced stamps outcome_announced_at on a terminal order, so a
// later redelivery's resume() recognizes a genuine duplicate instead of
// re-entering the reject/fill path that already completed.
//
// IT TAKES THE RECORDS THE MARKER IS ABOUT (#292), AND THAT IS THE POINT — the
// same decision, for the same reason, as Store.Save carrying `announce` rather
// than there being a second announcing variant beside it. The marker and the
// announcement it asserts must land in ONE write or the marker is a claim about
// something that may not have happened.
//
// Its two callers state opposite answers, and the difference is exactly the
// difference between the live path and a compensator:
//
//   - handleSubmit's FILLED branch passes the outcome record. The marker then
//     means "the outcome is committed for delivery" — a promise the relay keeps
//     — rather than "a publish call returned nil a moment ago".
//   - completeTerminalOutcome passes nil, because it has ALREADY published
//     directly. It is repairing a row with no outbox record behind it, so there
//     is nothing to enqueue; handing it a record would mean writing one for the
//     case defined by not having one.
//
// nil is therefore a legitimate answer and not a default, which is why the
// parameter is a slice rather than variadic: omitting it does not compile.
func (s *Service) markOutcomeAnnounced(ctx context.Context, st *orderpb.OrderState, ver int64, t time.Time, announce []outbox.Record) error {
	announced := cloneState(st)
	announced.OutcomeAnnouncedAt = timestamppb.New(t)
	return s.store.Save(ctx, announced, ver, announce)
}

// completeTerminalOutcome re-publishes the CommandOutcome for a SubmitOrder
// whose terminal state (FILLED or REJECTED) was persisted but never
// announced — a delivery that Saved the terminal OrderState and then failed
// before its fill/reject FACT or the trailing EmitOutcome went out (see
// outcome_announced_at, order_events.proto:19). It is resume()'s completion
// of that interrupted announcement.
//
// WHAT IT CAN NOW DO THAT IT COULD NOT (#292). The fill fold commits its
// ORDER_FILLED FACT to the outbox in the same transaction as the state, so for
// an order filled by an outbox build the FACT is not lost — it is in the table,
// and resume() flushes that table before calling this. announced is how many
// records that flush published: a non-zero value means the real FACT, with the
// real fill_id and price, has just gone out, and this function is completing an
// announcement rather than apologising for one. That is the difference between
// "recovered" and "gone", and it is why the log below branches on it instead of
// reporting a lost fill on every successful recovery.
//
// WHAT IT STILL CANNOT DO, AND WHY. The stored OrderState carries only the order's
// current AGGREGATE view — status, cumulative filled_quantity, leaves_quantity,
// average_fill_price (order/v1/order_events.proto:78-85). It has no field for
// the individual Fill that produced a FILLED state (fill_id, price,
// venue_execution_id, executed_at — order/v1/order_events.proto:169-211), nor
// for an OrderRejected's reason/error_code (order/v1/order_events.proto:222-230).
// The reject reason lives only in the literal "PRICE_UNAVAILABLE"/err.Error() in
// handleSubmit's ErrUnpriced branch, and a fill folded BEFORE migration 0006
// lived only in work()'s loop variable — neither was persisted anywhere this
// function, reading only store.Load's result, can recover it from. Re-emitting
// those exact FACTs is therefore not possible here without fabricating a fill or
// a rejection reason, which is worse than the FACT arriving late: it would put
// invented data on the record.
//
// What CAN be reconstructed, honestly, from the terminal OrderState alone is
// the CommandOutcome for the original SubmitOrder command — its status
// follows directly from st.status, with no other input needed. That is what
// this publishes. Where announced is zero the missing lifecycle FACT is a
// residual, KNOWN LIMIT: tv-sync, accounting, and audit still never see it.
// What this removes is the worse failure — a completed trade or a permanent
// rejection whose command outcome the caller was never told, with no recovery
// path at all.
func (s *Service) completeTerminalOutcome(ctx context.Context, st *orderpb.OrderState, ver int64, announced int) error {
	now := s.now().UTC()
	var status commandpb.CommandOutcomeStatus
	var reason, code string
	switch st.GetStatus() {
	case orderpb.OrderStatus_ORDER_STATUS_FILLED:
		status = commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED
		reason = "order filled (outcome re-announced after an interrupted delivery)"
		if announced > 0 {
			// THE FILL WAS RECOVERED, NOT LOST. resume()'s flush just published
			// the committed ORDER_FILLED FACT — the real fill_id, the real price
			// — so this is a completed announcement, not a hole in the record.
			// Still logged, because the interruption itself is worth seeing.
			s.logger.Warn("oms: completing an interrupted FILLED announcement — the ORDER_FILLED FACT was "+
				"committed to the outbox with the fill and has just been published from it, so nothing "+
				"was lost; only the trailing command outcome was outstanding (#292)",
				"order_id", st.GetOrderId(), "facts_recovered", announced)
			break
		}
		s.logger.Error("oms: completing an interrupted FILLED announcement WITHOUT its ORDER_FILLED FACT — "+
			"nothing was queued for this order, so the fill predates the outbox (migration 0006) and is "+
			"not recoverable from the stored OrderState; only the command outcome is re-published, and "+
			"tv-sync, accounting, and audit will never see this fill's FACT",
			"order_id", st.GetOrderId())
	default:
		// REJECTED today; EXPIRED is not currently reachable (Expire is never
		// called from this service), but the same reasoning applies if it ever is.
		status = commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED
		reason = "order rejected (outcome re-announced after an interrupted delivery)"
		code = "OUTCOME_RECOVERED"
		s.logger.Error("oms: completing an interrupted REJECTED announcement WITHOUT its ORDER_REJECTED FACT — "+
			"the original reject reason/code is not recoverable from the stored OrderState, so only a "+
			"generic command outcome is re-published",
			"order_id", st.GetOrderId(), "status", st.GetStatus().String())
	}
	if err := s.emitter.EmitOutcome(ctx, st.GetOrderId(), status, reason, code, "", now); err != nil {
		return err
	}
	// nil: this outcome has already gone out on the wire above. See
	// markOutcomeAnnounced — a compensator has nothing to enqueue.
	return s.markOutcomeAnnounced(ctx, st, ver, now, nil)
}

// adopt takes the venue's truth as ours: its fills, or its rejection.
//
// Folding a fill twice is prevented downstream, not here: the position book
// claims each fill_id in position_fills before folding it, so a fill this
// adoption re-emits after a crash is counted exactly once. That is why the
// venue must report its ORIGINAL fill ids on a query — a renamed fill defeats
// the claim and double-counts the book.
func (s *Service) adopt(ctx context.Context, st *orderpb.OrderState, ver int64, view execution.OrderView) error {
	if view.State == execution.OrderViewRejected {
		now := s.now().UTC()
		reason := view.Reason
		if reason == "" {
			reason = "venue reports this order rejected"
		}
		rejected := Reject(st, now)
		// THE VENUE'S REJECTION AND ITS ANNOUNCEMENT ARE NOW ONE WRITE (#292),
		// AND THIS PATH NEEDED IT MORE THAN THE ONE IT MIRRORS.
		//
		// It is the same shape as handleSubmit's ErrUnpriced reject —
		// Save(REJECTED), refuse()'s two publishes, then a third Save stamping
		// outcome_announced_at — and it carries the same unrecoverable half:
		// completeTerminalOutcome reconstructs the CommandOutcome from stored
		// state and says in its own comment that it cannot reconstruct
		// ORDER_REJECTED, because the reason lives only in the venue's answer and
		// has no OrderState counterpart. A crash between the Save and the publish
		// left the order durably REJECTED with the FACT gone and nothing able to
		// notice.
		//
		// AND THIS IS THE RECOVERY PATH, which is why it matters more rather than
		// less: the process reaching here is one that already crashed once, and
		// this is the venue's own account of what became of the order — the answer
		// the reconciliation exists to obtain. Losing it means asking the venue
		// again, and a venue that has since forgotten the order answers UNKNOWN,
		// which quarantines it.
		//
		// Both records are built BEFORE the Save, so a record that could never be
		// published stops the adoption instead of committing half of it.
		rejFact, ferr := s.emitter.RejectedFact(ctx, st.GetOrderId(), "VENUE_REJECTED", reason, now)
		if ferr != nil {
			return ferr
		}
		outFact, ferr := s.emitter.OutcomeFact(ctx, st.GetOrderId(),
			commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED, reason, "VENUE_REJECTED", "", now)
		if ferr != nil {
			return ferr
		}
		// Same marker, same reason as the ErrUnpriced reject in handleSubmit (see
		// order_events.proto:19) — and, like it, stamped in the SAME write rather
		// than two writes later, so it is true the instant it is durable.
		rejected.OutcomeAnnouncedAt = timestamppb.New(now)
		if err := s.store.Save(ctx, rejected, ver, []outbox.Record{rejFact, outFact}); err != nil {
			return err
		}
		// Flushed where refuse() stood, so the FACTs leave in the same order and at
		// the same point they did before — and so resume()'s caller sees the
		// rejection now rather than on the relay's next tick.
		_, ferr = s.relay.Flush(ctx, st.GetOrderId())
		return ferr
	}

	// The venue confirmed it holds the order, so the ack is established even if
	// we never recorded one.
	if st.GetVenueAckAt() == nil {
		acked := cloneState(st)
		acked.VenueAckAt = timestamppb.New(s.now().UTC())
		if err := s.store.Save(ctx, acked, ver, nil); err != nil {
			return err
		}
		ver++
		st = acked
	}

	// REFUSE A MULTI-FILL VIEW. ApplyFill has no fill_id dedup — that is left to
	// the position book, which claims each fill_id in position_fills before
	// folding it into the POSITION. The ORDER AGGREGATE folded here has no such
	// guard, and cancel/amend and the read API consult this aggregate directly.
	//
	// The failure this prevents: work() folds fill 1 and Saves it, then fails
	// before fill 2's Save. The redelivery reaches here, and the venue
	// reports BOTH fills, and folding fill 1 again would refold a fill this order
	// already contains. If the refold overfills, ApplyFill errors and the order
	// quarantines below anyway — safe, but a reconcilable order sits frozen for no
	// reason. If it fits within the remaining leaves, filled_quantity and
	// average_fill_price are silently double-counted and persisted, and nothing
	// downstream catches it.
	//
	// This is not reachable today — SimVenue is the only Querier and it always
	// returns exactly one full-leaves fill — but it goes live the moment a
	// multi-fill venue implements Querier. Fail closed rather than guess: refuse
	// to adopt more than one fill until ApplyFill (or this fold) gains its own
	// fill_id dedup. Single-fill adoption, the only case any wired venue can
	// currently produce, keeps working unchanged.
	if len(view.Fills) > 1 {
		return s.quarantine(ctx, st, ver, fmt.Sprintf(
			"venue reports %d fills for this order, and adopting more than one fill is not "+
				"yet safe: ApplyFill has no fill_id dedup, so folding a fill this order already "+
				"contains would silently double-count filled_quantity and average_fill_price",
			len(view.Fills)))
	}

	// REFUSE A FILLED/PARTIALLY_FILLED VIEW THAT CARRIES ZERO FILLS. This is the
	// other direction of the guard above, and it is the more dangerous of the
	// two because it fails OPEN if left unchecked: view.State says the venue
	// traded this order, but view.Fills is empty, so the for loop below simply
	// never executes. Nothing errors. venue_ack_at gets stamped (harmless), the
	// order is returned nil (success), the order stays at ROUTED, and no fill,
	// no position, and no ledger entry is ever created for a trade the venue
	// itself just reported. The next startup sweep re-asks this same venue, gets
	// this same answer, and does this same nothing — a live position the
	// platform has permanently forgotten. Quarantine instead: adopting a claim
	// of "filled" with nothing to fold would leave a traded order looking
	// untraded, which is exactly the failure this whole path exists to prevent.
	if (view.State == execution.OrderViewFilled || view.State == execution.OrderViewPartiallyFilled) && len(view.Fills) == 0 {
		return s.quarantine(ctx, st, ver, fmt.Sprintf(
			"venue reports this order %s but supplied no fill to fold, so the platform cannot "+
				"record what traded. Adopting the claim without the fill would leave a traded "+
				"order looking untraded", view.State.String()))
	}

	for _, fill := range view.Fills {
		next, aerr := ApplyFill(st, fill, fill.GetExecutedAt().AsTime())
		if aerr != nil {
			// The venue's own fills do not fit the order we hold. That is not a
			// transient fault and re-driving cannot help; it is a disagreement
			// about what this order IS, and it freezes.
			return s.quarantine(ctx, st, ver, fmt.Sprintf(
				"venue reported a fill this order cannot accept (%v). The venue's record and "+
					"ours describe different orders under one id", aerr))
		}
		// THE ADOPTED FILL RIDES ITS OWN TRANSACTION TOO (#292), for exactly the
		// reasons work()'s does — and this path matters more, not less, because it
		// is the recovery path: the process that reaches here is the one that
		// already crashed once. An adopted fill whose publish failed would leave a
		// FILLED order whose ORDER_FILLED FACT nothing can reconstruct, and the
		// next redelivery would find it terminal and complete only the outcome.
		fact, ferr := s.emitter.FillFact(ctx, fill, next)
		if ferr != nil {
			return ferr
		}
		if err := s.store.Save(ctx, next, ver, []outbox.Record{fact}); err != nil {
			return err
		}
		ver++
		if _, err := s.relay.Flush(ctx, next.GetOrderId()); err != nil {
			return err
		}
		st = next
		if IsTerminal(st) {
			break
		}
	}
	return nil
}

// quarantine freezes an order whose truth could not be established, and says so
// where somebody will see it: on the order, in the log at ERROR, and on a
// counter that can be alerted.
//
// It returns nil — the command is ACKED. A quarantine is terminal for this
// delivery: redelivering it would re-derive the same freeze forever, and the
// thing that resolves it is a human with venue access, not another attempt.
// ver is the version this caller loaded st at. A quarantine is itself a write,
// so it can lose the same race it is often reporting; see the ErrConflict branch
// on the Save below for why that is left to fail loudly rather than forced.
func (s *Service) quarantine(ctx context.Context, st *orderpb.OrderState, ver int64, reason string) error {
	now := s.now().UTC()
	next := cloneState(st)
	next.Quarantine = &orderpb.OrderQuarantine{
		At:          timestamppb.New(now),
		Reason:      reason,
		LastQueryAt: timestamppb.New(now),
	}

	// LOG AND COUNT BEFORE THE SAVE, NOT AFTER. This used to Save first and log
	// only on success — so a Save failure (a store outage, a full disk) returned
	// early and the freeze was never counted and never logged. The command still
	// nacks and gets redelivered, but until it succeeds, an order this platform
	// has decided it cannot safely re-drive keeps being re-driven, silently,
	// because the one signal that should have stopped it never fired. Logging
	// first means the operator learns an attempt was made either way; the
	// wording says ATTEMPTING, not DONE, because persistence is not yet known.
	if s.quarantined != nil {
		s.quarantined.Inc()
	}
	s.logger.Error("ORDER QUARANTINE ATTEMPTED — the platform cannot establish what the venue did with this order, so it is stopping rather than guess. Once persisted it will not be re-driven, cancelled, or mentioned again until a human resolves it against the venue's own order history",
		"order_id", st.GetOrderId(),
		"portfolio_id", st.GetPortfolioId(),
		"instrument_id", st.GetInstrumentId(),
		"venue", st.GetVenue(),
		"status", st.GetStatus().String(),
		"reason", reason,
	)
	// A CONFLICT ON THE QUARANTINE WRITE IS NOT SWALLOWED. It means the order
	// moved again between the decision to freeze and the freeze itself, so this
	// state is stale and forcing it would overwrite whatever the winner recorded
	// — the very defect quarantine exists to report. Returning the error nacks
	// the command, and the redelivery re-derives the freeze against fresh state.
	if err := s.store.Save(ctx, next, ver, nil); err != nil {
		return err
	}
	return nil
}

// quarantineAge is how long an order has been frozen, rendered for the refusal an
// operator actually reads.
//
// OrderQuarantine.at is documented Required and — like its sibling last_query_at,
// whose own comment says it exists "so an operator can tell a fresh contradiction
// from a stale one" — nothing read it back. So a cancel refused on an order
// frozen thirty seconds ago and one frozen three weeks ago produced the SAME
// message.
//
// The difference is the whole triage. A freeze minutes old is probably the
// incident in progress; one weeks old is a position whose true size nobody has
// established since, sitting in a book that has been traded around it. The
// refusal already carries the reason precisely "so the operator does not need a
// second lookup" — the age is the other half of that sentence.
//
// An UNSET at yields "unknown", never a duration from the epoch. A quarantine
// whose timestamp the producer never set would otherwise report itself as
// fifty-six years old, which is worse than saying nothing: it is a number, and
// numbers get believed.
func quarantineAge(q *orderpb.OrderQuarantine, now time.Time) string {
	if q.GetAt() == nil {
		return "unknown"
	}
	at := q.GetAt().AsTime().UTC()
	if at.IsZero() {
		return "unknown"
	}
	d := now.UTC().Sub(at)
	if d < 0 {
		// A freeze stamped in the future — a clock skew between pods. Saying so is
		// better than rendering a negative age an operator has to interpret.
		return "unknown (stamped in the future)"
	}
	return d.Round(time.Second).String()
}
