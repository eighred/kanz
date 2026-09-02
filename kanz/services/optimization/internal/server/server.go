// Package server is the optimization service's HTTP surface (OPT-01e): optimize
// target weights + build a rebalance proposal, and materialize an approved
// proposal into OMS-01 order commands. Stateless over internal/optimization +
// the bridge; the market inputs (covariance/expected returns/current weights)
// arrive in the request, so the service needs no broker or database to serve the
// optimize/propose path. A real deployment wires the bus publisher + pre-trade
// gate behind the bridge seams at the composition root.
//
// IT CANNOT MATERIALIZE ANYTHING TODAY, AND THAT IS DELIBERATE (#646). No
// mandate reaches this service — not in the request, not from a registry — so
// /v1/propose returns proposals whose MandateStatus is UNCHECKED, and /v1/orders
// refuses to turn an unchecked proposal into order commands. The route is kept,
// and refuses out loud, rather than emitting capital commands certified by a
// check that never ran; Server.materialize carries what wiring retires it.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	optimizationpb "github.com/eighred/kanz/kanz-schemas-go/optimization/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/optimization"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/optimization/internal/bridge"
)

// maxRequestBytes bounds a /v1 request body. A dense covariance for a several-
// hundred-asset universe fits well under 8 MiB; the cap turns an unbounded body
// (which drives O(n³)/O(k³) solver work) into a 400.
const maxRequestBytes = 8 << 20 // 8 MiB

// Readiness gates traffic; the compute endpoints are pure, so the service is
// ready as soon as it is up (the flag exists for graceful shutdown).
type Readiness struct{ ready atomic.Bool }

func (r *Readiness) Set(ready bool) { r.ready.Store(ready) }
func (r *Readiness) Ready() bool    { return r.ready.Load() }

// Server is the HTTP handler.
type Server struct {
	logger    *slog.Logger
	readiness *Readiness
	mux       *http.ServeMux

	// freshness bounds how old a proposal's inputs may be when it becomes orders
	// (#970). Its ZERO VALUE REFUSES EVERYTHING — see bridge.ErrFreshnessUnbounded
	// — so a composition root that forgets WithProposalFreshness fails closed
	// rather than restoring the unbounded behaviour.
	freshness bridge.Freshness

	// materializerFor builds a per-request materializer scoped to the caller's
	// tenant, or is nil.
	//
	// NIL IS THE DEFAULT AND THE SAFE STATE: the route still builds the commands
	// and returns them, and nothing reaches the bus. It is non-nil only when the
	// composition root was given both a broker and OPTIMIZATION_AUTO_PUBLISH — a
	// reversal of the human-in-the-loop stance that has to be asked for by name.
	materializerFor MaterializerFor

	// halt is the platform kill-switch as this service sees it (#738/#739).
	//
	// NIL IS HALTED, and that is the whole reason the field is a *halt.Gate and
	// not a bool: a composition root that arms auto-publish and forgets to wire
	// the gate refuses to publish rather than trading through a declared halt.
	// The dry-run path never consults it — a halt refuses new execution
	// exposure, and a proposal that reaches nobody is not exposure.
	halt *halt.Gate

	// gate is what /v1/propose needs to reach a mandate verdict, or is the zero
	// value.
	//
	// ZERO IS THE OLD BEHAVIOUR, EXACTLY: no mandate resolves, CheckMandate
	// returns MandateUnchecked, and /v1/orders declines to materialize. Wiring it
	// is what produces a MandateFeasible proposal and therefore what makes
	// /v1/orders work at all (#751) — so the option is additive and the unwired
	// service is no less safe than before, only no more useful.
	gate MandateGate
}

// MandateGate is everything the propose path needs to turn a rebalance into a
// CHECKED one: whose rules apply, the evaluator, and the reference data the
// sector and issuer dimensions resolve through.
//
// ONE NAMED THING rather than three options, because they are useless apart. An
// engine with no mandate source checks nothing; a mandate source with no
// classifier resolves SECTOR to the empty bucket, which compliance refuses as
// unresolvable rather than passing (#640) — so a partial gate produces a
// confident refusal instead of a verdict, and that is worse than an honest
// MandateUnchecked. Supplying them together makes the partial case a visible
// choice at the composition root instead of a silent one.
type MandateGate struct {
	// Mandates resolves the mandate governing a portfolio, per TENANT. Nil ⇒ no
	// verdict is reachable and every proposal stays MandateUnchecked.
	Mandates compliance.MandateSource
	// Engine evaluates the candidate book against the mandate. Nil is tolerated
	// by CheckMandate (it builds a default), but the composition root supplies
	// the configured one.
	Engine *compliance.Engine
	// Classifier resolves SECTOR / ISSUER / ASSET_CLASS. Nil makes those
	// dimensions unresolvable, which the engine REFUSES rather than passes.
	Classifier compliance.Classifier
}

