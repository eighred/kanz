// Package server is a venue adapter's gRPC face: the venue.v1.VenueAdapterService
// the OMS calls (INFRA-M7a). It is venue-agnostic — venue-binance and venue-okx
// both serve it, fronting their own connector.
//
// It is a thin shell on purpose. All the exchange behaviour — signing, rate
// limits, partial-fill aggregation, the healing seam — already lives in the
// connector and did not change when it crossed a process boundary. This layer
// does three things: record the order in the adapter's own view, delegate to the
// connector, and preserve the two contracts that a naive RPC shell would quietly
// destroy.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/internal/venueadapter/orderview"
)

// requireDecimalDomain refuses a request carrying an out-of-domain Decimal
// (#246), before the order is recorded or worked.
//
// Found by the widened arch guard, not by hand: the OrderState on these two
// requests carries quantity, limit_price, filled_quantity and average_fill_price
// as common.v1.Decimal, and Decimal.exponent is a plain int32 on the wire.
// internal/execution/exchange_common.go renders those with the UNBOUNDED
// dec.FromProto, which materialises 10^abs(exponent) — an order carrying
// {1, -2000000000} does not place a wrong trade, it stops the adapter answering
// while it still reports healthy.
//
// The OMS is the only caller a NetworkPolicy lets through, which bounds WHO can
// send this, not WHAT they can send: the OrderState originates in a user command
// and reaches here through the OMS unchanged. "Only an internal caller" has
// never been a domain check.
func requireDecimalDomain(req proto.Message) error {
	path, ok := dec.InDomainDeep(req)
	if ok {
		return nil
	}
	return status.Errorf(codes.InvalidArgument,
		"venue: %s carries a Decimal whose exponent is outside the computable domain (|exponent| > 64)", path)
}

// Server implements venue.v1.VenueAdapterService over one exchange connector.
type Server struct {
	venuepb.UnimplementedVenueAdapterServiceServer

	venue  execution.Venue
	closer execution.Closer
	view   orderview.Store
	closes execution.CloseTracker
	proof  execution.AccountProof
	logger *slog.Logger
	// halted is the platform kill-switch (#635). THIS IS THE LAST LINE: everything
	// upstream of it — the gateway's 423, the OMS refusing admission — can be
	// bypassed by a path that does not go through them, and this cannot, because
	// there is no other way an order of ours reaches an exchange.
	//
	// A NIL GATE IS HALTED, so an adapter built without one refuses to place
	// anything. That is the safe direction and it is why New takes it positionally.
	halted *halt.Gate
}

// New returns a Server fronting venue. closes is the in-flight-close registry the
// connector's healing watchdog drains — it lives in THIS process now (before the
// split, the OMS wrote it and the connector read it through a shared pointer).
//
// proof is what the EXCHANGE said about this adapter's credential at startup, and
// it is a required argument rather than an option because its zero value is the
// honest one: an adapter that never proved its account reports an unverified claim,
// and the OMS can tell the difference. An optional proof would default to "trust
// me" in exactly the deployments nobody remembered to configure.
// gate is the platform kill-switch, and it is REQUIRED in the same sense proof
// is: its zero value is the honest one. A nil gate answers halted, so an adapter
// wired without one refuses to place orders rather than placing them unbraked.
func New(venue execution.Venue, view orderview.Store, closes execution.CloseTracker, proof execution.AccountProof, gate *halt.Gate, logger *slog.Logger) *Server {
	s := &Server{venue: venue, view: view, closes: closes, proof: proof, halted: gate, logger: logger}
	// A venue that cannot cancel AT the exchange would silently downgrade every
	// cancel to a ledger-only entry while the order stays live. BinanceVenue is a
	// Closer; assert it rather than discover otherwise in production.
	if c, ok := venue.(execution.Closer); ok {
		s.closer = c
	}
	return s
}

