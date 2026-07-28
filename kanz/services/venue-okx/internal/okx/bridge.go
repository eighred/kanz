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
	// ExpectedOrders is what this adapter believes is open at the venue.
	ExpectedOrders = execution.ExpectedOrders
	// ExpectedBalances is the per-asset balance the reconciler compares against.
	ExpectedBalances = execution.ExpectedBalances
	// UserDataStream is the private-websocket transport seam (tests inject a fake).
	UserDataStream = execution.UserDataStream

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

	// WorkerDeps is what the composition root hands the background workers.
	WorkerDeps = execution.WorkerDeps

	// Vendor-neutral exchange plumbing.
	VenueSettings   = execution.VenueSettings
	SymbolMapper    = execution.SymbolMapper
	StaticSymbolMap = execution.StaticSymbolMap
	APIError        = execution.APIError
	WeightBucket    = execution.WeightBucket
)

var (
	// The connector refuses to fire a request it cannot afford, and NEVER
	// fabricates a fill to cover for it.
	ErrRateLimited  = execution.ErrRateLimited
	ErrEgressDenied = execution.ErrEgressDenied

	NewWeightBucket       = execution.NewWeightBucket
	NewExchangeHTTPClient = execution.NewExchangeHTTPClient
	NewCloseRegistry      = execution.NewCloseRegistry

	// Exact base-10 helpers. Money and sizes are big.Rat-backed common.v1.Decimal;
	// `double` is banned on any path moving capital, and these are the only way a
	// decimal crosses to or from an exchange's string wire format.
	Sleep     = execution.Sleep
	CapDur    = execution.CapDur
	FormatDec = execution.FormatDec
	ParseDec  = execution.ParseDec
	SubDec    = execution.SubDec
)

// Subjects both exchange reconcilers publish healing FACTs on — shared so the
// OMS's consumers see one subject regardless of which venue healed.
const (
	SubjectStateHealed  = execution.SubjectStateHealed
	SubjectBalanceRecon = execution.SubjectBalanceRecon
)
