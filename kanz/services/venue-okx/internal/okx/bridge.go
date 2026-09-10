package okx

// The seams this connector consumes from the shared EMS library
// (internal/execution). Nothing else in internal/execution is used, so this file
// IS the connector's dependency surface — if it grows, the coupling grew.
//
// It mirrors venue-binance's bridge exactly, which is the point: after INFRA-M7a,
// an exchange adapter is a connector plus a thin, identical shell. The parts that
// are the SAME for every venue (the order view, the gRPC face, the probes) live in
// internal/venueadapter and are shared; the parts that differ (signing, symbols,
// the shape of an execution report) stay in here, in their own process.
//
// Aliases, not wrappers: these are the same types, so a *bus.Producer still
// satisfies Publisher and the FACTs this connector publishes are the same FACTs
// the OMS already consumes.

import "github.com/eighred/kanz/internal/execution"

// Worker seams — supplied by the venue-okx composition root.
type (
	// Publisher is the bus publish surface. The user-data stream and the reconciler
	// publish fill and StateHealed FACTs through it — the ASYNC half of the venue
	// contract, which does not go over venue.v1 gRPC at all.
	Publisher = execution.Publisher
	// OrderLookup enriches an exchange execution report (which carries only the
	// clOrdId) with Kanz order context. Backed by THIS adapter's own order view.
	OrderLookup = execution.OrderLookup
	// OrderTracker is that same view's WRITE half alongside it (#904): the
	// user-data ingester advances an order here when the venue reports it
	// filled, so a filled order goes terminal in the adapter's own view instead
	// of being re-queried and re-healed on every reconciliation pass forever.
	OrderTracker = execution.OrderTracker
	// ExpectedOrders is what this adapter believes is open at the venue.
	ExpectedOrders = execution.ExpectedOrders
	// ExpectedBalances is the per-asset balance the reconciler compares against.
	ExpectedBalances = execution.ExpectedBalances
	// The venue-margin seam (#408). Satisfied by this connector's own signed REST
	// client — margin state is a READ OF OKX, never a reconstruction of its
	// maths from Kanz's positions.
	VenueMarginSource   = execution.VenueMarginSource
	VenueMargin         = execution.VenueMargin
	VenuePositionMargin = execution.VenuePositionMargin
	SupportStatus       = execution.SupportStatus
	// UserDataStream is the private-websocket transport seam (tests inject a fake).
	UserDataStream = execution.UserDataStream

	// ReportRefusal is the SHARED answer to an execution report this connector
	// will not publish as a fill (#1045): the counter, the ERROR log and the
	// freeze on this adapter's own order view, in one place for both venues. The
	// bound that produces the refusal is shared for the same reason — the defect
	// was in both connectors because the arithmetic was written in both.
	ReportRefusal = execution.ReportRefusal

	// UserDataBackoff is the SHARED re-dial policy for the private user-data run
	// loop (#1047). Its reset is tied to an execution report actually resolved
	// rather than to a successful connect, which is what stops an order-view
	// outage becoming an exchange rate-limit and then an IP ban.
	UserDataBackoff = execution.UserDataBackoff

	// MarkTickPublisher is the shared reference-mark tick publisher (#673). The
	// ticker feed does not build the market.crypto.trade envelope itself and does
	// not see the publish error: both connectors share one implementation, so a
	// repair to either cannot land on only one venue.
	MarkTickPublisher = execution.MarkTickPublisher

	// The In-Flight Certainty seam. The registry is LOCAL to this process now: the
	// gRPC CancelOrder handler writes it, this connector's healing watchdog reads
	// it. Before the split the OMS wrote it through an in-process pointer — and
	// that sharing was exactly what let THIS reconciler try to heal a Binance
	// order (see execution.SelfHealing).
	PendingCloses = execution.PendingCloses
	CloseIntent   = execution.CloseIntent
	CloseRegistry = execution.CloseRegistry
	CloseTracker  = execution.CloseTracker

	// The EMS interfaces OKXVenue satisfies — unchanged by crossing a process
	// boundary.
	Venue  = execution.Venue
	Closer = execution.Closer
	// Querier is the READ half the OMS's crash recovery needs (#920): "do you
	// hold this order, and what did you do with it?". The gRPC face serves it,
	// and OKXVenue implements it over the same private queryOrder /
	// queryAlgoOrder this connector's own reconciler has always used.
	Querier = execution.Querier
	// OrderView is that answer, and OrderViewState is its verdict. THE ZERO VALUE
	// IS INDETERMINATE, deliberately — see execution.OrderViewState. The
	// dangerous value is UNKNOWN, which authorizes placing the order again.
	OrderView      = execution.OrderView
	OrderViewState = execution.OrderViewState

	// WorkerDeps is what the composition root hands the background workers.
	WorkerDeps = execution.WorkerDeps

	// Vendor-neutral exchange plumbing.
	VenueSettings    = execution.VenueSettings
	SymbolMapper     = execution.SymbolMapper
	StaticSymbolMap  = execution.StaticSymbolMap
	InstrumentSymbol = execution.InstrumentSymbol
	APIError         = execution.APIError
	WeightBucket     = execution.WeightBucket
)