// Execute works the order and returns its fills.
//
// The order is recorded in the adapter's own view BEFORE it is worked. That order
// matters: the fill for a marketable order can arrive on the user-data websocket
// before this RPC even returns, and the ingester enriches it by reading exactly
// this view. Record after, and the first fill of a fast order finds nothing.
func (s *Server) Execute(ctx context.Context, req *venuepb.ExecuteRequest) (*venuepb.ExecuteResponse, error) {
	// THE PLATFORM KILL-SWITCH, ON THE LAST LINE BEFORE THE EXCHANGE (#635).
	//
	// BEFORE view.Record, deliberately: an order this adapter will not place must
	// not be written into the view as one it has, or the reconciler and the
	// enrichment path both start believing in an order the exchange never saw.
	//
	// WHY IT IS HERE AND NOT ONLY AT ADMISSION. The OMS refuses NEW orders while
	// halted, which covers everything a human or a strategy submits. It does not
	// cover what the OMS does to orders it ALREADY admitted: the sweep re-drives
	// an interrupted order, resume() re-works one after a crash, and a scheduled
	// parent slices children on its own timer. Each of those reaches an exchange
	// without passing through admission again. This check is what makes "no new
	// exposure while halted" true of those paths too.
	//
	// FailedPrecondition, not Unavailable: the OMS must not read this as a venue
	// outage to heal around. The order is left ROUTED with no venue ack, which is
	// exactly the state resume() knows how to recover once an operator resumes the
	// platform — it asks the exchange what happened, finds nothing, and re-drives.
	//
	// CancelOrder is deliberately NOT gated. See halt.Gate for the boundary.
	if reason, halted := halt.Refusal(s.halted); halted {
		s.logger.Warn("venue: refusing to place an order — platform halted",
			"mic", s.venue.MIC(), "order_id", req.GetState().GetOrderId(), "reason", reason)
		return nil, status.Errorf(codes.FailedPrecondition, "venue: %s", reason)
	}
	if err := requireDecimalDomain(req); err != nil {
		return nil, err
	}
	st := req.GetState()
	if st.GetOrderId() == "" {
		return nil, status.Error(codes.InvalidArgument, "venue: order_id is required")
	}
	// AND REFUSE AN ORDER THIS ADAPTER HAS ALREADY SEEN THE VENUE FINISH (#914).
	//
	// BEFORE view.Record, for the reason the halt gate above is: an order this
	// adapter will not place must not be written into the view as one it has.
	//
	// THE VIEW IS THE SMALLER HALF, and it is the half the issue was filed for.
	// Record is a plain upsert, so a re-dispatch of an order the view holds
	// FILLED overwrote it back to the OMS's ROUTED. Memory.Record stamps
	// terminalAt only for a terminal status, so the entry lost its eviction
	// clock, Open started returning it again, and the reconciler resumed
	// spending REST weight on it and re-emitting StateHealed about it on every
	// pass — the leak Progress closed, reopened through the other writer.
	//
	// THE BIGGER HALF IS WHAT THE LINE AFTER THE RECORD WOULD DO: place, at the
	// exchange, an order the venue has already finished. Both connectors stamp
	// order_id as the client order id (see orderid.Valid for why the id shape is
	// the intersection of the venues), so the exchange dedups a resubmission —
	// WHILE THE ORIGINAL IS STILL OPEN. Once it has filled, that id is free
	// again, and the idempotency this platform leans on is weakest in exactly
	// the case this guard covers. Nothing downstream catches it either:
	// BinanceVenue.Execute's recovery branch runs only when the placement
	// FAILED, and a second placement that succeeds returns real fills for a real
	// second trade.
	//
	// NO OMS PATH REACHES THIS TODAY, and the guard is worth its cost anyway.
	// Service.work is the only caller of Venue.Execute, and each of its three
	// callers either provably precedes the first placement (admission, and
	// resume's PENDING_NEW branch, which is reachable only because work saves
	// ROUTED before it calls the venue) or goes through Reconcile, which returns
	// ActionRedrive only when the venue itself answered UNKNOWN. This is the
	// last line before the exchange; it is the wrong place to hold an invariant
	// by knowing what every caller currently does.
	//
	// REFUSE RATHER THAN WORK-IT-AND-KEEP-THE-STATUS, which was the other option
	// on the table. Keeping the status fixes the bookkeeping and leaves the
	// duplicate placement, and it is quiet: the OMS would be told the order was
	// worked and given whatever the second placement returned. Refusing costs a
	// nack and, past the venue adapter, a quarantine or a DLQ entry for an order
	// that did in fact finish — a human looking at something that needs no
	// repair. That is the SimVenue.QueryOrder trade taken the same way round: a
	// false freeze against a second trade, fail closed.
	//
	// AN ID IS NOT REUSED ACROSS ORDERS, so this cannot refuse a different
	// order: orderid.Mint is 128 bits and Store.Create is an admission gate that
	// refuses a duplicate order_id.
	//
	// THE RESIDUAL, STATED RATHER THAN GLOSSED. Memory forgets a terminal order
	// after DefaultTerminalRetention, so past that window Execute sees no entry
	// and proceeds; Postgres never prunes and the guard is permanent there. The
	// SimVenue.forgotten latch is deliberately NOT copied here, for the reason
	// stated there: Execute cannot tell a forgotten order from a brand-new one,
	// so refusing every miss would not bound anything, it would stop the adapter
	// dead.
	if err := s.refuseFinished(ctx, st.GetOrderId()); err != nil {
		return nil, err
	}
	// AND THE RECORD MERGES ONTO THE VIEW RATHER THAN REPLACING IT (#944).
	//
	// The guard above refuses a TERMINAL prior. It returns nil for every other
	// one, so a re-dispatch of an order the view holds PARTIALLY_FILLED passed it
	// and the plain Store.Record that used to be on this line wrote the OMS's
	// OrderState over the venue's own filled_quantity and leaves_quantity. The
	// OMS's copy is behind the venue BY CONSTRUCTION here: this adapter learns of
	// a fill on the exchange's websocket and the OMS learns of it from this
	// adapter, so the state arriving on an ExecuteRequest cannot be fresher than
	// the one already in the view. orderview.Dispatch keeps the five fields the
	// exchange is the authority on and takes the OMS's terms, which is the same
	// rule recordStatus follows on the cancel path — one rule for the view, not
	// two — through the same atomic read-decide-write (#934).
	//
	// BOUNDED HONESTLY, BECAUSE IT WAS NOT A LOST FILL. The status the old write
	// left behind was the OMS's ROUTED, which is not terminal, so Open kept
	// returning the order and the healing watchdog re-queried the exchange and
	// healed the quantities next pass; the fill FACTs are built from the report,
	// never from this view. What did not self-correct is venue_orders, which is
	// never pruned: until that pass this adapter's answer to "what did the venue
	// report filled" was the OMS's stale guess, and Seam.Lookup hands it to the
	// user-data ingester enriching any execution report arriving in the window.
	//
	// IT COSTS ONE MORE READ OF THE VIEW than the upsert did, on the order path,
	// because refuseFinished's Get and this one are still two operations. That is
	// a check-then-act: a fill landing between them leaves a terminal order the
	// guard has already waved through, and the placement goes out. The RECORD
	// survives it — the merge carries the venue's verdict forward rather than
	// writing ROUTED over it — so what remains is #914's window, not this one.
	// Folding the two reads into this single Update is the repair, and #947
	// carries it.
	if err := orderview.Dispatch(ctx, s.view, st); err != nil {
		// Not a soft failure, and that includes orderview.ErrContended: working an
		// order we have no record of leaves its fills unenrichable and invisible to
		// the reconciler — better to refuse it and let the OMS see the error than
		// to trade blind.
		s.logger.Error("venue: could not record order before working it", "mic", s.venue.MIC(), "order_id", st.GetOrderId(), "err", err)
		return nil, status.Errorf(codes.Internal, "venue: record order %s: %v", st.GetOrderId(), err)
	}

	fills, err := s.venue.Execute(ctx, st)
	if err != nil {
		// Report the failure. NEVER synthesize a fill to paper over it — a
		// fabricated fill books a trade that never happened.
		return nil, status.Errorf(codes.Unavailable, "venue: execute order %s: %v", st.GetOrderId(), err)
	}
	// Zero fills is a valid answer: a resting limit order that did not trade. It
	// is NOT an error, and turning it into one would make the OMS treat a working
	// order as a failed one.
	return &venuepb.ExecuteResponse{Fills: fills}, nil
}

