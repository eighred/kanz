// Package orders is the api-gateway's governed write surface (OMS-01d): the
// only authenticated entry point that turns a client request into an order
// COMMAND on the bus. It binds the authenticated principal as the command
// issuer (the client cannot forge it — the gateway overrides any body value),
// stamps the tenant, and keys envelope dedup on the idempotency key, then
// publishes; the bus Producer's VerifyCommandIssuer (AUTH-01c) is the second
// layer that rejects a command whose issuer the caller may not assume.
package orders

import (
	"context"
	"io"
	"net/http"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"time"

	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"

	"github.com/eighred/kanz/internal/orderid"
	"github.com/eighred/kanz/internal/platform/halt"
)

// Order command subjects (mirror services/oms/internal/order; kept local so the
// gateway does not depend on the OMS service package).
const (
	subjectSubmit  = "order.order.submit"
	subjectCancel  = "order.order.cancel"
	subjectApprove = "order.order.approve"
	domain         = "order"
)

// maxBodyBytes is middleware.MaxRequestBody, not a fourth copy of 1 MiB (#887).
//
// This number was declared THREE times across the api-gateway — here,
// internal/orders and internal/proxy — each with its own comment justifying the
// same ceiling. Three spellings of one bound is how it gets raised in one place
// and not the others. The middleware package owns it because that layer applies
// it BEFORE authentication, and an arch guard ties it to the edge's own
// proxy-body-size so the two cannot drift.
const maxBodyBytes = middleware.MaxRequestBody

// Publisher is the bus publish surface — satisfied by *bus.Producer.
type Publisher interface {
	Publish(ctx context.Context, e bus.Event) error
}

// Handler serves POST /v1/orders, /v1/orders/{id}/cancel and — when the
// deployment names an approver — /v1/orders/{id}/approve. A nil publisher means
// writes are disabled (the gateway runs read-only) — the routes 503.
type Handler struct {
	pub Publisher
	// approveRole is the deployment's approver (API_GATEWAY_APPROVE_ROLE). EMPTY ⇒
	// the approve route is not registered (#535); see Routes for why that is not
	// the same as registering it and refusing everyone.
	//
	// A ROLE STRING RATHER THAN A BOOL, so the composition root passes what config
	// already holds and nothing has to restate the condition — the same shape as
	// proxy.Roles, which fronts the other half of this control.
	approveRole string
	// halted is the platform kill-switch (#635). It is a POSITIONAL argument to
	// New rather than an option because this is the capital path's front door: an
	// optional brake is an absent brake, and a nil *halt.Gate answers halted, so
	// the compiler is what makes every construction of this handler state where
	// its brake comes from.
	halted    *halt.Gate
	unmarshal protojson.UnmarshalOptions
}

// New returns a write handler over the publisher. A nil publisher disables the
// write routes; an empty approveRole leaves the approve route unregistered.
//
// gate is the platform kill-switch. A NIL GATE IS HALTED, so a caller that
// passes nil gets a surface that refuses every order — which is the safe
// direction and never the intended one; production passes the gate halt.Arm
// folds the operator's ModeChanged FACT into.
func New(pub Publisher, approveRole string, gate *halt.Gate) *Handler {
	return &Handler{
		pub:         pub,
		approveRole: approveRole,
		halted:      gate,
		unmarshal:   protojson.UnmarshalOptions{DiscardUnknown: true},
	}
}

// refuseIfHalted answers 423 Locked when the platform kill-switch is engaged, and
// reports whether it did (#635).
//
// WHY THE GATEWAY CHECKS AT ALL, when the OMS refuses the same order one hop
// later. Two reasons, and neither is redundancy for its own sake. The first is
// the answer a human gets: without this the client is told 202 Accepted and has
// to discover from an asynchronous ORDER_REJECTED that the platform is halted —
// during an incident, with an operator watching. 423 with the reason and the
// timestamp is the honest answer to "did my order go through". The second is
// that a brake this close to the caller is the one that still works if the OMS
// is itself part of the incident.
//
// 423 LOCKED, not 503 and not 403. 503 means "try again shortly", which is wrong:
// nothing changes until an operator resumes, and a client that retries a 503 is
// hammering a halted platform. 403 means "you may not", when the truth is that
// nobody may. The gateway already answers 423 for a halted webhook path, so the
// two write surfaces speak with one voice.
//
// CANCEL DOES NOT CALL THIS. A halt refuses new exposure; it does not lock an
// operator out of the exits. See halt.Gate for the whole boundary.
func (h *Handler) refuseIfHalted(w http.ResponseWriter) bool {
	if h.pub == nil {
		// A READ-ONLY GATEWAY HAS NO WRITE SURFACE TO BRAKE. Its own answer — 503
		// "order writes are disabled", from publish below — is the true one, and
		// it must not be displaced by a fact about a path this deployment does not
		// have. Such a deployment also has no bus, so its gate could only ever
		// report the closed zero value.
		return false
	}
	reason, halted := halt.Refusal(h.halted)
	if !halted {
		return false
	}
	writeError(w, http.StatusLocked, "order refused: "+reason)
	return true
}