// Ready reports whether a verdict is reachable at all.
func (g MandateGate) Ready() bool { return g.Mandates != nil }

// MaterializerFor builds a materializer for ONE tenant — the authenticated
// caller's.
//
// A FACTORY RATHER THAN A SHARED VALUE, deliberately. One handler serves every
// tenant concurrently, so a materializer holding a tenant field that the handler
// stamped per request would let two callers publish under each other's tenant.
// That is the account-boundary failure this platform refuses to start over,
// arriving through a struct field instead of a manifest.
type MaterializerFor func(tenant string) Materializer

// Materializer publishes a materialized proposal's commands and records the
// materialization. The composition root supplies it; the handler never builds a
// bus envelope itself.
type Materializer interface {
	// Publish emits one command. It is the bridge.Publisher seam.
	Publish(ctx context.Context, cmd *orderpb.SubmitOrder) error
	// Record emits the ProposalMaterialized FACT. Best effort: the commands are
	// already in flight by the time it runs, so a failure is returned to be
	// logged and counted rather than failing the caller's request.
	Record(ctx context.Context, fact *optimizationpb.ProposalMaterialized) error
	// Armed reports whether this materializer actually sends commands. False is
	// the default posture — a broker is configured, the FACT is recorded, and
	// nothing is published — and the handler must report which happened.
	Armed() bool
}

// WithAutoPublish arms the switch (#409).
//
// It is an OPTION rather than a constructor parameter so that every existing
// caller — and every test — keeps the dry-run behaviour by construction. A
// service that starts trading on its own recommendation because someone added a
// parameter and a caller passed nil would be exactly the silent acquisition this
// must never have.
func WithAutoPublish(f MaterializerFor) Option { return func(s *Server) { s.materializerFor = f } }

// WithProposalFreshness sets the bound on how old a proposal's inputs may be
// (#970).
//
// NOT OPTIONAL IN EFFECT, only in syntax: without it Server.freshness is the zero
// Freshness, whose MaxAge is 0, and bridge.Freshness.Check refuses on that. The
// option exists so the number comes from the deployment's configuration rather
// than from a default this package invented — and the refusal is what makes
// forgetting it loud instead of silent.
func WithProposalFreshness(f bridge.Freshness) Option { return func(s *Server) { s.freshness = f } }

// WithMandateGate gives the propose path a mandate source, an evaluator and a
// classifier (#751).
//
// AN OPTION, like the two below, so a deployment that has not wired a mandate
// stream keeps today's behaviour by construction: proposals come back
// MandateUnchecked and /v1/orders refuses them. That is the state #646 chose
// deliberately, and it must stay reachable — a service that started certifying
// its own rebalances because someone added a constructor parameter and a caller
// passed the zero value would be the silent acquisition this design refuses.
func WithMandateGate(g MandateGate) Option { return func(s *Server) { s.gate = g } }

// WithHaltGate hands the server the platform kill-switch (#739).
//
// WHY THIS SERVICE NEEDS ITS OWN BRAKE when the OMS already refuses an order
// admitted during a halt: it is the same argument the api-gateway settled — a
// brake this close to the caller is the one that still works if the OMS is
// itself part of the incident, and an operator who halts the platform should get
// a refusal here rather than a 200 followed by an asynchronous rejection nobody
// is watching for. It is also the only layer that can refuse the rebalance
// WHOLE: the OMS sees N independent orders and can admit some of them, which is
// how a portfolio ends up with one leg of a pair trade on.
//
// An OPTION, like WithAutoPublish, so a caller that publishes nothing is not
// made to wire a subscription it has no use for.
func WithHaltGate(g *halt.Gate) Option { return func(s *Server) { s.halt = g } }

// Option customizes the server.
type Option func(*Server)

// WithMetrics is GONE, deliberately (#409). /metrics used to be mounted on this
// mux, which put it on the same port as the order-materializing routes — and
// allow-observability-scrape must admit whatever port carries /metrics. That is
// how every other header-trusting service on this platform ended up reachable
// from kanz-observability with a self-chosen principal (#232).
//
// Metrics are now served by a second listener owned by the composition root, so
// the port the monitoring plane may open carries nothing but telemetry.