// CancelOrder withdraws the order at the exchange.
//
// It Tracks the close BEFORE dispatching it. That is the In-Flight Certainty seam
// (EXEC-M4c): if the cancel times out or the answer is ambiguous, the intent is
// already registered and the healing watchdog will resolve it against the
// exchange. Tracking only on success would lose exactly the cases the watchdog
// exists for — the ambiguous ones.
//
// An OK response means the venue CONFIRMED the withdrawal. Any error means the
// close is still in flight. There is no third answer, which is why
// CancelOrderResponse has no fields.
func (s *Server) CancelOrder(ctx context.Context, req *venuepb.CancelOrderRequest) (*venuepb.CancelOrderResponse, error) {
	if err := requireDecimalDomain(req); err != nil {
		return nil, err
	}
	st := req.GetState()
	if st.GetOrderId() == "" {
		return nil, status.Error(codes.InvalidArgument, "venue: order_id is required")
	}
	if s.closer == nil {
		return nil, status.Error(codes.Unimplemented, "venue: this venue cannot withdraw an order at the exchange")
	}

	if s.closes != nil {
		s.closes.Track(execution.CloseIntent{OrderID: st.GetOrderId()})
	}
	if err := s.closer.CancelOrder(ctx, st); err != nil {
		// The close stays tracked and in flight. The watchdog owns it now. Do NOT
		// resolve it here — an error is precisely the case where we do not know.
		return nil, status.Errorf(codes.Unavailable, "venue: cancel order %s: %v", st.GetOrderId(), err)
	}
	// Confirmed at the venue.
	if s.closes != nil {
		s.closes.Resolve(st.GetOrderId())
	}
	if err := s.recordStatus(ctx, st, orderpb.OrderStatus_ORDER_STATUS_CANCELLED); err != nil {
		// The cancel landed; only our local view is stale. Log it — do not fail the
		// RPC, or the OMS would retry a cancel that already succeeded.
		s.logger.Warn("venue: cancel confirmed but order view not updated", "mic", s.venue.MIC(),
			"order_id", st.GetOrderId(), "err", err)
	}
	return &venuepb.CancelOrderResponse{}, nil
}