// refuseIfUnnameable answers 400 when a submit carries no order_id AND no
// Idempotency-Key, and reports whether it did (#723). Submit only: cancel and
// approve take their key from the request path and the approver, both of which
// a retry reproduces.
//
// A MINTED ID CANNOT ALSO BE THE DEDUP KEY. Minting is safe when the CLIENT has
// named the operation, because then the value the broker collapses on is the
// header and the fresh id is merely the order's name. With no header the id
// would be both, and it is new on every attempt — so the retry of an ambiguous
// timeout published a second COMMAND under a second Nats-Msg-Id, and
// JetStream's dedup, the OMS consumer's claim and this gateway's own
// middleware.Idempotency all missed for that one reason. Because
// internal/execution stamps order_id straight into the venue clOrdId, the
// second COMMAND became a second LIVE ORDER at the exchange.
//
// THIS IS THE CASE THE ESTATE'S BACKSTOP CANNOT CATCH, which is what separates
// it from the cross-pod claim #721 landed. TENANT_ORDER is provisioned with
// --dupe-window=2m (infra/nats/bootstrap-job.yaml), so a retry that DOES carry a
// key is collapsed by the broker even when a per-pod claim misses it. A retry
// with no key presents a different Nats-Msg-Id, so the window has nothing to
// match and every attempt is stored. Measured, not reasoned: three attempts of
// one intent against a real nats-server put three SubmitOrder COMMANDs on a
// stream that had the 2m window configured.
//
// REFUSING IS THE ONLY HONEST ANSWER, not a conservative one. The server cannot
// recognise the retry: deriving a key from the request body would collapse two
// deliberately identical orders, and a fund sending the same order twice on
// purpose is ordinary. Between silently placing two orders and silently placing
// none, this path takes neither — it refuses in the client's own response, and
// names both remedies so the refusal is actionable. That is CLAUDE.md's
// fail-loudly rule on the one path where the silent alternative spends capital.
//
// A READ-ONLY GATEWAY IS EXEMPT, for the reason refuseIfHalted gives one line
// up: with no publisher there is no write surface, publish answers 503 "order
// writes are disabled", and that true answer must not be displaced by a fact
// about a path this deployment does not have. Nothing is published either way,
// so the exemption costs no safety.
func (h *Handler) refuseIfUnnameable(w http.ResponseWriter, r *http.Request) bool {
	if h.pub == nil || r.Header.Get("Idempotency-Key") != "" {
		return false
	}
	writeError(w, http.StatusBadRequest,
		"this submit carries neither an order_id nor an Idempotency-Key, so a retry after a "+
			"timeout could not be told apart from a second order and would place one; the order "+
			"was NOT submitted. Send an Idempotency-Key header, or set order_id in the body, "+
			"and retry")
	return true
}

// Routes registers the write endpoints.
//
// THIS IS THE CAPITAL PATH. Every route here reaches a live exchange, so every route here
// requires an authority that moves capital — and an arch test asserts that property over
// WHATEVER this package registers, so a route added tomorrow is covered the moment it exists.
//
// TWO AUTHORITIES, HELD BY DIFFERENT PEOPLE (#539, #410). Submit and cancel require Trade.
// Approve requires authz.Approve and MUST NOT accept Trade: it is the second signature that
// releases an order the OMS held for exceeding the dual-control threshold, and a signature
// the proposer can give themselves is one signature recorded as two.
//
// THE APPROVE ROUTE IS REGISTERED ONLY WHEN AN APPROVER IS NAMED (#535). authz.Approve is
// carried by no role unless API_GATEWAY_APPROVE_ROLE names one, and a route whose capability
// nobody holds answers 403 to every principal that exists — "you may not", when the truth is
// "nobody may, in this deployment". That is indistinguishable from a control working as
// intended, which is how the funding route survived from #415 to #535 unnoticed. Unregistered,
// the answer is 404: there is no approval surface here, and that is both true and actionable.
//
// NOT GATED ON THE PUBLISHER, deliberately — a configured approver on a read-only gateway
// still gets 503 from publish like the other two routes, because "writes are disabled" and
// "you are not the approver" are different answers and both beat a 403 nobody can act on.
func (h *Handler) Routes(mux *authz.Mux) {
	mux.Handle(authz.Trade, "POST /v1/orders", h.submit)
	mux.Handle(authz.Trade, "POST /v1/orders/{id}/cancel", h.cancel)
	if h.approveRole != "" {
		mux.Handle(authz.Approve, "POST /v1/orders/{id}/approve", h.approve)
	}
}

