package order

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	commandpb "github.com/kanz-eng/kanz-schemas-go/command/v1"
	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
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

// WithClaimWait bounds how long a cancel or an amend waits for the goroutine
// currently working an order before giving up and nacking. It exists to be
// LOWERED in tests; raising it in production trades a longer stall of the whole
// cancel subject for a slightly better chance of catching a slow venue, and past
// JetStream's AckWait it starts deleting cancels outright — see
// defaultClaimWait, which explains why.
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
		now: time.Now, logger: logger, claimWait: defaultClaimWait,
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
		// its ACCEPTED fact is already emitted, so there is nothing left for this
		// delivery to do — the claim holder will carry it to completion. This is
		// the same reasoning as the ErrExists branch above: the order exists and
		// something in this process already owns it.
		return nil
	}
	defer release()

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
		rejected := Reject(st, rejectedAt)
		if serr := s.store.Save(ctx, rejected); serr != nil {
			return serr
		}
		if rerr := s.refuse(ctx, st.GetOrderId(), "PRICE_UNAVAILABLE", err.Error(), rejectedAt); rerr != nil {
			return rerr
		}
		// The FACT and the outcome are both out — stamp outcome_announced_at so
		// a redelivery of this SubmitOrder hits resume()'s genuine-duplicate
		// branch instead of re-entering this path. See order_events.proto:19.
		return s.markOutcomeAnnounced(ctx, rejected, rejectedAt)
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
	if err := s.emitter.EmitOutcome(ctx, st.GetOrderId(), status, reason, "", "", now); err != nil {
		return err
	}
	if st.GetStatus() == orderpb.OrderStatus_ORDER_STATUS_FILLED {
		// The fill's FACT (inside s.work's loop, above) and this trailing
		// outcome are both out — stamp outcome_announced_at so a redelivery
		// of this SubmitOrder hits resume()'s genuine-duplicate branch
		// instead of re-driving or re-announcing a finished order.
		return s.markOutcomeAnnounced(ctx, st, now)
	}
	return nil
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

	// THE FOLD BELOW READS st, NOT THE STORE, AND THAT IS ONLY SAFE BECAUSE OF
	// THE PER-ORDER LOCK. Every caller of work() holds it (handleSubmit, resume),
	// and cancel and amend now WAIT for it (awaitClaim), so within this process
	// nothing can move this order between the read above and the Saves below.
	// ApplyFill's IsTerminal guard is therefore evaluated against state that is
	// still current — which is precisely what it was NOT before the per-order
	// lock reached cancel, and why a cancel landing inside venue.Execute could be
	// overwritten by a FILLED Save it never saw.
	//
	// ACROSS REPLICAS IT IS STILL NOT SAFE, and re-reading here would not make it
	// so: it would shrink the window between Load and Save, not remove it. A
	// probabilistic fix on the capital path is how the partition_key
	// serialization claim got written in the first place. That gap belongs to
	// Store.Save gaining a version predicate — see postgres.go.
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
				"order history before it can be cancelled. quarantine reason: %s", q.GetReason()), now)
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
		return s.completeCancelAnnouncement(ctx, st, st.GetLeavesQuantity(), now)
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
	return s.completeCancelAnnouncement(ctx, next, cancelledQty, now)
}