// New builds the server and registers routes.
func New(readiness *Readiness, logger *slog.Logger, opts ...Option) *Server {
	s := &Server{logger: logger, readiness: readiness, mux: http.NewServeMux()}
	for _, opt := range opts {
		opt(s)
	}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	s.mux.HandleFunc("POST /v1/propose", s.handlePropose)
	s.mux.HandleFunc("POST /v1/orders", s.handleOrders)
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if !s.readiness.Ready() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// --- propose -----------------------------------------------------------------

type proposeRequest struct {
	PortfolioID     string      `json:"portfolio_id"`
	Instruments     []string    `json:"instruments"`
	ExpectedReturns []float64   `json:"expected_returns"`
	Covariance      [][]float64 `json:"covariance"`
	// Observations is how many periods the supplied covariance was estimated
	// over. Absent (0) means NOT STATED and is answered as such — the response
	// carries CovarianceQuality FULL_RANK rather than OBSERVED, so a caller can
	// tell a covariance nobody vouched for from one that was checked against its
	// own sample size (#621).
	Observations   int                         `json:"observations"`
	BlackLitterman *blRequest                  `json:"black_litterman"`
	Objective      optimization.Objective      `json:"objective"`
	Constraints    *optimization.ConstraintSet `json:"constraints"`
	Current        map[string]float64          `json:"current_weights"`
	NAV            float64                     `json:"nav"`
	Prices         map[string]float64          `json:"prices"`
	Threshold      float64                     `json:"threshold"`
	// Currency is the unit NAV and Prices are quoted in, needed to build the
	// candidate book a mandate is evaluated against.
	//
	// CALLER-SUPPLIED LIKE THE REST OF THE BOOK, and that is consistent rather
	// than a hole: this endpoint evaluates a portfolio the caller DESCRIBES —
	// weights, NAV and prices all arrive in the request — so the verdict is a
	// feasibility answer about a hypothetical, not an authorization. The
	// authoritative gate stays the OMS pre-trade path when the resulting orders
	// are actually submitted, which reads the book from the platform's own record.
	Currency string `json:"currency"`
}

// blRequest is the optional Black-Litterman input on /v1/propose. Present ⇒ the
// handler computes the posterior μ and uses it as ExpectedReturns before Optimize.
type blRequest struct {
	MarketWeights []float64 `json:"market_weights"`
	RiskAversion  float64   `json:"risk_aversion"`
	Tau           float64   `json:"tau"`
	Views         []viewDTO `json:"views"`
}

// viewDTO is one Black-Litterman view: a picking row p (length n), its return q,
// and its variance omega (0 ⇒ the He-Litterman default for this view).
type viewDTO struct {
	P     []float64 `json:"p"`
	Q     float64   `json:"q"`
	Omega float64   `json:"omega"`
}

func (s *Server) handlePropose(w http.ResponseWriter, r *http.Request) {
	// WHO IS ASKING — and on this route that is now load-bearing rather than
	// ceremony. A mandate is resolved PER TENANT, and the tenant comes from the
	// principal the gateway injects; before the mandate check existed this route
	// needed no identity, which is why it had none.
	principal, ok := auth.PrincipalFromHeaders(r.Header)
	if !ok || principal.Subject == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "no authenticated principal — this surface is reachable only through the api-gateway, " +
				"and whose mandate governs a portfolio cannot be answered without one",
		})
		return
	}
	var req proposeRequest
	if !decode(w, r, &req) {
		return
	}
	in := optimization.MarketInputs{
		Instruments:     req.Instruments,
		ExpectedReturns: req.ExpectedReturns,
		Covariance:      req.Covariance,
		Observations:    req.Observations,
	}
	if req.BlackLitterman != nil {
		mu, err := blMu(req)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		in.ExpectedReturns = mu
	}
	// ONE CALL, NOT TWO STEPS AND A MISSING THIRD (#751). This handler used to
	// run Optimize then Rebalance inline and stop — which is every step of
	// optimization.Propose except the mandate check, so the proposal it returned
	// carried MandateStatus's zero value and /v1/orders refused it. Calling
	// Propose is what makes a MandateFeasible proposal reachable; the mandate
	// below is what makes the verdict real rather than asserted.
	now := time.Now()
	mandate, ok := s.resolveMandate(w, r, principal, req.PortfolioID, now)
	if !ok {
		return
	}
	if mandate != nil && req.Currency == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "currency is required when a mandate governs this portfolio: the candidate book " +
				"is valued in it, and a currency restriction evaluated against an unstated unit " +
				"would be a verdict about nothing",
		})
		return
	}
	proposal, err := optimization.Propose(r.Context(), req.PortfolioID, in, req.Objective, req.Constraints,
		req.Current, req.NAV, req.Prices, req.Threshold,
		s.gate.Classifier, s.gate.Engine, mandate, req.Currency, now)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, proposal)
}