// QueryOrder asks the connector what the EXCHANGE did with one order, and
// serves the answer to the OMS (#920).
//
// # This is the RPC that makes crash recovery possible against a real venue
//
// The OMS's resume path asks the venue what became of an interrupted order and
// acts on the answer. Until this existed, no out-of-process adapter could be
// asked, so every interrupted ROUTED order in a real deployment was frozen for
// a human. Both connectors already held the answer behind a private queryOrder
// their own reconcilers call; this surfaces it, and nothing about the exchange
// protocol crosses the boundary — the REST call stays inside the connector, in
// this process, behind the credential that never leaves it.
//
// # THE ERROR RETURN AND THE UNKNOWN VERDICT ARE DIFFERENT ANSWERS
//
// An error means the question could not be asked. UNKNOWN means the exchange
// positively stated it has no such order, and the OMS is entitled to place the
// order again on the strength of it. So a rate-limit refusal is
// ResourceExhausted and every other failure is Unavailable; neither can reach
// the OMS as a verdict, because a verdict only travels on the OK path.
//
// A CONNECTOR THAT IS NOT A Querier ANSWERS Unimplemented, which GRPCVenue reads
// as INDETERMINATE — the same quarantine the OMS performed before this RPC
// existed. It is not an error the OMS should retry: no amount of asking again
// will make a connector able to ask its exchange.
//
// # NOT GATED BY THE PLATFORM HALT, AND NOT ANSWERED FROM THE LOCAL VIEW
//
// Execute is halt-gated because it moves capital. This is a read, and it is the
// read an operator most needs WHILE the platform is stopped — the same boundary
// CancelOrder sits on.
//
// It also does not answer from s.view, which would be free and would be wrong.
// The view is this adapter's own record, seeded by Execute; a MISS in it is not
// the exchange stating it has no such order, it is this process not having
// written one — exactly the distinction UNKNOWN must never blur. The exchange is
// the authority on what the exchange holds, so the exchange is who gets asked.
func (s *Server) QueryOrder(ctx context.Context, req *venuepb.QueryOrderRequest) (*venuepb.QueryOrderResponse, error) {
	if err := requireDecimalDomain(req); err != nil {
		return nil, err
	}
	st := req.GetState()
	if st.GetOrderId() == "" {
		return nil, status.Error(codes.InvalidArgument, "venue: order_id is required")
	}
	q, ok := s.venue.(execution.Querier)
	if !ok {
		return nil, status.Errorf(codes.Unimplemented,
			"venue: the %s connector cannot ask the exchange what became of an order", s.venue.MIC())
	}
	view, err := q.QueryOrder(ctx, st)
	if err != nil {
		// ResourceExhausted names the weight budget specifically, because the two
		// demand different operator actions: an exhausted budget is this
		// platform's own back-pressure and clears itself, an Unavailable is the
		// exchange. Both are "could not ask", and NEITHER may become a verdict.
		if errors.Is(err, execution.ErrRateLimited) {
			s.logger.Warn("venue: could not ask the exchange about an order — weight budget exhausted",
				"mic", s.venue.MIC(), "order_id", st.GetOrderId())
			return nil, status.Errorf(codes.ResourceExhausted,
				"venue: could not ask %s about order %s: %v", s.venue.MIC(), st.GetOrderId(), err)
		}
		return nil, status.Errorf(codes.Unavailable,
			"venue: could not ask %s about order %s: %v", s.venue.MIC(), st.GetOrderId(), err)
	}
	return &venuepb.QueryOrderResponse{
		State:  execution.OrderViewStateProto(view.State),
		Fills:  view.Fills,
		Reason: view.Reason,
	}, nil
}

