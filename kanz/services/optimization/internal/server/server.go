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
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	optimizationpb "github.com/eighred/kanz/kanz-schemas-go/optimization/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

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
}

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
	res, err := optimization.Optimize(in, req.Objective, req.Constraints)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	proposal := optimization.Rebalance(req.PortfolioID, req.Current, res.Weights, req.NAV, req.Prices, req.Threshold, time.Now())
	proposal.Objective = req.Objective
	proposal.ExpectedReturn = res.ExpectedReturn
	// BOTH FIELDS OR NEITHER. ExpectedRisk is nil whenever the covariance could
	// not support a risk number, and CovarianceQuality is what says why — a
	// response carrying the first without the second would put a null on the wire
	// with no explanation for it (#621).
	proposal.ExpectedRisk = res.ExpectedRisk
	proposal.CovarianceQuality = res.CovarianceQuality
	writeJSON(w, http.StatusOK, proposal)
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
	cmds, err := bridge.ToOrders(proposal, principal.Subject)
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
				"", nil, nil, mat)
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