// resolveMandate asks WHOSE RULES APPLY, and fails closed when it cannot say.
//
// The tenant comes from the authenticated principal and never from the body:
// MandateSource takes a tenantID in its signature precisely so no caller can
// reach a mandate without saying whose, after tenant B's "growth" order was once
// evaluated against tenant A's "growth" limits (#243).
//
// A source that ERRORS does not fall through to an unchecked proposal. An
// unresolvable tenant is terminal — nobody can say which rules apply — and any
// other error is transient; both refuse here rather than returning a proposal
// whose MandateStatus would read as "not checked" when the truth is "could not
// be checked". Those spell the same thing to a reader and mean different things
// to an operator.
func (s *Server) resolveMandate(w http.ResponseWriter, r *http.Request, principal *auth.Principal,
	portfolioID string, asOf time.Time) (*compliancepb.Mandate, bool) {

	if s.gate.Mandates == nil {
		// No source wired: the pre-#751 posture, and it stays honest — the
		// proposal comes back MandateUnchecked and /v1/orders declines it.
		return nil, true
	}
	m, found, err := s.gate.Mandates.Mandate(r.Context(), principal.Tenant, portfolioID, asOf)
	switch {
	// BOTH TERMINAL SENTINELS, and they are terminal for the same reason: no
	// amount of asking again makes the answer appear. An unresolvable tenant
	// means nobody can say which rules apply; an unreadable mandate means the
	// rules that do apply cannot be parsed. Letting either fall through to the
	// transient arm below tells the caller to retry against data that cannot
	// change — which on the bus is an infinite redelivery that starves every
	// other portfolio of evaluation (#619), and here is a client retrying
	// forever. Refuse once, and say which it was.
	case errors.Is(err, compliance.ErrMandateTenantUnresolved),
		errors.Is(err, compliance.ErrMandateUnreadable):
		s.logger.WarnContext(r.Context(), "mandate is terminally unusable for a rebalance proposal",
			"portfolio_id", portfolioID, "tenant", principal.Tenant, "err", err)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "the mandate governing this portfolio could not be established or could not be " +
				"read, so none of its rules were evaluated and no proposal is returned. This does " +
				"not resolve on retry — the mandate must be republished",
		})
		return nil, false
	case err != nil:
		s.logger.ErrorContext(r.Context(), "mandate lookup failed for a rebalance proposal",
			"portfolio_id", portfolioID, "tenant", principal.Tenant, "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "the mandate source could not be read; this is retryable and no proposal is " +
				"returned rather than one nothing checked",
		})
		return nil, false
	case found.NoMandate():
		// NO MANDATE GOVERNS IT. CheckMandate answers MandateUnchecked for a nil
		// mandate — deliberately, because "nobody declared constraints" is not the
		// same claim as "the constraints were evaluated and passed" — so such a
		// portfolio still cannot materialize orders here. That is the honest
		// reading and it is recorded on #751 rather than papered over by
		// returning a verdict nothing produced.
		return nil, true
	}
	return m, true
}

// --- materialize to orders ---------------------------------------------------

type ordersRequest struct {
	Proposal optimization.RebalanceProposal `json:"proposal"`

	// Issuer is DECODED SO IT CAN BE REFUSED, never so it can be used (#409).
	//
	// The issuer is stamped on every emitted command and is what the audit trail
	// records as the person who moved the capital. It now comes from the
	// gateway-authenticated principal and from nowhere else. Accepting it from the
	// body was the forged-issuer hole AUTH-01c exists to close: this service
	// authenticates nobody, so any caller that reached it could attribute a trade
	// to anyone.
	//
	// SILENTLY OVERRIDING IT WOULD BE WORSE THAN ACCEPTING IT. A caller who sends
	// an issuer and gets a 200 has been told their attribution was honoured. It
	// was not. So a body that names an issuer is a 400 that says why.
	Issuer string `json:"issuer"`
}

