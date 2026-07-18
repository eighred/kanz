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
	sharedCount    prometheus.Counter
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

	// Fast path for an obvious re-submit: skip the compliance gate and validation
	// for an order we already know. This is an OPTIMIZATION ONLY — it is a
	// check-then-act and cannot be the guard. Admission is enforced atomically by
	// store.Create below, which is what actually stands between a duplicated
	// SubmitOrder and a duplicated venue order.
	if _, err := s.store.Load(ctx, cmd.GetOrderId()); err == nil {
		return nil
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
			return nil // lost the admission race — the winner works the order
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