func (h *Handler) submit(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	// The brake, before the body is even read — the same placement the webhook
	// perimeter uses, for the same reason: a halted platform does not do work it
	// is going to refuse.
	if h.refuseIfHalted(w) {
		return
	}
	// +1 SO AN OVERSIZED BODY IS DETECTED RATHER THAN TRUNCATED (#887). Without
	// it LimitReader silently cut the body at exactly the ceiling and handed the
	// prefix to protojson, which then failed to parse — so a body that was too
	// LARGE was reported as "invalid order body", sending the caller to look for
	// a syntax error it did not make. The other two readers in this service
	// already used the +1 form; this one did not.
	//
	// middleware.BodyLimit refuses the same request one layer out, so this is a
	// backstop rather than the primary bound — it still has to be right, because
	// a handler mounted outside that chain would have only this.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body failed")
		return
	}
	if len(body) > maxBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	var cmd orderpb.SubmitOrder
	if err := h.unmarshal.Unmarshal(body, &cmd); err != nil {
		writeError(w, http.StatusBadRequest, "invalid order body: "+err.Error())
		return
	}
	if cmd.GetOrderId() == "" {
		if h.refuseIfUnnameable(w, r) {
			return
		}
		// NOT uuid.NewString(). The order id is stamped directly as the venue's
		// client order id, and OKX refuses a clOrdId that is longer than 32
		// characters or contains anything but letters and digits — so a
		// hyphenated 36-character UUID made every order minted here UNPLACEABLE
		// at OKX, while Binance accepted the same id and traded normally.
		// orderid.Mint gives the same 128 bits in 32 hex characters, which is
		// also the shape the signal fan-out has always produced.
		cmd.OrderId = orderid.Mint()
	}
	// Bind identity: the gateway is the sole issuer authority — it overrides any
	// client-supplied metadata so the issuer cannot be forged.
	cmd.Metadata = bindMetadata(cmd.GetMetadata(), p, cmd.GetOrderId())

	if err := h.publish(r.Context(), p, subjectSubmit, cmd.GetOrderId(), &cmd, idempotencyKey(r, cmd.GetOrderId())); err != nil {
		writePublishError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"order_id": cmd.GetOrderId(), "status": "submitted"})
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	orderID := r.PathValue("id")
	cmd := &orderpb.CancelOrder{
		OrderId:  orderID,
		Metadata: bindMetadata(nil, p, orderID),
	}
	if err := h.publish(r.Context(), p, subjectCancel, orderID, cmd, idempotencyKey(r, orderID)); err != nil {
		writePublishError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"order_id": orderID, "status": "cancel_requested"})
}