// Describe reports who this adapter is: the venue it trades, the exchange account
// its credential belongs to, and whether the EXCHANGE ITSELF confirmed that.
//
// The OMS calls it at dial time and refuses to start when the answer disagrees with
// its own configuration. That is the point: the account is the collateral boundary,
// and before this RPC existed the OMS could only believe a string in its own
// manifest — so a typo posted fills to one fund's ledger rows while the exchange
// debited another's, with nothing in the platform able to notice.
func (s *Server) Describe(context.Context, *venuepb.DescribeRequest) (*venuepb.DescribeResponse, error) {
	resp := &venuepb.DescribeResponse{
		Mic:               s.venue.MIC(),
		Account:           s.venue.Account(),
		AccountVerified:   s.proof.Verified,
		ExchangeAccountId: s.proof.ExchangeAccountID,
	}

	// AND WHICH ORDER TYPES IT CAN ACTUALLY PLACE (#405), for the same reason the
	// account is here: before this field the OMS could only believe order.v1's
	// enum, which declares four types while the spot connectors translate two.
	// A stop was admitted, announced and stored, and refused only inside Execute
	// — the OMS had no way to ask.
	//
	// A CONNECTOR THAT DOES NOT IMPLEMENT OrderTypeDeclarer LEAVES THIS EMPTY,
	// and empty means "did not say", never "supports nothing". The OMS names and
	// counts such an adapter at startup and refuses only under
	// OMS_REQUIRE_ORDER_TYPE_SUPPORT — because a field added to a schema must not
	// silently become a trading outage for an adapter that predates it.
	if d, ok := s.venue.(execution.OrderTypeDeclarer); ok {
		resp.SupportedOrderTypes = d.OrderTypes()
	}
	// AND THE SAME QUESTION FOR TIME-IN-FORCE (#486). Separately optional: an
	// adapter may declare one and not the other, and treating silence on either
	// as a refusal would be a trading outage caused by a schema addition.
	if d, ok := s.venue.(execution.TimeInForceDeclarer); ok {
		resp.SupportedTimeInForce = d.TimeInForce()
	}
	// AND WHICH COLLATERAL REGIMES IT CAN WORK AN ORDER UNDER (#417). Same
	// contract a third time, and this is the field that turns the platform's
	// leverage refusal from a hardcode into something a venue can answer for
	// itself: both spot connectors declare CASH only, so an order asking for
	// cross or isolated margin is refused at ADMISSION, naming the venue, rather
	// than by a blanket rule in internal/signal/translate that could never say
	// which venue could have taken it.
	if d, ok := s.venue.(execution.MarginModeDeclarer); ok {
		resp.SupportedMarginModes = d.MarginModes()
	}
	return resp, nil
}

// InstrumentLister is implemented by a connector that can enumerate the pairs it
// is configured to trade (#406).
type InstrumentLister interface {
	Instruments() []execution.InstrumentSymbol
}

