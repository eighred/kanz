package binance

// The seams this connector consumes from the shared EMS library
// (internal/execution). Nothing else in internal/execution is used, so this file
// IS the connector's dependency surface — if it grows, the coupling grew.
//
// It exists because the connector moved OUT of the OMS (INFRA-M7a-2). It used to
// live in package execution and could name these directly; now it lives in its
// own service and its own process, and reaches back only for the vendor-NEUTRAL
// parts: the bus publish seam, the order-context lookups its reconciler needs,
// the in-flight-close registry, the weight-budget rate limiter, and the exact
// decimal helpers. No exchange-specific code moved the other way — that is the
// whole point of the split.
//
// Aliases, not wrappers: these are the same types, so a *bus.Producer still
// satisfies Publisher and the FACTs this connector publishes are the same FACTs.

import "github.com/kanz-eng/kanz/internal/execution"

// Worker seams — supplied by the venue-binance composition root.
type (
	// Publisher is the bus publish surface. The connector's user-data stream and
	// reconciler publish fill and StateHealed FACTs through it — the ASYNC half of
	// the venue contract, which does not go over venue.v1 gRPC at all.
	Publisher = execution.Publisher
	// OrderLookup enriches an exchange execution report (which carries only the
	// clOrdId) with Kanz order context. Now backed by the ADAPTER'S OWN order view,
	// not the OMS store — the OMS is in another process.
	OrderLookup = execution.OrderLookup
	// ExpectedOrders is what this adapter believes is open at the venue. Also its
	// own view now.
	ExpectedOrders = execution.ExpectedOrders
	// ExpectedBalances is the per-asset balance the reconciler compares against.
	ExpectedBalances = execution.ExpectedBalances
	// UserDataStream is the private-websocket transport seam (tests inject a fake).
	UserDataStream = execution.UserDataStream

	// PendingCloses / CloseIntent / CloseRegistry: the In-Flight Certainty seam.
	// The registry is now LOCAL to this process — the gRPC CancelOrder handler is
	// the writer, this connector's healing watchdog is the reader. Before the
	// split, the OMS wrote and the connector read across an in-process pointer.
	PendingCloses = execution.PendingCloses
	CloseIntent   = execution.CloseIntent
	CloseRegistry = execution.CloseRegistry
	CloseTracker  = execution.CloseTracker

	// Venue / Closer: the EMS interfaces BinanceVenue satisfies. It still does —
	// the venue-binance gRPC server calls Execute and CancelOrder through them, so
	// the connector's contract is unchanged by having crossed a process boundary.
	Venue  = execution.Venue
	Closer = execution.Closer

	// WorkerDeps is what the composition root hands the background workers
	// (user-data stream, reconciler, ticker). venue-binance's main supplies these
	// now: its own bus producer, its own order view, its own close registry.
	WorkerDeps = execution.WorkerDeps

	// Vendor-neutral exchange plumbing.
	VenueSettings   = execution.VenueSettings
	SymbolMapper    = execution.SymbolMapper
	StaticSymbolMap = execution.StaticSymbolMap
	APIError        = execution.APIError
	weightBucket    = execution.WeightBucket
)

var (
	// ErrRateLimited / ErrEgressDenied: the connector refuses to fire a request it
	// cannot afford, and NEVER fabricates a fill to cover for it.
	ErrRateLimited  = execution.ErrRateLimited
	ErrEgressDenied = execution.ErrEgressDenied

	newWeightBucket       = execution.NewWeightBucket
	newExchangeHTTPClient = execution.NewExchangeHTTPClient

	// Used by the connector's own tests, which moved with it.
	NewCloseRegistry = execution.NewCloseRegistry

	// Exact base-10 helpers. Money and sizes are big.Rat-backed common.v1.Decimal;
	// `double` is banned on any path moving capital, and these are the only way a
	// decimal crosses to or from an exchange's string wire format.
	sleep     = execution.Sleep
	capDur    = execution.CapDur
	formatDec = execution.FormatDec
	parseDec  = execution.ParseDec
	subDec    = execution.SubDec
)

// Subjects both exchange reconcilers publish healing FACTs on — shared so the
// OMS's consumers see one subject regardless of which venue healed.
const (
	subjectStateHealed  = execution.SubjectStateHealed
	subjectBalanceRecon = execution.SubjectBalanceRecon
)