// approve is the SECOND SIGNATURE on an order the OMS is holding (#539, #410).
// The OMS holds anything over the dual-control threshold until an ApproveOrder
// arrives; the gateway is its only permitted caller under network-policies.yaml,
// so until this route existed a held order was held by nobody's decision forever
// — a control that from the outside is indistinguishable from an outage.
func (h *Handler) approve(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	// AN APPROVAL IS A NEW ORDER, arriving late. The OMS replays every gate when
	// it releases a held proposal, so a signature collected before the halt must
	// not be the thing that puts an order in front of an exchange during one.
	if h.refuseIfHalted(w) {
		return
	}
	// +1 SO AN OVERSIZED BODY IS DETECTED RATHER THAN TRUNCATED (#887). Without
	// it LimitReader silently cut the body at exactly the ceiling and handed the
	// prefix to protojson, which then failed to parse — so a body that was too
	// LARGE was reported as "invalid order body", sending the caller to look for
	// a syntax error it did not make. The other two readers in this service
	// already used the +1 form; this one did not.
	//
	// middleware.BodyLimit refuses the same request one layer out, so this is a
	// backstop rather than the primary bound — it still has to be right, because
	// a handler mounted outside that chain would have only this.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body failed")
		return
	}
	if len(body) > maxBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	var cmd orderpb.ApproveOrder
	if err := h.unmarshal.Unmarshal(body, &cmd); err != nil {
		writeError(w, http.StatusBadRequest, "invalid approval body: "+err.Error())
		return
	}
	// AN APPROVAL WITH NO DIGEST SIGNS NOTHING. The digest is the value the
	// signature covers; without it the stored command could change between the
	// proposal and the release — propose a defensible order, collect the
	// approval, apply a different one. The OMS re-derives the digest and is the
	// authority on whether it MATCHES; this is the shape check, refused here
	// only because a 202 followed by a silent bus-side rejection is exactly the
	// "looks healthy" failure this platform does not ship.
	if cmd.GetDigest() == "" {
		writeError(w, http.StatusBadRequest,
			"an approval must carry the digest of the command it releases, or it signs nothing")
		return
	}
	// THE PATH AND THE PRINCIPAL WIN OVER THE BODY. A caller-supplied order id
	// approves somebody else's order and a caller-supplied issuer is one person
	// signing as two — dual control over a forgeable identity is theatre, which
	// is the forged-actor defect #444 closed on the override path.
	orderID := r.PathValue("id")
	cmd.OrderId = orderID
	cmd.Metadata = bindMetadata(cmd.GetMetadata(), p, orderID)

	if err := h.publish(r.Context(), p, subjectApprove, orderID, &cmd, approvalIdempotencyKey(r, orderID, p)); err != nil {
		writePublishError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"order_id": orderID, "status": "approval_submitted"})
}

// publish stamps the command envelope and hands it to the producer, with the
// authenticated principal on ctx so the producer's VerifyCommandIssuer (AUTH-01c)
// can run.
func (h *Handler) publish(ctx context.Context, p *middleware.Principal, subject, orderID string, payload proto.Message, idem string) error {
	if h.pub == nil {
		return errWritesDisabled
	}
	// The REVERSE of cmd/api-gateway's edgePrincipal, and it carries the
	// portfolio scope for the same reason that one does (#225): a principal
	// stripped of a dimension on the way back is the same defect facing the other
	// way. Claims cannot be reconstructed from the edge type and is deliberately
	// left nil — the producer's VerifyCommandIssuer falls back to the subject
	// match, which is the branch a gateway-minted "user:{sub}" issuer takes.
	ctx = auth.WithPrincipal(ctx, &auth.Principal{
		Subject: p.Subject, Tenant: p.Tenant, Roles: p.Roles, Portfolios: p.Portfolios,
	})
	return h.pub.Publish(ctx, bus.Event{
		// SUBJECT CARRIES THE TENANT; EVENT_TYPE DOES NOT. They are deliberately
		// different here and nowhere else in the estate — see bus.TenantRoutedSubject.
		Subject:        bus.TenantRoutedSubject(p.Tenant, subject),
		EventType:      subject,
		EventClass:     envelopepb.EventClass_EVENT_CLASS_COMMAND,
		SchemaVersion:  1,
		Domain:         domain,
		EventTime:      time.Now().UTC(),
		PartitionKey:   orderID,
		IdempotencyKey: idem,
		TenantID:       p.Tenant,
		Payload:        payload,
	})
}

