package order

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	commandpb "github.com/kanz-eng/kanz-schemas-go/command/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/internal/execution"
	"github.com/kanz-eng/kanz/services/oms/internal/compliance"

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
	accounts       *execution.AccountBindings
	requireAccount bool
	sharedOnce     sync.Map // "tenant/portfolio@MIC" → struct{}, so the warning is said once
	// working claims an order_id for the goroutine currently driving it, so a
	// resume can never run alongside the delivery that already owns the order.
	// See claim().
	working     sync.Map // order_id → struct{}
	sharedCount prometheus.Counter
	// quarantined counts orders frozen because venue truth could not be
	// established. It is the alertable signal: a quarantine is a position whose
	// true size nobody knows, and it must not be discoverable only by reading logs.
	quarantined prometheus.Counter
}

// ServiceOption customizes the handler.
type ServiceOption func(*Service)

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

// NewService wires the handler. gate defaults to deny-nothing (compliance.AllowAll)
// when nil; router may be nil to admit orders without working them (they rest).
// closes is the in-flight-close registry the venue-close dispatch path writes to
// and the reconcilers' healing watchdogs drain; nil disables venue-side cancel
// dispatch (the cancel stays ledger-only).
func NewService(store Store, emitter *Emitter, gate compliance.Gate, router *execution.Router, closes execution.CloseTracker, logger *slog.Logger, opts ...ServiceOption) (*Service, error) {
	if store == nil || emitter == nil {
		return nil, errors.New("oms: store and emitter required")
	}
	if gate == nil {
		gate = compliance.AllowAll{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	svc := &Service{
		store: store, gate: gate, emitter: emitter, router: router, closes: closes,
		now: time.Now, logger: logger,
	}
	for _, opt := range opts {
		opt(svc)
	}
	return svc, nil
}

// Handle is the bus.EventHandler. It dispatches by the command subject/type.
func (s *Service) Handle(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	switch env.GetEventType() {
	case SubjectSubmit:
		return s.handleSubmit(ctx, env, payload)
	case SubjectCancel:
		return s.handleCancel(ctx, payload)
	case SubjectAmend:
		return s.handleAmend(ctx, payload)
	default:
		// Subscription scope matched but the type is unknown — surface so the
		// operator sees the misconfiguration (same stance as ingest).
		return errors.New("oms: unknown command type: " + env.GetEventType())
	}
}

func (s *Service) handleSubmit(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
	var cmd orderpb.SubmitOrder
	if err := proto.Unmarshal(payload, &cmd); err != nil {
		// A malformed command body is a permanent defect; reject-and-ack rather
		// than redeliver forever. order_id is unknown, so nothing to correlate.
		s.logger.Error("oms: malformed SubmitOrder", "err", err)
		return nil
	}
	now := s.now().UTC()

	// AN ORDER WE ALREADY KNOW IS NOT AUTOMATICALLY A DUPLICATE.
	//
	// This used to return nil on sight, which is right for a genuinely duplicated
	// command and catastrophic for a redelivery of one whose work was interrupted:
	// delivery 1 created the order and died at the venue, delivery 2 acked it, and
	// the order sat at ROUTED forever with nothing working it. resume() establishes
	// what the venue actually did and acts on that — or freezes the order when it
	// cannot. Admission itself is still enforced atomically by store.Create below.
	if existing, err := s.store.Load(ctx, cmd.GetOrderId()); err == nil {
		return s.resume(ctx, existing)
	} else if !errors.Is(err, ErrNotFound) {
		return err // transient store failure
	}

	// Pre-trade compliance gate (OMS-01f).
	breach, err := s.gate.Check(ctx, &cmd)
	if err != nil {
		return err // transient gate failure ⇒ retry
	}
	if breach != nil {
		return s.refuse(ctx, cmd.GetOrderId(), "COMPLIANCE_"+breach.Code, breach.Reason, now)
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

	// WHOSE COLLATERAL DOES THIS ORDER SPEND? Resolve the exchange account before the
	// order exists, because it is not a routing detail — it is the answer to that
	// question, and an order admitted without one is an order that will margin against
	// whichever account its adapter happens to hold.
	account, rej := s.resolveAccount(env.GetTenantId(), cmd.GetPortfolioId(), cmd.GetVenue())
	if rej != nil {
		return s.refuse(ctx, cmd.GetOrderId(), rej.Code, rej.Msg, now)
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
	// The account is stamped by the OMS, never by the caller. A caller that could name
	// the account could name ANY portfolio's account — it would be choosing whose
	// collateral to spend — which is why SubmitOrder has no such field to set.
	st.VenueAccountId = account

	// THE ADMISSION GATE. Create is atomic: exactly one concurrent delivery of this
	// order_id can insert it, and every other gets ErrExists. Losing the race means
	// another delivery already owns this order and is working it — so this one acks
	// and stops, HERE, before s.work() below routes it to a venue. The old
	// Load()-then-Save() let both deliveries through this point and both reached the
	// venue: the state converged (Save upserts) while the fund traded twice.
	if err := s.store.Create(ctx, st); err != nil {
		if errors.Is(err, ErrExists) {
			// Lost the admission race — the winner works the order. Deliberately NOT
			// a resume: the winner is mid-flight by construction, and the interrupted
			// case is reached through the Load fast path above (on redelivery) or the
			// startup sweep (after a crash), both of which take the per-order claim.
			return nil
		}
		return err
	}
	if err := s.emitter.EmitAccepted(ctx, st); err != nil {
		return err
	}

	// Work the order if a router is wired; otherwise it rests (ACCEPTED).
	st, err = s.work(ctx, st)
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
		if serr := s.store.Save(ctx, Reject(st, rejectedAt)); serr != nil {
			return serr
		}
		return s.refuse(ctx, st.GetOrderId(), "PRICE_UNAVAILABLE", err.Error(), rejectedAt)
	}
	if err != nil {
		return err
	}

	status := commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_ACCEPTED
	reason := "order admitted"
	if st.GetStatus() == orderpb.OrderStatus_ORDER_STATUS_FILLED {
		status = commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED
		reason = "order filled"
	}
	return s.emitter.EmitOutcome(ctx, st.GetOrderId(), status, reason, "", "", now)
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
func (s *Service) work(ctx context.Context, st *orderpb.OrderState) (*orderpb.OrderState, error) {
	if s.router == nil {
		return st, nil
	}
	venue, err := s.router.Route(st)
	if errors.Is(err, execution.ErrNoVenue) {
		return st, nil // nothing wired at all ⇒ rest (a paper deployment)
	}
	// A named-but-unconfigured venue is refused at admission and cannot reach here.
	// If it ever does, it is an error — never a silent rest. An order nobody can
	// execute must not look like an order that is working.
	if err != nil {
		return st, err
	}
	routed := Route(st, s.now().UTC())
	if err := s.store.Save(ctx, routed); err != nil {
		return st, err
	}
	if err := s.emitter.EmitRouted(ctx, routed.GetOrderId(), venue.MIC(), "", routed.GetAsOf().AsTime()); err != nil {
		return st, err
	}
	st = routed

	fills, err := venue.Execute(ctx, st)
	if err != nil {
		return st, err
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
	if err := s.store.Save(ctx, acked); err != nil {
		return st, err
	}
	st = acked

	for _, fill := range fills {
		next, aerr := ApplyFill(st, fill, fill.GetExecutedAt().AsTime())
		if aerr != nil {
			// An over-fill from the venue is a bug, not a transient fault; log
			// and stop working this order rather than loop.
			s.logger.Error("oms: venue fill rejected by aggregate", "order_id", st.GetOrderId(), "err", aerr)
			break
		}
		if err := s.store.Save(ctx, next); err != nil {
			return st, err
		}
		if err := s.emitter.EmitFill(ctx, fill, next); err != nil {
			return st, err
		}
		st = next
		if IsTerminal(st) {
			break
		}
	}
	return st, nil
}

func (s *Service) handleCancel(ctx context.Context, payload []byte) error {
	var cmd orderpb.CancelOrder
	if err := proto.Unmarshal(payload, &cmd); err != nil {
		s.logger.Error("oms: malformed CancelOrder", "err", err)
		return nil
	}
	now := s.now().UTC()
	st, err := s.store.Load(ctx, cmd.GetOrderId())
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
	// Withdraw the order AT the venue before recording the cancellation. Without
	// this the ledger calls the order CANCELLED while it is still resting — and
	// still fillable — on the exchange. st (not next) carries the pre-cancel
	// quantities the close intent is built from.
	s.closeAtVenue(ctx, st, now)

	if err := s.store.Save(ctx, next); err != nil {
		return err
	}
	if err := s.emitter.EmitCancelled(ctx, next.GetOrderId(), cancelledQty, now); err != nil {
		return err
	}
	return s.emitter.EmitOutcome(ctx, next.GetOrderId(),
		commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED, "order cancelled", "", "", now)
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
	now := s.now().UTC()
	st, err := s.store.Load(ctx, cmd.GetOrderId())
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
	next, aerr := Amend(st, &cmd, now)
	if aerr != nil {
		var re *RejectError
		if errors.As(aerr, &re) {
			return s.outcomeReject(ctx, cmd.GetOrderId(), re.Code, re.Msg, now)
		}
		return aerr
	}
	if err := s.store.Save(ctx, next); err != nil {
		return err
	}
	return s.emitter.EmitOutcome(ctx, next.GetOrderId(),
		commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED, "order amended", "", "", now)
}

// refuse emits an ORDER_REJECTED FACT and a REJECTED command outcome, then acks
// (returns nil) — a refused command is terminal.
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
// EMPTY DENIES. This is deliberately the opposite of pkg/auth's PolicyAuthorizer,
// which treats an absent portfolio claim as unrestricted — a sane default for a
// read path and the wrong one here, where a dropped claim would silently
// authorize a caller against every portfolio in the tenant. On the capital path
// the absence of proof is not proof; it is the absence of entitlement.
func entitledTo(allowed []string, portfolio string) bool {
	if portfolio == "" {
		return false // an order that cannot say whose it is cannot be authorized
	}
	for _, p := range allowed {
		if p == portfolio {
			return true
		}
	}
	return false
}

// claim takes exclusive, in-process ownership of one order_id and returns the
// function that releases it. ok is false when another goroutine in THIS process
// already holds it, and the caller must then do nothing at all.
//
// WHAT THIS IS FOR, AND WHAT IT IS NOT. store.Create is the ADMISSION gate: it
// decides which of two concurrent first-deliveries owns a new order. It has
// nothing to say about two deliveries that both find an order that ALREADY
// exists — which, once the handler resumes interrupted orders instead of acking
// them, is two goroutines both re-driving one order to a venue. The bus
// partition key makes that rare; rare is not a posture on the capital path.
//
// It is per-order, not global: a single lock would serialize every order in the
// OMS behind the slowest venue call.
//
// It is IN-PROCESS ONLY, and that is a real limit, not an oversight. Two OMS
// pods resuming the same order are not excluded by this and cannot be — that
// requires a lease in the store. It is the same single-replica assumption the
// order and position stores already carry (see the openStores comment in
// cmd/oms/main.go); this narrows the window that exists WITHIN a pod, which is
// the window a redelivery actually opens.
func (s *Service) claim(orderID string) (func(), bool) {
	if _, loaded := s.working.LoadOrStore(orderID, struct{}{}); loaded {
		return func() {}, false
	}
	return func() { s.working.Delete(orderID) }, true
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
func (s *Service) resume(ctx context.Context, st *orderpb.OrderState) error {
	// A terminal order is genuinely finished — this really is a duplicate.
	if IsTerminal(st) {
		return nil
	}
	// An already-quarantined order is frozen and stays frozen. Re-running the
	// policy on every redelivery would just re-derive the same freeze, and the
	// venue answer that resolves it is a human's to obtain.
	if st.GetQuarantine() != nil {
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
	fresh, err := s.store.Load(ctx, st.GetOrderId())
	if err != nil {
		return err
	}
	if IsTerminal(fresh) || fresh.GetQuarantine() != nil {
		return nil
	}
	st = fresh

	// PENDING_NEW: admitted but never routed. Nothing reached a venue, so there
	// is nothing to reconcile and nothing to be careful about — work it. This sits
	// UNDER the claim, not before it, because working an order is exactly the
	// thing two goroutines must not do at once.
	if st.GetStatus() == orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW {
		_, err := s.work(ctx, st)
		return err
	}

	if s.router == nil {
		return nil // a paper deployment: the order rests, and nothing routed it
	}
	venue, err := s.router.Route(st)
	if err != nil {
		// Nothing here can execute this order. It is not resumable and it is not
		// safely abandonable either — freeze it and say so.
		return s.quarantine(ctx, st, fmt.Sprintf(
			"order cannot be routed to any configured venue (%v), so nothing can be asked "+
				"what happened to it", err))
	}

	q, ok := venue.(execution.Querier)
	if !ok {
		return s.quarantine(ctx, st, fmt.Sprintf(
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
			return s.store.Save(ctx, acked)
		}
		return nil
	case ActionRedrive:
		_, err := s.work(ctx, st)
		return err
	case ActionAdopt:
		return s.adopt(ctx, st, view)
	default:
		return s.quarantine(ctx, st, reason)
	}
}

// adopt takes the venue's truth as ours: its fills, or its rejection.
//
// Folding a fill twice is prevented downstream, not here: the position book
// claims each fill_id in position_fills before folding it, so a fill this
// adoption re-emits after a crash is counted exactly once. That is why the
// venue must report its ORIGINAL fill ids on a query — a renamed fill defeats
// the claim and double-counts the book.
func (s *Service) adopt(ctx context.Context, st *orderpb.OrderState, view execution.OrderView) error {
	if view.State == execution.OrderViewRejected {
		now := s.now().UTC()
		if err := s.store.Save(ctx, Reject(st, now)); err != nil {
			return err
		}
		reason := view.Reason
		if reason == "" {
			reason = "venue reports this order rejected"
		}
		return s.refuse(ctx, st.GetOrderId(), "VENUE_REJECTED", reason, now)
	}

	// The venue confirmed it holds the order, so the ack is established even if
	// we never recorded one.
	if st.GetVenueAckAt() == nil {
		acked := cloneState(st)
		acked.VenueAckAt = timestamppb.New(s.now().UTC())
		if err := s.store.Save(ctx, acked); err != nil {
			return err
		}
		st = acked
	}

	for _, fill := range view.Fills {
		next, aerr := ApplyFill(st, fill, fill.GetExecutedAt().AsTime())
		if aerr != nil {
			// The venue's own fills do not fit the order we hold. That is not a
			// transient fault and re-driving cannot help; it is a disagreement
			// about what this order IS, and it freezes.
			return s.quarantine(ctx, st, fmt.Sprintf(
				"venue reported a fill this order cannot accept (%v). The venue's record and "+
					"ours describe different orders under one id", aerr))
		}
		if err := s.store.Save(ctx, next); err != nil {
			return err
		}
		if err := s.emitter.EmitFill(ctx, fill, next); err != nil {
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
func (s *Service) quarantine(ctx context.Context, st *orderpb.OrderState, reason string) error {
	now := s.now().UTC()
	next := cloneState(st)
	next.Quarantine = &orderpb.OrderQuarantine{
		At:          timestamppb.New(now),
		Reason:      reason,
		LastQueryAt: timestamppb.New(now),
	}
	if err := s.store.Save(ctx, next); err != nil {
		return err
	}
	if s.quarantined != nil {
		s.quarantined.Inc()
	}
	s.logger.Error("ORDER QUARANTINED — the platform cannot establish what the venue did with this order, so it has stopped rather than guess. It will not be re-driven, cancelled, or mentioned again until a human resolves it against the venue's own order history",
		"order_id", st.GetOrderId(),
		"portfolio_id", st.GetPortfolioId(),
		"instrument_id", st.GetInstrumentId(),
		"venue", st.GetVenue(),
		"status", st.GetStatus().String(),
		"reason", reason,
	)
	return nil
}