const (
	SupportUnknown     = execution.SupportUnknown
	SupportSupported   = execution.SupportSupported
	SupportUnsupported = execution.SupportUnsupported
)

var (
	// The connector refuses to fire a request it cannot afford, and NEVER
	// fabricates a fill to cover for it.
	ErrRateLimited  = execution.ErrRateLimited
	ErrEgressDenied = execution.ErrEgressDenied

	NewWeightBucket = execution.NewWeightBucket
	// sleep/capDur are GONE from this surface (#1047). The connectors no longer
	// reach for the raw wait-and-cap primitives: the only thing that ever
	// composed them was the user-data reconnect delay, and that decision now
	// lives in execution.UserDataBackoff where one policy governs both venues and
	// both of their failure paths. Reaching for them again here would be a second
	// re-dial policy, which is how the venue-safety bound came to differ from the
	// connect-failure bound in the first place.
	NewExchangeHTTPClient = execution.NewExchangeHTTPClient
	NewCloseRegistry      = execution.NewCloseRegistry

	NewMarkTickPublisher = execution.NewMarkTickPublisher

	// The two DROP reasons an execution report carries when this connector could
	// not RESOLVE it to an order it holds (#1047) — aliased as values, like the
	// venue verdicts, so a connector cannot invent a third spelling of
	// "store_error" and split the series it is meant to be alerted on.
	DropUnknownOrder = execution.DropUnknownOrder
	DropStoreError   = execution.DropStoreError

	// The user-data reconnect policy, shared so one venue cannot be fixed alone
	// (#1047). Its reset is tied to a resolved execution report rather than to a
	// successful connect, which is what stops a store outage becoming an
	// exchange-side rate-limit or IP ban.
	NewUserDataBackoff     = execution.NewUserDataBackoff
	NewUserDataBackoffWith = execution.NewUserDataBackoffWith

	// The venue verdicts. Aliased as values rather than re-declared so a
	// connector cannot invent a ninth answer, and so "which constant is UNKNOWN"
	// has exactly one definition on the platform.
	OrderViewIndeterminate   = execution.OrderViewIndeterminate
	OrderViewUnknown         = execution.OrderViewUnknown
	OrderViewWorking         = execution.OrderViewWorking
	OrderViewPartiallyFilled = execution.OrderViewPartiallyFilled
	OrderViewFilled          = execution.OrderViewFilled
	OrderViewRejected        = execution.OrderViewRejected
	// The two TERMINAL WITHDRAWN verdicts (#924): the venue pulled the order, or
	// its time in force elapsed. Both may carry fills — whatever traded before the
	// withdrawal — and the OMS ADOPTS both rather than quarantining.
	OrderViewCancelled = execution.OrderViewCancelled
	OrderViewExpired   = execution.OrderViewExpired

	// Exact base-10 helpers. Money and sizes are big.Rat-backed common.v1.Decimal;
	// `double` is banned on any path moving capital, and these are the only way a
	// decimal crosses to or from an exchange's string wire format.
	FormatDec = execution.FormatDec
	ParseDec  = execution.ParseDec

	// THERE IS NO BARE SUBTRACTION ALIAS HERE ANY MORE, and that is the repair
	// rather than tidying (#1045). execution.SubDec was how this connector
	// computed leaves off an execution report, and it is not a bound: it
	// represents a negative result and reports success. Leaving the alias in
	// place would leave the next author one identifier away from writing the
	// defect again, beside a helper whose whole purpose is to stop it.
	//
	// LeavesRemaining is the BOUND between what this platform ordered and what
	// the venue says it filled, and the only way this connector computes leaves
	// off an execution report (#1045). Aliased rather than re-derived here for
	// the reason every other decimal helper is: a subtraction written per
	// connector is a bound implemented per connector, and this one was missing
	// from both.
	LeavesRemaining = execution.LeavesRemaining

	// The two refusals LeavesRemaining answers with. Aliased as values so this
	// connector cannot invent a third meaning for "the venue's number does not fit".
	ErrVenueOverfill         = execution.ErrVenueOverfill
	ErrLeavesUnrepresentable = execution.ErrLeavesUnrepresentable
)

// Subjects both exchange reconcilers publish healing FACTs on — shared so the
// OMS's consumers see one subject regardless of which venue healed.
const (
	SubjectStateHealed  = execution.SubjectStateHealed
	SubjectBalanceRecon = execution.SubjectBalanceRecon
)