func (h *Handler) principal(w http.ResponseWriter, r *http.Request) (*middleware.Principal, bool) {
	p := middleware.PrincipalFromContext(r.Context())
	if p == nil || p.Subject == "" {
		writeError(w, http.StatusUnauthorized, "authentication required to issue an order")
		return nil, false
	}
	// AN AUTHENTICATED CALLER WITH NO TENANT MAY NOT MOVE CAPITAL.
	//
	// Neither authenticator requires the tenant claim: the OIDC path reads
	// stringClaim(custom[cfg.TenantClaim]) and the dev HS256 path decodes
	// `tenant`, and both accept a token that omits it, checking only the subject.
	// This handler then stamps Event.TenantID from that principal, so without
	// this gate an order command reaches the producer with an empty tenant.
	//
	// Refusing is the point, and the alternative was considered and rejected:
	// giving this service a ProducerConfig.Tenant fallback would make the publish
	// SUCCEED, silently attributing somebody's order to the gateway's own tenant.
	// A capital command booked against the wrong tenant is wrong forever and
	// nothing downstream can tell. On this path the absence of proof is not
	// proof — it is the absence of entitlement, the same stance the OMS takes in
	// entitledTo, where an empty portfolio claim denies rather than permits.
	//
	// 403, not 401: the caller IS authenticated. The token is valid and simply
	// does not say whose capital it may spend, which re-authenticating will not
	// fix — an operator has to correct the claim (or API_GATEWAY_OIDC_TENANT_CLAIM,
	// if it names a claim the IdP does not populate).
	if p.Tenant == "" {
		writeError(w, http.StatusForbidden,
			"this token carries no tenant, so it cannot say whose capital an order would spend")
		return nil, false
	}
	return p, true
}

// bindMetadata sets the command-common metadata from the authenticated
// principal, overriding any client-supplied value (anti-forgery). The issuer is
// "user:{subject}"; target_id is the order id.
func bindMetadata(existing *commandpb.CommandMetadata, p *middleware.Principal, orderID string) *commandpb.CommandMetadata {
	reason := ""
	if existing != nil {
		reason = existing.GetReason() // a client-supplied note is advisory; preserve it
	}
	return &commandpb.CommandMetadata{
		Issuer:   "user:" + p.Subject,
		TargetId: orderID,
		Reason:   reason,
		// From the AUTHENTICATED principal, never from `existing` — a
		// client-supplied scope is a caller granting themselves the portfolio
		// they are attacking. Same rule as Issuer above, and for the same reason.
		PrincipalPortfolios: p.Portfolios,
	}
}

// idempotencyKey prefers the client's Idempotency-Key header (API-01d dedup);
// absent, the order id is the natural key.
//
// SUBMIT AND CANCEL ONLY. An order is submitted once and cancelled once, so the
// order id IS the natural key for them and a retry that lands on another pod
// must collapse. Approval is not once-per-order — see approvalIdempotencyKey.
//
// THE FALLBACK IS ONLY STABLE IF THE CALLER'S ORDER ID IS (#723). This comment
// used to say "a stable natural key" flatly, and that read as a property of the
// function when it is a precondition on the caller. `cancel` meets it — the id
// comes from the request path. `submit` did not: it minted an id moments before
// calling this, so the "natural key" was fresh per attempt and every dedup layer
// downstream missed. submit now refuses that combination rather than passing a
// minted id through here, which is what keeps this fallback honest.
func idempotencyKey(r *http.Request, orderID string) string {
	if k := r.Header.Get("Idempotency-Key"); k != "" {
		return k
	}
	return orderID
}

// approvalIdempotencyKey is the order id AND the approver, because "Alice
// approves order X" and "Bob approves order X" are different commands (#581).
//
// WHAT THE BARE ORDER ID DID. The key rides as the broker's Nats-Msg-Id and
// JetStream collapses duplicates inside the stream's window. An approval can be
// REFUSED and then legitimately retried BY SOMEBODY ELSE — that is the whole
// point of maker-checker, and self-approval is its most common refusal. Alice
// proposes and cannot sign; Bob signs; Bob's command carried the same key as
// Alice's refused attempt and was dropped by the broker. Bob got 202, the OMS
// never saw it, and the order stayed pending with nothing saying why — #539's
// shape on the path built to fix it.
//
// It became reachable when #575 put the refusal on the pending queue: until the
// approver could SEE the refusal, nobody knew to retry.
//
// A SAME-SUBJECT RETRY IS STILL COLLAPSED, which is the property not to trade
// away — a double-clicked approve must not publish twice.
//
// The header still wins. A client that sends Idempotency-Key has said what it
// means by "the same request", and this estate does not second-guess that.
func approvalIdempotencyKey(r *http.Request, orderID string, p *middleware.Principal) string {
	if k := r.Header.Get("Idempotency-Key"); k != "" {
		return k
	}
	// NOT string concatenation with a separator: a subject is free-form and any
	// separator can appear inside one, so "a|b" + "c" and "a" + "b|c" would
	// collide — the same argument dualcontrol.Digest makes for length-prefixing.
	// A collision here silently drops a real approval.
	return dualcontrol.Digest(orderID, p.Subject)
}