// mandateVerdictFields are the Proposal fields that assert the OUTCOME OF A
// COMPLIANCE CHECK. None of them is the caller's to set, for the reason written
// above ordersRequest.Issuer, applied to the field that decides whether the
// rebalance becomes live orders at all: this service authenticates nobody, so
// any caller that reached it could certify its own proposal mandate-feasible and
// bridge.ToOrders would emit the commands (#646). #409 moved the issuer to the
// authenticated principal and left the constraint verdict exactly where the
// issuer had been.
//
// "MandateFeasible" IS THE PRE-#646 NAME of MandateStatus and is listed so that a
// client still sending it is REFUSED rather than ignored. An input the server
// drops on the floor tells the caller the same lie as one it overrides.
var mandateVerdictFields = []string{"MandateStatus", "MandateFeasible", "Violations"}

// orderDTO is the JSON-friendly projection of a SubmitOrder command (protojson
// for the full proto is heavier than this reporting surface needs).
type orderDTO struct {
	OrderID      string  `json:"order_id"`
	PortfolioID  string  `json:"portfolio_id"`
	InstrumentID string  `json:"instrument_id"`
	Side         string  `json:"side"`
	Quantity     float64 `json:"quantity"`
	Issuer       string  `json:"issuer"`
}

func (s *Server) handleOrders(w http.ResponseWriter, r *http.Request) {
	// WHO IS ASKING? The gateway is the sole identity authority on this platform:
	// it verifies the token and injects the principal headers, and this service is
	// reachable only through it (a NetworkPolicy is what makes trusting those
	// headers sound). No principal means either the caller bypassed the gateway or
	// the gateway is misconfigured — both are refusals, never an anonymous trade.
	principal, ok := auth.PrincipalFromHeaders(r.Header)
	if !ok || principal.Subject == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "no authenticated principal — this surface is reachable only through the api-gateway, " +
				"which is what makes the issuer on an emitted command real",
		})
		return
	}
	// THE RAW BODY, NOT JUST THE DECODED STRUCT. A field the caller must not set
	// has to be seen as PRESENT, and a Go zero value cannot tell "absent" from
	// "sent as the zero" — the same reason the issuer check below compares
	// against the empty string rather than trusting a decoded value.
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var req ordersRequest
	if err := json.Unmarshal(body, &req); err != nil {
		// The decoder's own message is passed through: MandateStatus.UnmarshalJSON
		// names the three legal verdicts, and a caller told only "invalid request
		// body" would have to guess which field it meant.
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body: " + err.Error()})
		return
	}
	if req.Issuer != "" && req.Issuer != principal.Subject {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "issuer is taken from the authenticated principal and must not be supplied in the body; " +
				"a command attributed to anyone but the caller is the forged-issuer defect AUTH-01c prevents",
		})
		return
	}
	if field, supplied := suppliedMandateVerdict(body); supplied {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "the proposal field " + field + " states the outcome of a mandate check and must " +
				"not be supplied in the body; it is the gate on whether this rebalance becomes live " +
				"orders, and a caller that could set it would be certifying its own trade",
		})
		return
	}
	s.materialize(w, r, req.Proposal, principal)
}