// completeCancelAnnouncement publishes the ORDER_CANCELLED FACT and the
// EXECUTED command outcome for an order already saved CANCELLED, then stamps
// cancel_announced_at so a further redelivery hits the terminal-refusal branch
// instead of resuming again.
//
// A DUPLICATE ORDER_CANCELLED FACT IS AN ACCEPTED TRADE-OFF. If EmitCancelled
// below succeeds and the Save at the end of this function then fails, the next
// redelivery re-enters here (cancel_announced_at is still unset) and re-emits
// both EmitCancelled and EmitOutcome. That folds "order X is cancelled" twice,
// which every downstream projection treats idempotently — versus the
// alternative this replaces, which lost the FACT entirely and told the caller
// REJECTED for a cancel that had already succeeded. Do not "fix" this into
// exactly-once without a fill_id-style dedup on the FACT itself; that is a
// larger change than this one.
//
// It is always called under the per-order lock (handleCancel and resume both
// hold it), so the duplicate this documents is a REDELIVERY duplicate only —
// never two goroutines announcing one cancellation concurrently.
func (s *Service) completeCancelAnnouncement(ctx context.Context, st *orderpb.OrderState, cancelledQty *commonpb.Decimal, now time.Time) error {
	if err := s.emitter.EmitCancelled(ctx, st.GetOrderId(), cancelledQty, now); err != nil {
		return err
	}
	if err := s.emitter.EmitOutcome(ctx, st.GetOrderId(),
		commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED, "order cancelled", "", "", now); err != nil {
		return err
	}
	announced := cloneState(st)
	announced.CancelAnnouncedAt = timestamppb.New(now)
	return s.store.Save(ctx, announced)
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
				"order history before it can be amended. quarantine reason: %s", q.GetReason()), now)
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
	fresh, err := s.store.Load(ctx, st.GetOrderId())
	if err != nil {
		return err
	}
	// TERMINAL BUT UNANNOUNCED: the state was persisted before the interrupted
	// delivery could announce it. Complete the announcement rather than ack a
	// redelivery of an order the world was never told about. This is decided
	// AGAIN, under the claim, because the goroutine we just raced for it may
	// have completed the announcement between our first read and this one.
	if IsTerminal(fresh) {
		if orderOutcomeAnnounced(fresh) {
			return nil
		}
		return s.completeTerminalOutcome(ctx, fresh)
	}
	if fresh.GetQuarantine() != nil {
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

// markOutcomeAnnounced stamps outcome_announced_at on a terminal order whose
// FACT and CommandOutcome have both just been published successfully, so a
// later redelivery's resume() recognizes a genuine duplicate instead of
// re-entering the reject/fill path that already completed. Mirrors
// completeCancelAnnouncement's trailing Save for the cancel transition.
func (s *Service) markOutcomeAnnounced(ctx context.Context, st *orderpb.OrderState, t time.Time) error {
	announced := cloneState(st)
	announced.OutcomeAnnouncedAt = timestamppb.New(t)
	return s.store.Save(ctx, announced)
}

// completeTerminalOutcome re-publishes the CommandOutcome for a SubmitOrder
// whose terminal state (FILLED or REJECTED) was persisted but never
// announced — a delivery that Saved the terminal OrderState and then failed
// before EmitFill/EmitRejected or the trailing EmitOutcome went out (see
// outcome_announced_at, order_events.proto:19). It is resume()'s completion
// of that interrupted announcement.
//
// WHAT IT CANNOT DO, AND WHY. The stored OrderState carries only the order's
// current AGGREGATE view — status, cumulative filled_quantity, leaves_quantity,
// average_fill_price (order/v1/order_events.proto:78-85). It has no field for
// the individual Fill that produced a FILLED state (fill_id, price,
// venue_execution_id, executed_at — order/v1/order_events.proto:169-211), nor
// for an OrderRejected's reason/error_code (order/v1/order_events.proto:222-230).
// Those values lived only in the local variables of the interrupted call —
// the `fill` loop variable in work() (service.go, the fill-fold loop) or the
// literal "PRICE_UNAVAILABLE"/err.Error() in handleSubmit's ErrUnpriced
// branch — and were never persisted anywhere this function, reading only
// store.Load's result, can recover them from. Re-emitting the exact
// ORDER_FILLED/ORDER_REJECTED FACT is therefore not possible here without
// fabricating a fill or a rejection reason, which is worse than the FACT
// arriving late: it would put invented data on the record.
//
// What CAN be reconstructed, honestly, from the terminal OrderState alone is
// the CommandOutcome for the original SubmitOrder command — its status
// follows directly from st.status, with no other input needed. That is what
// this publishes. The missing lifecycle FACT for this specific interruption
// is a residual, KNOWN LIMIT of this fix: tv-sync, accounting, and audit still
// never see it. What this fix removes is the worse failure — a completed
// trade or a permanent rejection whose command outcome the caller was never
// told, with no recovery path at all.
func (s *Service) completeTerminalOutcome(ctx context.Context, st *orderpb.OrderState) error {
	now := s.now().UTC()
	var status commandpb.CommandOutcomeStatus
	var reason, code string
	switch st.GetStatus() {
	case orderpb.OrderStatus_ORDER_STATUS_FILLED:
		status = commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED
		reason = "order filled (outcome re-announced after an interrupted delivery)"
		s.logger.Error("oms: completing an interrupted FILLED announcement WITHOUT its ORDER_FILLED FACT — "+
			"the original fill is not recoverable from the stored OrderState, so only the command outcome "+
			"is re-published; tv-sync, accounting, and audit will never see this fill's FACT",
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
	return s.markOutcomeAnnounced(ctx, st, now)
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
		rejected := Reject(st, now)
		if err := s.store.Save(ctx, rejected); err != nil {
			return err
		}
		reason := view.Reason
		if reason == "" {
			reason = "venue reports this order rejected"
		}
		if err := s.refuse(ctx, st.GetOrderId(), "VENUE_REJECTED", reason, now); err != nil {
			return err
		}
		// Same marker, same reason as the ErrUnpriced reject in handleSubmit:
		// see order_events.proto:19.
		return s.markOutcomeAnnounced(ctx, rejected, now)
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

	// REFUSE A MULTI-FILL VIEW. ApplyFill has no fill_id dedup — that is left to
	// the position book, which claims each fill_id in position_fills before
	// folding it into the POSITION. The ORDER AGGREGATE folded here has no such
	// guard, and cancel/amend and the read API consult this aggregate directly.
	//
	// The failure this prevents: work() folds fill 1 and Saves it, then fails
	// before fill 2's Save or EmitFill. The redelivery reaches here, the venue
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
		return s.quarantine(ctx, st, fmt.Sprintf(
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
		return s.quarantine(ctx, st, fmt.Sprintf(
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
	if err := s.store.Save(ctx, next); err != nil {
		return err
	}
	return nil
}