// ListInstruments reports what this adapter can actually trade.
//
// Nothing on this platform could enumerate the tradeable set: datamaster answers
// GET /v1/securities/{id} and has no List, and the gateway proxies the same
// single-key reads. So an operator could not answer "what can this deployment
// trade?" without reading a manifest, and no UI could offer a choice of pairs.
//
// A CONNECTOR THAT CANNOT ENUMERATE RETURNS AN EMPTY LIST, not an error. It is
// the "did not say" case again, and the caller is told which venue answered so an
// aggregated list cannot silently drop a venue's entire catalogue and look merely
// short.
func (s *Server) ListInstruments(context.Context, *venuepb.ListInstrumentsRequest) (*venuepb.ListInstrumentsResponse, error) {
	resp := &venuepb.ListInstrumentsResponse{Mic: s.venue.MIC()}
	lister, ok := s.venue.(InstrumentLister)
	if !ok {
		s.logger.Warn("venue: adapter cannot enumerate its instruments — every pair-picking surface will show this venue as empty",
			"mic", s.venue.MIC())
		return resp, nil
	}
	for _, in := range lister.Instruments() {
		resp.Instruments = append(resp.Instruments, &venuepb.VenueInstrument{
			InstrumentId:  in.InstrumentID,
			VenueSymbol:   in.VenueSymbol,
			BaseAsset:     in.Pair.Base,
			QuoteAsset:    in.Pair.Quote,
			QuoteMismatch: in.QuoteMismatch,
		})
	}
	return resp, nil
}

// refuseFinished refuses to work an order this adapter's view already holds at a
// terminal status. See the call site in Execute for the argument.
//
// A STORE FAILURE REFUSES TOO. It leaves this adapter unable to establish that
// the order is not already finished, and an unknown on the capital path fails
// closed — the same direction Execute takes when the Record itself fails.
func (s *Server) refuseFinished(ctx context.Context, orderID string) error {
	prior, _, ok, err := s.view.Get(ctx, orderID)
	if err != nil {
		s.logger.Error("venue: could not read the order view before working an order",
			"mic", s.venue.MIC(), "order_id", orderID, "err", err)
		return status.Errorf(codes.Internal,
			"venue: could not establish whether order %s has already been worked: %v", orderID, err)
	}
	if !ok || !orderview.Terminal(prior.GetStatus()) {
		return nil
	}
	// AlreadyExists, not the halt gate's FailedPrecondition: an operator reading
	// a refusal has to be able to tell "the platform is stopped" from "the
	// exchange already finished this order", and those demand different actions.
	s.logger.Warn("venue: refusing to place an order the venue has already finished",
		"mic", s.venue.MIC(), "order_id", orderID, "status", prior.GetStatus().String())
	return status.Errorf(codes.AlreadyExists,
		"venue: order %s is already %s in this adapter's view — the exchange finished it, and "+
			"placing it again would trade the fund twice", orderID, prior.GetStatus())
}