// materialize turns an approved proposal into order commands, publishes them if
// the switch is armed, and records the FACT. It is split out of handleOrders so
// that the WIRE boundary (who is asking, which fields are not theirs to set) and
// the CAPITAL ACTION are separately testable — the proposal it takes has already
// been through that boundary.
//
// NO REQUEST GETS PAST ITS FIRST STATEMENT TODAY. bridge.ToOrders refuses a
// proposal no mandate check has passed, and this service cannot produce a
// checked one; see the refusal below for what wiring changes that. The publish
// and FACT machinery beneath it is kept, and kept under test by calling this
// method directly, because it is correct and its absence is what would have to
// be rebuilt.
func (s *Server) materialize(w http.ResponseWriter, r *http.Request, proposal optimization.RebalanceProposal,
	principal *auth.Principal) {
	cmds, err := bridge.ToOrders(proposal, principal.Subject, s.freshness)
	if bridge.IsFreshnessRefusal(err) {
		// A STALE PROPOSAL IS NOT A MANDATE REFUSAL, and rendering it as one would
		// send the operator to the wrong place (#970). The two need different
		// actions — re-run the optimizer against the current book, versus
		// investigate a limit — so they get different status codes and different
		// reason classes rather than sharing the branch below.
		//
		// 409 AND NOT 422: the request is well formed and the caller was entitled
		// to make it; the proposal is simply no longer valid against the state.
		// 422 would tell them to fix their request, which is the wrong instruction
		// — the fix is to compute a new proposal.
		s.logger.Warn("refused to materialize a stale rebalance proposal",
			"portfolio_id", proposal.PortfolioID, "issuer", principal.Subject,
			"as_of", proposal.AsOf.UTC().Format(time.RFC3339), "err", err)
		code := bridge.RefusalCode(err)
		detail := "a rebalance proposal is a DELTA against the holdings read at as_of. Against a " +
			"book that has since moved the delta is the wrong trade, and every child this service " +
			"emits is a MARKET order — nothing absorbs the drift. Re-run the optimizer against " +
			"the current book; see #970"
		if code == "FRESHNESS_UNCONFIGURED" {
			// NOTHING IS WRONG WITH THIS PROPOSAL, and telling the caller to re-run
			// the optimizer would send them round a loop that refuses forever.
			detail = "this DEPLOYMENT has no OPTIMIZATION_PROPOSAL_MAX_AGE set, so no proposal can " +
				"be materialized. An unset bound is UNKNOWN rather than unlimited and refuses " +
				"closed; the proposal itself was not examined. See #970"
		}
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":  err.Error(),
			"reason": code,
			"as_of":  proposal.AsOf.UTC().Format(time.RFC3339),
			"detail": detail,
		})
		return
	}
	if bridge.IsEnvelopeRefusal(err) {
		// AN UNBOUNDED PROPOSAL IS NOT A MANDATE REFUSAL (#972), and rendering it
		// under the #646 detail below would tell an operator this service has no
		// mandate source — true, and not why THIS request was refused. The two
		// need different actions: state the envelope, versus wire a mandate stream.
		//
		// 422: unlike a stale proposal (409, where the request was fine and the
		// world moved), this request is genuinely incomplete — the envelope is the
		// caller's to supply, and supplying it is the fix.
		s.logger.Warn("refused to materialize an unbounded rebalance proposal",
			"portfolio_id", proposal.PortfolioID, "issuer", principal.Subject, "err", err)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  err.Error(),
			"reason": bridge.EnvelopeRefusalCode(err),
			"detail": "an approved rebalance must say what it may cost: a max_notional ceiling for " +
				"the whole trade list, and optionally a slippage bound and an execution window. " +
				"Without an envelope every child is an unbounded MARKET order good for the rest " +
				"of the day. An ABSENT envelope means nobody decided; a zero INSIDE one is a " +
				"decision. See #972",
		})
		return
	}
	if err != nil {
		// A PROPOSAL WITH NO VERDICT DOES NOT MATERIALIZE, AND THE REFUSAL SAYS SO.
		//
		// This service has no mandate source: proposeRequest carries none, Server
		// holds no compliance.Engine and no compliance.MandateSource, and the
		// composition root passes only WithAutoPublish — so /v1/propose builds
		// proposals that are MandateUnchecked and there is, today, no route through
		// this service that produces a MandateFeasible one. Every /v1/orders request
		// therefore ends here, and that is the honest state of the service rather
		// than a bug in this handler: the alternative is turning an unchecked
		// proposal into live SubmitOrder commands, which is what #646 is.
		//
		// RETIRING THIS means giving the service a mandate the caller did not
		// supply — the OMS's pattern, which builds a compliance.MandateRegistry and
		// replays the lifecycle.v1.ConfigChanged FACTs carrying each serialized
		// Mandate into it, gating readiness on MandateRegistry.Arm — and then
		// calling optimization.Propose instead of Optimize plus Rebalance. It is not
		// done here because a mandate this service invented, or accepted from the
		// requester, would be a control in name only.
		s.logger.Warn("refused to materialize a rebalance proposal",
			"portfolio_id", proposal.PortfolioID, "issuer", principal.Subject,
			"verdict", proposal.MandateStatus.String(), "err", err)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":   err.Error(),
			"verdict": proposal.MandateStatus.String(),
			"detail": "orders are materialized only from a proposal a mandate check has passed. This " +
				"service has no mandate source wired, so it cannot produce or certify one; see #646",
		})
		return
	}

	// AUTO-PUBLISH (#409). Off by default: the commands are built and returned,
	// and nothing reaches the bus — the human-in-the-loop stance the bridge's own
	// header describes. Armed, they go out on order.order.submit as COMMANDs,
	// through the OMS's ordinary admission path.
	//
	// THE PRINCIPAL RIDES ON THE CONTEXT because the producer re-checks the
	// issuer against it (AUTH-01c). This service already refused a body-supplied
	// issuer above; the bus check is the second, independent one, and it is what
	// makes the refusal structural rather than a handler's good manners.
	//
	// A PUBLISH FAILURE STOPS THE LOOP. Materialize returns what it managed to
	// send, and the caller is told: half a rebalance reported as a whole one is
	// how a portfolio ends up with one leg of a pair trade on.
	published := false
	var result bridge.MaterializeResult
	if s.materializerFor != nil {
		mat := s.materializerFor(principal.Tenant)
		published = mat.Armed()
		// THE PLATFORM HALT, CHECKED BEFORE THE FIRST COMMAND IS BUILT (#739).
		//
		// Whole-rebalance or nothing. Checking per-order inside the publish loop
		// would let a halt arriving mid-flight leave half a rebalance live, which
		// is the failure the loop's own comment below already refuses to accept
		// for a publish error. Only the armed path is gated: the dry run reaches
		// no bus, so there is no exposure for the brake to refuse.
		if published && s.halt.Halted() {
			mode, reason, since := s.halt.State()
			s.logger.Warn("rebalance materialization refused — the platform is halted",
				"portfolio_id", proposal.PortfolioID, "issuer", principal.Subject,
				"mode", mode.String(), "reason", reason, "since", since)
			writeJSON(w, http.StatusLocked, map[string]any{
				"error":  "the platform is halted; no order was published",
				"mode":   mode.String(),
				"reason": reason,
			})
			return
		}
		ctx := auth.WithPrincipal(r.Context(), principal)
		var perr error
		// gate is nil DELIBERATELY: the OMS re-runs the pre-trade gate on
		// admission and is the authority on it. A second mandate registry and
		// position book inside this service would be a second compliance
		// implementation, and the day the two disagreed nobody could say which
		// one governed the trade.
		if published {
			result, perr = bridge.Materialize(ctx, proposal, principal.Tenant, principal.Subject,
				"", nil, nil, mat, s.freshness)
		} else {
			result = bridge.MaterializeResult{Submitted: cmds}
		}
		s.recordMaterialization(ctx, mat, proposal, principal, published, result)
		if perr != nil {
			s.logger.Error("auto-publish failed part-way through a rebalance — some commands are in "+
				"flight and the rest are not",
				"portfolio_id", proposal.PortfolioID, "issuer", principal.Subject,
				"submitted", len(result.Submitted), "err", perr)
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error":     "the rebalance was published only in part; the submitted orders are live",
				"submitted": len(result.Submitted),
			})
			return
		}
		cmds = result.Submitted
	}

	out := make([]orderDTO, 0, len(cmds))
	for _, c := range cmds {
		q := c.GetQuantity()
		out = append(out, orderDTO{
			OrderID:      c.GetOrderId(),
			PortfolioID:  c.GetPortfolioId(),
			InstrumentID: c.GetInstrumentId(),
			Side:         c.GetSide().String(),
			Quantity:     float64(q.GetCoefficient()) * pow10(q.GetExponent()),
			Issuer:       c.GetMetadata().GetIssuer(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"orders": out, "count": len(out), "published": published})
}

// recordMaterialization emits the ProposalMaterialized FACT.
//
// IT RUNS ON BOTH PATHS, and `published` is what separates them. A dry run is a
// real outcome — the commands were built and handed to the caller — and a reader
// must be able to tell it from a live run without consulting the deployment's
// environment. "Nothing configured" and "checked, and fine" must never look the
// same, and that applies to a capital action as much as to a control.
//
// Failures are logged, never fatal: by the time this runs the commands are
// already in flight, so refusing the request would misreport what happened. But
// losing the record of an automated capital action is a defect, so it is loud.
func (s *Server) recordMaterialization(ctx context.Context, mat Materializer,
	p optimization.RebalanceProposal, principal *auth.Principal, published bool,
	res bridge.MaterializeResult) {
	if mat == nil {
		return // no broker: there is nowhere to record it, and the composition root said so at startup
	}
	fact := &optimizationpb.ProposalMaterialized{
		PortfolioId: p.PortfolioID,
		Issuer:      principal.Subject,
		Published:   published,
	}
	// THE PROPOSAL'S OWN HORIZON, BESIDE THE MATERIALIZATION INSTANT (#970). The
	// gap between the two IS the drift this decision was exposed to; without it
	// on the record an auditor asking "how stale was the book this rebalance was
	// built on" has no answer and no way to reconstruct one. A zero AsOf is left
	// nil rather than stamped as the epoch — an undated proposal is refused
	// upstream, and encoding "no horizon" as 1970 would put a false one on the
	// FACT.
	if !p.AsOf.IsZero() {
		fact.ProposalAsOf = timestamppb.New(p.AsOf.UTC())
	}
	// AND THE ENVELOPE IT ACTED WITHIN (#972). Without it the record says what
	// moved and not what bounded it: an auditor asking "was this rebalance capped,
	// and at what" would have to find the proposal, which arrived in a request
	// body nobody kept.
	if c := p.Constraints; c != nil {
		fact.Constraints = &optimizationpb.ProposalConstraints{
			MaxNotional:     c.MaxNotional,
			MaxSlippageBps:  c.MaxSlippageBPS,
			ExecutionWindow: durationpb.New(c.ExecutionWindow),
			ReasonCodes:     c.ReasonCodes,
		}
		if !c.ExpiresAt.IsZero() {
			fact.Constraints.ExpiresAt = timestamppb.New(c.ExpiresAt.UTC())
		}
	}
	for _, c := range res.Submitted {
		fact.SubmittedOrderIds = append(fact.SubmittedOrderIds, c.GetOrderId())
	}
	for _, rj := range res.Rejected {
		fact.Rejected = append(fact.Rejected, &optimizationpb.RejectedOrder{
			OrderId:      rj.Command.GetOrderId(),
			InstrumentId: rj.Command.GetInstrumentId(),
			Reason:       rj.Reason,
		})
	}
	if err := mat.Record(ctx, fact); err != nil {
		s.logger.Error("MATERIALIZATION WENT UNRECORDED — orders were issued and the audit FACT that "+
			"names who authorized them did not reach the bus",
			"portfolio_id", p.PortfolioID, "issuer", principal.Subject,
			"submitted", len(res.Submitted), "err", err)
	}
}

// --- helpers -----------------------------------------------------------------

// blMu assembles a BLInput from the request's black_litterman block and returns
// the posterior expected returns. A view omega of 0 stays 0 in the diagonal we
// pass, which BlackLitterman reads as "use the He-Litterman default for this view".
func blMu(req proposeRequest) ([]float64, error) {
	bl := req.BlackLitterman
	k := len(bl.Views)
	p := make([][]float64, k)
	q := make([]float64, k)
	omega := make([]float64, k)
	for i, v := range bl.Views {
		p[i] = v.P
		q[i] = v.Q
		omega[i] = v.Omega
	}
	return optimization.BlackLitterman(optimization.BLInput{
		Covariance:    req.Covariance,
		MarketWeights: bl.MarketWeights,
		RiskAversion:  bl.RiskAversion,
		Tau:           bl.Tau,
		P:             p,
		Q:             q,
		Omega:         omega,
	})
}

func pow10(exp int32) float64 {
	p := 1.0
	for i := int32(0); i < exp; i++ {
		p *= 10
	}
	for i := int32(0); i < -exp; i++ {
		p /= 10
	}
	return p
}

// suppliedMandateVerdict reports the name of a proposal field that asserts a
// mandate verdict, when the request body sets one.
//
// It reads the RAW body rather than the decoded proposal so that the pre-#646
// spelling — which no longer maps to a Go field and would otherwise be discarded
// without a word — is caught too. Values that assert nothing are allowed
// through, so a client can echo a /v1/propose response back unchanged and get
// the refusal that describes its actual problem (nothing checked it) rather than
// one about a field it merely copied.
func suppliedMandateVerdict(body []byte) (string, bool) {
	var outer struct {
		Proposal map[string]json.RawMessage `json:"proposal"`
	}
	if err := json.Unmarshal(body, &outer); err != nil {
		return "", false // an unparseable body was already refused by the decode above
	}
	for _, name := range mandateVerdictFields {
		for key, raw := range outer.Proposal {
			// EqualFold because encoding/json matches keys case-insensitively: a
			// case-sensitive check here would refuse "MandateStatus" and admit
			// "mandatestatus", which the decoder honours.
			if strings.EqualFold(key, name) && !assertsNoVerdict(raw) {
				return key, true
			}
		}
	}
	return "", false
}

// assertsNoVerdict reports whether a raw JSON value makes no claim about a
// mandate check — null, an empty list, and the UNCHECKED name itself, which are
// exactly what /v1/propose puts on the wire. Everything else, INCLUDING false,
// is a verdict: false was the pre-#646 spelling of infeasible.
func assertsNoVerdict(raw json.RawMessage) bool {
	switch strings.ToUpper(strings.TrimSpace(string(raw))) {
	case "NULL", "[]", "\"\"", "\"UNCHECKED\"":
		return true
	}
	return false
}

// readBody reads a bounded request body whole, for a handler that needs the raw
// bytes as well as the decoded struct.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body: " + err.Error()})
		return nil, false
	}
	return b, true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