// recordStatus writes the outcome of a venue-confirmed close into this adapter's
// own view WITHOUT letting the OMS's copy of the order overwrite what the venue
// itself already reported (#921).
//
// THE RACE THAT MAKES THIS REACHABLE IS NOT HYPOTHETICAL. A confirmed cancel is
// the venue saying the order is NO LONGER WORKING. It is not the venue saying the
// order did not trade, and on this path the two are routinely the same answer:
// BinanceVenue.CancelOrder maps Binance's -2011 "Unknown order sent" to a
// CONFIRMED withdrawal precisely because the order may have "already filled,
// expired, or withdrawn by an earlier attempt". So a cancel that races a fill
// returns success, and a plain Record here wrote CANCELLED — together with the
// OMS's quantities, which are behind the venue by construction — over a FILLED
// verdict and the filled/leaves quantities orderview.Progress had just folded in
// from the exchange's own execution report.
//
// WHAT THAT COSTS, BOUNDED HONESTLY, BECAUSE IT IS NOT #904's LEAK. The
// reconciler is unaffected: both statuses are terminal, so Open still excludes
// the order. Eviction is unaffected: Memory keeps the FIRST terminal sighting as
// the retention clock. What it costs is the record itself. venue_orders is never
// pruned, so in the durable store the overwrite is permanent, and this adapter's
// answer to "what did the venue do with this order" — the attribution the
// platform owes every order, and the state Seam.Lookup hands the user-data
// ingester to enrich a late execution report — stops being the exchange's report
// and becomes the OMS's stale guess.
//
// THE DECISION: A TERMINAL VERDICT ALREADY IN THE VIEW IS NEVER OVERWRITTEN, the
// shape #914 gave Execute. Three alternatives were on the table.
//
//   - Merge, and still take the cancel's status. Keeps the numbers and writes a
//     verdict the cancel never established: a record reading CANCELLED with
//     filled equal to ordered and leaves zero, which is harder to act on than
//     either honest answer, and which still loses what the venue returned.
//   - Merge, and refuse only to downgrade FILLED. The right instinct with a
//     hand-written membership test — and the -2011 comment names EXPIRED in the
//     same breath as filled, so the hand-written set is already short one member
//     on the day it is written. The set that matters is orderview.Terminal's, so
//     this asks it rather than keeping a second copy of it here.
//   - orderview.Progress. Wrong tool twice: terminal → terminal is an ALLOWED
//     transition there, deliberately, so it would still move FILLED to CANCELLED;
//     and it merges FROM the reported state, which on this path is the OMS's
//     record — the thing that must not win.
//
// A NON-TERMINAL ORDER IS MERGED ONTO THE VIEW'S OWN ENTRY, not onto the
// request's state, and that half is not cosmetic: the view is where the venue's
// partial-fill quantities live, so recording the OMS's copy over a
// PARTIALLY_FILLED entry erases a fill that DID happen just as surely as
// overwriting a terminal one. Only when this adapter has NO record does the
// request seed one — that is the full OrderState the OMS sent, carrying the
// order's terms, not the partial reconstruction Progress refuses with
// ErrNotInView.
//
// IT DOES NOT REFUSE THE RPC THE WAY Execute DOES, and the asymmetry is the
// point. refuseFinished runs BEFORE the exchange is touched, so refusing there
// prevents a second trade. This runs AFTER the venue confirmed the withdrawal;
// failing here would tell the OMS to retry a cancel that already succeeded.
//
// A VIEW THAT CANNOT BE READ WRITES NOTHING. Not knowing what the venue already
// reported is a critical unknown, and it fails closed: the entry is left alone,
// so the order stays non-terminal, Open keeps returning it, and the healing
// watchdog re-reads venue truth on the next pass — a cost that self-corrects. The
// alternative is a store blip permanently replacing the exchange's verdict with
// the OMS's, which nothing corrects.
//
// THE READ AND THE WRITE ARE ONE OPERATION (#934). They were not: this was a
// plain Get followed by a plain Record, and a fill landing between the two was
// written over by a merge built on the pre-fill read — the same erasure #921
// closed, narrowed from a certainty to the width of one window rather than shut.
// orderview.Update now applies the write only while the view still holds the
// exact value the decision was made against, and RE-DECIDES when it does not,
// which is the half that matters: a retry that re-applied the old decision would
// write CANCELLED over the FILLED the racing writer had just established, and
// a conditional write that simply gave up would drop a cancel that must land.
// Re-deciding against a now-terminal view means keeping the venue's verdict and
// writing nothing, which is this function's own rule reached a second time.
//
// THE CONDITION IS THE VALUE, NOT THE STATUS, and that is not a detail. A cancel
// always writes a status CHANGE, so a compare-and-set on the status catches the
// interleaving where the racing writer also moved the status — and misses the
// ordinary one, where a partially filling order takes another fill and the venue
// writes PARTIALLY_FILLED over PARTIALLY_FILLED with a larger filled_quantity.
// The status predicate holds, the merge is still built on the pre-fill read, and
// the quantity the exchange reported is lost with a green test beside it.
//
// THE RESIDUAL. Update gives up after orderview.UpdateAttempts contended rounds
// and returns ErrContended — a refusal, not a silent overwrite. The caller logs
// it and the RPC still succeeds, so the view keeps the venue's own report and
// stays non-terminal, and the healing watchdog re-reads venue truth next pass.
func (s *Server) recordStatus(ctx context.Context, st *orderpb.OrderState, next orderpb.OrderStatus) error {
	orderID := st.GetOrderId()
	return orderview.Update(ctx, s.view, orderID, func(prior *orderpb.OrderState, found bool) (*orderpb.OrderState, error) {
		base := st
		if found {
			if orderview.Terminal(prior.GetStatus()) {
				s.logger.Info("venue: close confirmed for an order the venue had already finished — keeping the venue's own verdict",
					"mic", s.venue.MIC(), "order_id", orderID, "status", prior.GetStatus().String())
				return nil, nil
			}
			base = prior
		}
		cloned, cok := proto.Clone(base).(*orderpb.OrderState)
		if !cok {
			return nil, fmt.Errorf("order %s did not clone", orderID)
		}
		cloned.Status = next
		return cloned, nil
	})
}
