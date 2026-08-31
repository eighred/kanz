package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"unicode"
)

// EVERY LONG-LIVED MAP OR SLICE IN THE ESTATE MUST HAVE AN EVICTOR
// (#805, #834, #814, #842).
//
// # What this is protecting
//
// Producer.sequence was keyed by {event_type, partition_key}, and partition_key
// ON THE ORDER PATH IS THE ORDER ID — the OMS stamps it in the one builder every
// order FACT goes through, and the api-gateway, both venue adapters and
// optimization do the same. One Producer per process, no evictor, roughly 2-6
// permanent entries per order the process had ever touched, for the life of the
// pod.
//
// It was the platform's highest-throughput long-lived map and it was monotonic.
// At institutional rates the OMS heap grows until the pod is OOM-killed — on the
// execution path that means orders in flight at an unknown state and a restart
// that has to reconcile them. It arrives as a memory eviction rather than an
// error, so nothing on the trading path reports it until the pod dies.
//
// The same shape then arrived one service out (#834): the api-gateway's quota
// middleware held a token bucket and an in-flight counter per PRINCIPAL and
// evicted from neither, on the only route that accepts an order. And again on
// the CONTROL plane (#814): the pre-trade gate's say-it-once ledger was keyed
// partly by the order's instrument id — a free-form string off SubmitOrder that
// nothing validates beyond "not empty" — so a caller entitled to one portfolio
// grew a permanent entry per INVENTED instrument, on every order the gate
// REFUSED, which cost its author nothing.
//
// # Why the guard is estate-wide, and what that changed (#842)
//
// This guard used to name three directories. Everything outside them was
// unguarded, and the tracker shows what that produced: #809 (tv-sync's in-RAM
// book), #810 (the compliance Monitor's books), #814, #834 and #844 are five
// separately-filed instances of ONE class, each fixed correctly, none of them
// stopping the next — because the thing that stops the next one is the guard,
// and the guard stopped at pkg/bus.
//
// A hand-written scope list is a copy of the estate, and a copy of the estate
// is the artifact that rots. So the walk is now the module, default-deny: every
// mutex-guarded map or slice field anywhere in kanz/ must be shrunk somewhere,
// or carry an entry below saying why it cannot grow — or, for the population
// that was already there when this widened, the issue that retires it.
//
// # The triage rule, and it is about the KEY rather than the map
//
// A map keyed by a portfolio, a currency, a venue or an operator-published
// record is bounded BY THE ESTATE: entries appear because something inside the
// system was configured or published. A map keyed by something a CALLER chooses
// — an instrument id off a request, a principal, an order id, a subject — is
// bounded by nothing, and it is the shape of #834 and #814. That distinction is
// the argument every exemption below has to make, and it is the one a scan
// cannot make for itself: `map[string]X` looks identical either way.
//
// # What this guard DOES NOT SEE, stated so a green run is not over-read
//
//  1. IT PROVES AN EVICTOR EXISTS, NOT THAT IT RUNS. This was measured rather
//     than reasoned about: deleting only the CALL SITE of internal/compliance's
//     sayOnce sweep left the pre-#842 guard green, and three unit tests caught
//     it instead. The reachability arm below now kills that mutation — it took
//     two goes, because with one estate-wide set of called names, web-bff's
//     unrelated session sweep() vouched for the compliance one. What the arm
//     still cannot see is a sweep that is CALLED but never reached: behind a
//     condition nobody satisfies, or on a ticker nothing starts. Whether an
//     evictor RUNS is a behavioural property and belongs in the owning
//     package's tests.
//  2. IT INSPECTS FIELDS, SO A COLLECTION INSIDE A VALUE IS INVISIBLE (#884).
//     MandateRegistry.byKey is map[key][]*Mandate: the KEY set is bounded by
//     published mandates and this guard is satisfied, while the SLICE at each
//     key grows once per mandate version. Widening the walk does not change
//     that — the field is still one field — and no arm below would notice.
//  3. LONG-LIVED IS APPROXIMATED BY "GUARDED BY A sync MUTEX". A long-lived map
//     reached from one goroutine only, or guarded by a channel, a RWMutex behind
//     an embedded type, or an atomic, is not in the population at all.
//  4. ATTRIBUTION IS SYNTACTIC. A delete through a local variable is credited to
//     nothing, and the unattributed arm below only speaks up when that local's
//     field NAME collides with a tracked field — which is the case where a miss
//     could vouch for a leak.
//
// # Why it reads the AST with comments detached
//
// Three guards in this tree have already passed while asserting nothing, because
// a regex over raw source matched their own explanatory prose. The paragraphs you
// are reading cannot satisfy anything below: every file is parsed with mode 0.

// evictionScope is an ANCHOR, not the scope of the walk — the walk is the whole
// module. Each one names fields whose disappearance means the scan drifted
// rather than the estate got cleaner, so a broken walk cannot pass over an empty
// set.
type evictionScope struct {
	dir string
	// minFiles is a floor on non-test files parsed in that directory.
	minFiles int
	// mustFind are Type.field names the struct walk has to see.
	mustFind []string
	// mustEvict is a Type.field that has had an evictor since it was written. It
	// also proves receiver attribution works: it only resolves if the shrink site
	// was tied back to its own type.
	mustEvict string
}

var evictionScopes = []evictionScope{
	{
		dir:       "pkg/bus",
		minFiles:  5,
		mustFind:  []string{"Producer.sequence", "DedupWindow.expiry"},
		mustEvict: "DedupWindow.expiry",
	},
	{
		dir:       "services/api-gateway/internal/middleware",
		minFiles:  5,
		mustFind:  []string{"quota.inflight", "bucketSet.buckets", "replayCache.entries"},
		mustEvict: "replayCache.entries",
	},
	{
		dir:      "internal/compliance",
		minFiles: 8,
		mustFind: []string{"sayOnce.volatile", "MandateRegistry.byKey"},
		// MandateRegistry.rejected has had its evictor since Put was written, and
		// it predates this scope — so it proves receiver attribution here without
		// vouching for the field #814 repaired.
		mustEvict: "MandateRegistry.rejected",
	},
	{
		// A generic receiver: proves the walk unwraps Memory[T] rather than
		// reporting its delete as unattributable — which is what it did before
		// #842, on a store that holds pending dual-control proposals.
		dir:       "internal/dualcontrol/proposalstore",
		minFiles:  1,
		mustFind:  []string{"Memory.by"},
		mustEvict: "Memory.by",
	},
}

// evictionClass is WHY a collection may live without an evictor. There are only
// three admissible answers and they are not interchangeable — the class decides
// what else the guard demands of the entry.
type evictionClass int

const (
	// boundedByConstruction: every component of the key is written by the estate.
	// The entry is a claim about the KEY SPACE, not about today's size.
	boundedByConstruction evictionClass = iota
	// isTheStore: the collection IS an in-memory store's content, so evicting is
	// data loss rather than hygiene. Admissible ONLY where the package also
	// declares a durable sibling, which the guard checks rather than believes:
	// the bound is then the deployment posture (one replica, ephemeral, and
	// TestNoCompositionRootSilentlyFallsBackToAnInMemoryStore makes choosing it
	// loud), not an evictor this type could grow.
	isTheStore
	// deferredLeak: it IS unbounded, it is not fixed here, and an issue says so.
	// The guard demands the issue number. This class exists so the population
	// that predates the estate-wide walk is ENUMERATED rather than invisible —
	// it is a debt register, and the only correct direction for it is shorter.
	deferredLeak
)

type evictionExemption struct {
	class evictionClass
	// why names the key space and WHO WRITES IT. "It is small today" is not a
	// reason; "nothing a caller sends can create an entry" is.
	why string
	// issue retires the entry. Required for deferredLeak, meaningless otherwise.
	issue string
}

// mapEvictionExempt names a collection field ("pkgdir: Type.field") that lives
// without an evictor, and why.
//
// DEFAULT-DENY: a mutex-guarded map or slice field anywhere in the module, not
// listed here and not shrunk anywhere, fails.
//
// IT WAS ONCE EMPTY, AND THE FIRST DRAFT OF THIS GUARD IS WHY THAT MATTERS. It
// carried an invented entry for a field that does not exist, and the dead-entry
// arm caught it on the first run. An exemption nobody checks is worse than no
// exemption, because it reads as a decision somebody made.
var mapEvictionExempt = map[string]evictionExemption{
	// ---------------------------------------------------------------------
	// boundedByConstruction — the key space is written by the estate.
	// ---------------------------------------------------------------------
	// ADDED BY A CROSS-PR COLLISION, NOT BY EITHER PR BEING WRONG. This guard went
	// estate-wide (#842) and the write-path load harness landed (#865) within minutes
	// of each other, in parallel worktrees; neither branch could see the other, both
	// were green alone, and main was red on the merge of the second.
	"test/load/orderflow: ledger.seen": {boundedByConstruction,
		"keyed by the order id on the EXECUTION stream, which is retained for 24h — so the key " +
			"space is that window plus whatever the run itself submits, imposed by the stream's own " +
			"retention rather than by hope. The holder is a ONE-SHOT CLI: watchFacts replays the " +
			"backlog, the stages run, the report prints, the process exits. There is no long-lived " +
			"process for it to leak in. " +
			"IT MUST NOT BE FILTERED TO THIS RUN, which is the obvious repair and is wrong: " +
			"TestAnotherRunsFactsDoNotCountAsThisRunsAnnouncements pins that a foreign order is " +
			"FOLDED AND NOT COUNTED, because await must be able to resolve any id it is handed " +
			"while the outstanding depth counts only this run's tag. Dropping foreign ids at the " +
			"door breaks that test, and counting them makes the depth go negative on a re-run.", ""},
	"internal/cashview: View.byPF": {boundedByConstruction,
		"keyed by portfolio_id off an accounting PortfolioCashBalance announcement, which the ledger " +
			"publishes only for a portfolio it books for. A LEVEL, NOT A DELTA: each announcement " +
			"replaces the portfolio's entry, so repetition does not grow the map — only a new " +
			"portfolio does.", ""},
	"internal/compliance: sayOnce.stable": {boundedByConstruction,
		"every key's variable part is (tenant, portfolio). An order reaches the gate only after " +
			"order.delegatedAndEntitled, so a \"user:\" issuer can name only a portfolio the gateway " +
			"stamped from its verified principal, and the two machine issuers on order.order.submit " +
			"take theirs from a strategy signal or a rebalance proposal. The registry's keys are " +
			"narrower still: warnMisfiled and warnSystemFallback only form one for a portfolio that " +
			"ALREADY has a published mandate. Evicting here would be wrong rather than merely " +
			"unnecessary — an ungoverned portfolio is ungoverned all day, so a horizon would " +
			"re-announce it forever. The caller-keyed half of the same type is sayOnce.volatile, " +
			"which IS swept.", ""},
	"internal/compliance: MandateRegistry.byKey": {boundedByConstruction,
		"keyed by (tenant, portfolio) and written only by Put, whose only caller is the mandate " +
			"replay off a COMPACTED config subject. An entry exists because an operator published a " +
			"mandate for that portfolio; nothing a trading caller sends can create one. The KEY set " +
			"is what this guard sees — the []*Mandate at each key grows per version, which is #884 " +
			"and which no arm here can reach.", ""},
	"internal/compliance: MandateRegistry.tenantsByPortfolio": {boundedByConstruction,
		"same writer and same source as byKey — one entry per portfolio some tenant has published a " +
			"mandate for. It exists so a missed lookup can say WHY (#243), and it cannot outgrow the " +
			"set of published mandates.", ""},
	"internal/exchange/netdial: CachedDialer.cache": {boundedByConstruction,
		"keyed by the HOSTNAME half of the address being dialled, and every non-test caller dials a " +
			"venue REST or WS base out of that adapter's own config — one to three exchange hosts per " +
			"process. No request path reaches it with an address of its own choosing.", ""},
	"internal/execution: VenueCosts.stats": {boundedByConstruction,
		"keyed by venue MIC, stamped by the venue adapter on its own fills and folded through " +
			"costwatch. The key space is the platform's configured exchange vocabulary — a handful — " +
			"and the per-venue decision sample inside each entry is separately capped at minSamples.", ""},
	"internal/marketdata/mark: Source.prices": {boundedByConstruction,
		"keyed by instrument_id off market.*.trade / market.*.quote, and the market-data service only " +
			"streams MARKET_DATA_FEED_INSTRUMENTS — an operator-configured list. An expired entry is " +
			"TOMBSTONED rather than deleted on purpose (#96): the fact that an instrument was once " +
			"priced is what stops a stale mark being read as a fresh one, so a delete here would be a " +
			"regression rather than hygiene.", ""},
	"internal/marketedge/coverage: Recorder.state": {boundedByConstruction,
		"keyed by Series{instrument, venue} and written once per SUBSCRIPTION registered at startup " +
			"from cfg.Instruments, never per message and never per order. One entry per feed the " +
			"process was configured with.", ""},
	"internal/marketedge/volprofile: Store.series": {boundedByConstruction,
		"keyed by Series{instrument, venue} — one entry per feed this process has ever folded a trade " +
			"for, which is the subscription set rather than the message rate. The per-series history " +
			"inside each entry IS pruned, through pit.Put with the store's horizon.", ""},
	"internal/risk/pricing/credit: Store.byReference": {boundedByConstruction,
		"keyed by counterparty reference entity, written only by the credit Calibrator's scheduled " +
			"refresh over a configured set — no request path reaches Put. The version list at each key " +
			"is horizon-pruned through pit.Put, which TestPricingRetentionIsBounded enforces " +
			"separately.", ""},
	"internal/risk/pricing/curve: Store.byCurrency": {boundedByConstruction,
		"keyed by currency, and the currency list is parsed once at startup from " +
			"RISK_ENGINE_CALIBRATION_RATES — there is no runtime path that adds one. Per-key retention " +
			"is pit.Put's horizon, enforced by TestPricingRetentionIsBounded.", ""},
	"internal/risk/pricing/livequote: LiveQuotes.wanted": {boundedByConstruction,
		"the admitted instrument set itself, built by New from the strip parsed out of " +
			"RISK_ENGINE_CALIBRATION_RATES and never written again — no method mutates it, so its size " +
			"is decided before the subscription exists. It is what bounds LiveQuotes.latest below.", ""},
	"internal/risk/pricing/livequote: LiveQuotes.latest": {boundedByConstruction,
		"keyed by instrument id, and the ADMITTED key space is fixed at construction from the strip " +
			"parsed out of RISK_ENGINE_CALIBRATION_RATES — the same slice that binds the only reader, " +
			"SnapshotRateSource. Update drops anything else, so what the `market.>` wildcard delivers " +
			"cannot create an entry; only re-deploying with a longer strip can (#894). There is no " +
			"evictor because the key space cannot grow.", ""},
	"internal/risk/pricing/volsurface: Store.byUnderlying": {boundedByConstruction,
		"keyed by underlying id, written only by a scheduled calibration refresh, with pit.Put " +
			"pruning the version list at each key. Same shape and same writer discipline as the curve " +
			"and credit stores beside it.", ""},
	"internal/validation: Gate.reports": {boundedByConstruction,
		"keyed by ANALYTIC NAME and populated exactly once, at startup, from the compiled-in benchmark " +
			"inventory through app.LoadValidations. Growing it requires adding an analytic to the " +
			"source tree and redeploying.", ""},
	"internal/venuemargin: View.byAcct": {boundedByConstruction,
		"keyed by (venue, venue_account_id) off VenueMarginState, which each venue adapter publishes " +
			"for the ONE account its credential spends from. The key space is the operator's deployed " +
			"adapter credentials, and each fold replaces the whole snapshot rather than accumulating " +
			"into it.", ""},
	"pkg/bus: KafkaClient.readers": {boundedByConstruction,
		"one reader appended per Subscribe, and every composition root subscribes from a fixed " +
			"config-driven subject list at startup. It grows with the subjects a process is configured " +
			"to read, not with the messages on them; Close walks the same slice to shut them down.", ""},
	"services/accounting/internal/fxfeed: LiveFX.pairCcy": {boundedByConstruction,
		"built once in New from ParsePairs over an operator-supplied pair spec and never written " +
			"again — it is configuration held in a map, and the mutex is for the rates beside it.", ""},
	"services/accounting/internal/fxfeed: LiveFX.rates": {boundedByConstruction,
		"keyed by the foreign currency of a CONFIGURED pair: the handler looks the event's instrument " +
			"up in pairCcy first and drops anything unconfigured, so this map's key set can never " +
			"exceed the values of an immutable one.", ""},
	"services/autopilot/internal/actuate: LogScaler.counts": {boundedByConstruction,
		"keyed by scale-out target, and every call site in plan.go passes one of three string " +
			"literals — market-data, inference, risk-engine. No signal supplies this value, so no " +
			"volume of events can add a key.", ""},
	"services/autopilot/internal/actuate: LogFailover.done": {boundedByConstruction,
		"keyed by the region attribute of an SLO-burn signal, falling back to the signal's subject — " +
			"both of which name a service or region this platform itself deploys and monitors, not " +
			"anything an external feed mints. Contrast the same package's remediate maps, which take " +
			"their key off a DataQualityEvent and are filed as leaks.", ""},
	"services/datamaster/internal/pricing: Queue.byID": {boundedByConstruction,
		"keyed by ExceptionID(instrument, kind, source): a fixed four-value kind enum, a configured " +
			"vendor source, and an instrument out of the platform's own security master. It is also " +
			"the human override audit trail — a compliance record must say what the operator chose, so " +
			"evicting an entry would delete the answer rather than free memory.", ""},
	"services/lake-sink/internal/decode: CachingResolver.cache": {boundedByConstruction,
		"keyed by \"schema_id:version\", and a key is only inserted AFTER the schema registry resolved " +
			"it — a malformed or invented ref fails the inner resolve and is never cached. The key " +
			"space is therefore the registry's published catalogue, and (schema_id, version) is " +
			"immutable once published, so the entry never needs invalidating either.", ""},
	"services/lineage/internal/graph: Memory.datasets": {boundedByConstruction,
		"keyed by DatasetID{namespace, name}, built from the envelope's domain and schema ref — the " +
			"estate's schema taxonomy, dozens of entries, not its event count. The type's own comment " +
			"has said so since #244.", ""},
	"services/lineage/internal/graph: Memory.upstream": {boundedByConstruction,
		"the edge set of the same dataset taxonomy as datasets: one entry per dataset, each holding " +
			"the datasets it reads from. Bounded by the schema catalogue rather than by traffic.", ""},
	"services/lineage/internal/graph: Memory.ring": {boundedByConstruction,
		"a fixed-capacity ring (DefaultEventIndexCapacity, or LINEAGE_EVENT_INDEX_MAX): once full, " +
			"index() overwrites the oldest SLOT and deletes that slot's key from eventDS. It is the " +
			"evictor for the event index rather than a thing needing one, and this guard cannot see " +
			"that because the shrink is an indexed write paired with a delete on a SIBLING field.", ""},
	"services/market-data/internal/feed: Gate.lastSeq": {boundedByConstruction,
		"keyed by instrument_id, and the only wiring feeds this Gate the operator-configured " +
			"MARKET_DATA_FEED_INSTRUMENTS list — no venue path routes arbitrary per-tick symbols " +
			"through it.", ""},
	"services/market-data/internal/feed: Gate.lastTime": {boundedByConstruction,
		"the timestamp half of the same per-instrument gap check as lastSeq, with the same key and the " +
			"same operator-configured instrument list bounding it.", ""},
	"services/market-data/internal/feed: Snapshot.latest": {boundedByConstruction,
		"keyed by instrument_id with replace-on-write semantics, so its size is the instrument count " +
			"rather than the event count — the same bound as the Gate it sits beside.", ""},
	"services/market-data/internal/feed: Partitioned.lanes": {boundedByConstruction,
		"not an accumulator: a fixed-size []chan sized once at construction from cfg.lanes and " +
			"indexed by a hash of the instrument id. Nothing appends to it, and Close drains every " +
			"lane it holds.", ""},
	"services/market-data/internal/feed: captureSink.evs": {boundedByConstruction,
		"not long-lived at all: the sink is constructed inside Conform(), which runs under a " +
			"five-second deadline and returns, and no field anywhere holds the value afterwards. The " +
			"mutex is there because the adapter under conformance publishes concurrently.", ""},
	"services/oms/internal/riskview: View.byPF": {boundedByConstruction,
		"keyed by portfolio_id off risk.portfolio.measures_computed, which the risk engine publishes " +
			"for portfolios it computes. A LEVEL, NOT A DELTA: each fold replaces that portfolio's " +
			"snapshot, so only a new portfolio adds a key.", ""},
	"services/tv-sync/internal/projection: Projection.accounts": {boundedByConstruction,
		"tenant → account_id, where the account id is the portfolio off an order FACT that already " +
			"passed OMS entitlement — the provisioned account roster. NOTE the scope of that claim: it " +
			"covers this OUTER map only. #809 is about the per-account collections inside it " +
			"(orders, seenFills, execs), which live on a struct with no mutex of its own and are " +
			"therefore outside this guard's population entirely.", ""},
	"services/webhook-ingest/internal/ingest: PositionCache.byKey": {boundedByConstruction,
		"keyed by fund/venue/instrument off the OMS's own compacted venue-position stream. The webhook " +
			"caller influences the READ path only — Position() looks a key up and never creates one — " +
			"so an alert naming an invented instrument cannot add an entry.", ""},

	// ---------------------------------------------------------------------
	// isTheStore — the collection IS an in-memory store's content. Each of
	// these packages declares a Postgres sibling, which the guard checks: the
	// in-memory one is a stated deployment posture (one replica, ephemeral,
	// and the composition root has to say so out loud — #261), and evicting
	// from it would be data loss rather than hygiene.
	// ---------------------------------------------------------------------
	"internal/audit/linkstore: MemoryStore.links": {isTheStore,
		"the hash chain itself, in order. A regulator walks Links() end to end, so dropping a link " +
			"breaks verification of everything after it.", ""},
	"internal/audit/linkstore: MemoryStore.seen": {isTheStore,
		"keyed by a link's own SHA — the chain's duplicate check. Forgetting a hash would let the " +
			"same link be appended twice, which is the thing the set exists to refuse.", ""},
	"internal/marketdata/store: Memory.bars": {isTheStore,
		"keyed by (instrument, venue, resolution, bucket, KNOWLEDGE TIME): the store is bitemporal, so " +
			"a restatement is deliberately an additional row rather than an overwrite. Evicting one " +
			"changes the answer to an as-of query, which is the store's entire contract.", ""},
	"internal/marketdata/store: Memory.byInstrument": {isTheStore,
		"the observation set under the same bitemporal identity as bars — instrument, then the full " +
			"observation key including its knowledge time. Its Postgres sibling's primary key is the " +
			"same tuple.", ""},
	"internal/marketdata/store: Memory.coverage": {isTheStore,
		"one entry per (instrument, venue, resolution, bucket, ATTESTOR): who attested to a bar's " +
			"presence is part of the identity, and dropping an attestation silently weakens a coverage " +
			"answer.", ""},
	"services/accounting/internal/ledger: MemoryStore.journal": {isTheStore,
		"the IBOR journal keyed by portfolio — the fund's book of record when " +
			"ACCOUNTING_ALLOW_EPHEMERAL_LEDGER is opted into. Evicting an entry is deleting ledger " +
			"history.", ""},
	"services/accounting/internal/ledger: MemoryStore.seen": {isTheStore,
		"keyed by entry_id, the journal's idempotency ledger. Forgetting an id re-books the posting it " +
			"was refusing.", ""},
	"services/accounting/internal/ledger: MemoryStore.snapshots": {isTheStore,
		"one snapshot per portfolio, the resume point a fold restarts from. There is one entry per " +
			"portfolio the ledger books for, and it is data rather than cache.", ""},
	"services/alternatives/internal/fund: MemoryStore.journal": {isTheStore,
		"the commitment journal keyed by commitment_id, opted into through " +
			"ALTERNATIVES_ALLOW_EPHEMERAL_JOURNAL. It is the replay source a fund position is derived " +
			"from.", ""},
	"services/alternatives/internal/fund: MemoryStore.seen": {isTheStore,
		"keyed by event_id — the same idempotency role, and the same consequence, as the ledger's " +
			"seen set beside it.", ""},
	"services/audit/internal/audit: Memory.records": {isTheStore,
		"the WORM audit log in append order, opted into through AUDIT_ALLOW_EPHEMERAL_LOG. Every " +
			"record links to the previous hash, so dropping one breaks Head and the chain after it.", ""},
	"services/audit/internal/audit: Memory.byID": {isTheStore,
		"the same records indexed by event_id. It cannot shrink while records cannot, or a Get would " +
			"miss a record the chain still counts.", ""},
	"services/datamaster/internal/store: MemoryGoldenStore.records": {isTheStore,
		"the golden security master keyed by canonical instrument id, replace-on-write per resolution " +
			"cycle. Its size is the instrument universe, and evicting means serving no golden record " +
			"for an instrument the platform still trades.", ""},
	"services/oms/internal/order: MemoryStore.orders": {isTheStore,
		"the order book of record keyed by order_id, selected when OMS_DATABASE_URL is unset. Every " +
			"order must stay attributable — List, ListByPortfolio and ListByStatus are the parity " +
			"contract its Postgres sibling is held to, and that table is not pruned either. NOTE that " +
			"#842's body called this a test double; the composition root at " +
			"services/oms/cmd/oms/main.go says otherwise.", ""},
	"services/oms/internal/order: MemoryStore.appliedFills": {isTheStore,
		"the in-RAM mirror of the order_fills claim table (#782), keyed by fill_id. Forgetting a claim " +
			"reopens the double-fold window the claim exists to close, and the durable table keeps its " +
			"rows too.", ""},
	"services/oms/internal/position: Book.lots": {isTheStore,
		"keyed by (portfolio, venue, instrument), created only by a fill that already passed OMS " +
			"admission. A flat lot is retained deliberately: it carries cumulative realised P&L, and " +
			"nothing re-derives that if the entry goes.", ""},
	"services/oms/internal/position: Book.appliedFills": {isTheStore,
		"keyed by fill_id, and the field's own comment already argues this: it grows without bound and " +
			"so does position_fills, which is a cost of the store rather than a defect of the claim " +
			"(#818).", ""},
	"services/schema-registry/internal/storage: Memory.schemas": {isTheStore,
		"keyed by (schema_id, version), which is immutable once published — the registry's content, " +
			"not a cache of it. A version that disappeared would make an already-published payload " +
			"undecodable.", ""},
	"services/wealth/internal/book: MemoryStore.households": {isTheStore,
		"one household composition per household_id, last-write-wins, opted into through " +
			"WEALTH_ALLOW_EPHEMERAL_BOOK (without it the service refuses to start). Evicting drops a " +
			"household the estate has onboarded.", ""},

	// ---------------------------------------------------------------------
	// deferredLeak — genuinely unbounded, enumerated rather than fixed here,
	// each with the issue that retires it. This block is a debt register and
	// the only correct direction for it is shorter: the dead-entry arm fails
	// the day one of these grows an evictor and the entry is left behind.
	// ---------------------------------------------------------------------
	"internal/outbox: Memory.rows": {deferredLeak,
		"append-only, and MarkPublished only flips a bool — every read path FILTERS on published " +
			"rather than removing, so a published row is dead weight for the life of the process. This " +
			"is the OMS's, the position book's and the accounting ledger's outbox when no DSN is set.",
		"#890"},
	"internal/venueadapter/orderview: Memory.orders": {deferredLeak,
		"keyed by ORDER ID with no removal: a terminal order is filtered out of Open()'s return value " +
			"and kept in the map. One permanent entry per order either venue adapter has ever worked, " +
			"in the no-DSN posture both of them support.",
		"#891"},
	"services/autopilot/internal/remediate: LogQuarantiner.set": {deferredLeak,
		"keyed by the subject off a DataQualityEvent — a wire field with no bounded universe — and " +
			"read by nothing outside a test. It grows fastest during exactly the degraded period the " +
			"autopilot exists to handle. Same class as #844.",
		"#892"},
	"services/autopilot/internal/remediate: LogModelRoller.rolled": {deferredLeak,
		"keyed by a model_id attribute that FALLS BACK to the same wire subject when absent, so the " +
			"real model population is a convention rather than a bound. Also read by nothing outside a " +
			"test.",
		"#892"},
	"internal/risk: Cache.exposure": {deferredLeak,
		"keyed by portfolio — estate-controlled, so not the #814 shape — but never pruned when this " +
			"replica RELEASES ownership on a ring rebalance, which state/ownership.go already does for " +
			"the state store. The stale entry is still readable through the degraded path.",
		"#893"},
	"internal/risk: Cache.measures": {deferredLeak,
		"the measure half of the same asymmetry as Cache.exposure: written on every recompute, never " +
			"dropped on ownership handoff.",
		"#893"},
	"internal/execution: SimVenue.executed": {deferredLeak,
		"keyed by order_id and documented as unbounded by design because 'it is a simulator' — but " +
			"the OMS selects it whenever OMS_VENUE_ENDPOINTS is empty, so the process holding it is a " +
			"real one. The dedup argument for keeping the record is sound; the missing bound is not.",
		"#895"},
	"internal/prediction: PredictionCache.entries": {deferredLeak,
		"keyed by a SubjectID that Predict checks only for emptiness — that it is a portfolio id is a " +
			"convention at the caller, not a property of the type. No composition root builds a " +
			"SyncClient today, so this is a shape rather than a live leak.",
		"#895"},
}

// TestEveryLongLivedMapHasAnEvictor walks the module and holds every
// mutex-guarded map and slice field to the invariant.
func TestEveryLongLivedMapHasAnEvictor(t *testing.T) {
	root := moduleRoot(t)
	est := scanEvictionEstate(t, root)

	// NON-VACUITY 1: the estate was actually walked. These floors are well under
	// the measured population (2026-08-31: 87 map and 10 slice fields across the
	// module) so ordinary deletions do not trip them, and a walk that lost a tree
	// cannot pass on an empty set.
	if est.packages < 150 {
		t.Fatalf("parsed only %d package(s) of the module — the walk is broken, not the estate", est.packages)
	}
	if got := len(est.fields); got < 70 {
		t.Fatalf("found only %d mutex-guarded map/slice field(s) estate-wide — the struct walk has "+
			"drifted and this guard's verdict is empty", got)
	}
	if got := est.kindCount("slice"); got < 8 {
		t.Fatalf("found only %d mutex-guarded SLICE field(s) estate-wide — the slice half of this walk "+
			"has drifted. An append-only slice under a lock is the same unbounded leak as an unevicted "+
			"map with none of the delete( vocabulary that makes the map version greppable (#844)", got)
	}

	// NON-VACUITY 2: the anchors still resolve, per directory.
	for _, scope := range evictionScopes {
		if est.files[scope.dir] < scope.minFiles {
			t.Errorf("parsed only %d non-test file(s) in %s — the package moved and this guard is "+
				"scanning nothing there", est.files[scope.dir], scope.dir)
			continue
		}
		for _, must := range scope.mustFind {
			if _, ok := est.fields[scope.dir+": "+must]; !ok {
				t.Errorf("the struct scan did not find a field %q in %s — it is one this guard was "+
					"written about, so the walk has drifted", must, scope.dir)
			}
		}
		if !est.shrunk[scope.dir+": "+scope.mustEvict] {
			t.Errorf("the shrink scan did not attribute any delete(/truncation to %s in %s — that field "+
				"has had an evictor since it was written, so receiver attribution is broken and this walk "+
				"would report every collection as unevicted or none of them", scope.mustEvict, scope.dir)
		}
	}

	// UNATTRIBUTED ARM: a delete on a struct field the guard cannot tie to the
	// enclosing method's receiver, WHERE THAT FIELD NAME IS ALSO A TRACKED FIELD
	// in the same package. That is the case that matters: silently counting such
	// a delete for every field of that name is how a name-keyed check lets one
	// type's fix vouch for another type's leak, and silently dropping it reports
	// an evicted field as a leak. Deletes through a local whose name collides
	// with nothing tracked are noise — the compliance Monitor's book.positions
	// (#810) is one, on a struct with no mutex of its own.
	if len(est.unattributed) > 0 {
		sort.Strings(est.unattributed)
		t.Errorf("shrink sites this guard cannot attribute to a receiver, whose field name collides with "+
			"a tracked field: %v.\n\nEither move the delete into a method on the owning type, or extend "+
			"scanEvictionEstate to resolve this shape — do not leave it unattributed.", est.unattributed)
	}

	usedExempt := map[string]bool{}
	var unevicted []string

	keys := make([]string, 0, len(est.fields))
	for k := range est.fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		if est.shrunk[key] {
			continue
		}
		ex, ok := mapEvictionExempt[key]
		if !ok {
			unevicted = append(unevicted, key)
			continue
		}
		usedExempt[key] = true

		// THE CLASS IS CHECKED, NOT TAKEN ON TRUST. Each of the three carries an
		// obligation the guard can verify, so an entry cannot be upgraded to a
		// safer-sounding class without the evidence that class requires.
		switch ex.class {
		case isTheStore:
			// DERIVED, not asserted: "this map IS the store" is only admissible
			// where a durable sibling exists in the same package, because that is
			// what makes the in-memory one a deployment posture rather than the
			// only way the service can run. If the Postgres implementation is ever
			// deleted, this exemption stops being admissible the same day.
			if !est.durableSibling[pkgOfKey(key)] {
				t.Errorf("%s is exempt as %q, but %s declares no Postgres* type — an in-memory store with "+
					"no durable sibling is not a posture, it is the only way that service runs, and its "+
					"growth is a leak", key, ex.why, pkgOfKey(key))
			}
		case deferredLeak:
			if !issueRefPattern.MatchString(ex.issue) {
				t.Errorf("%s is exempt as a KNOWN unbounded leak but names no issue (%q) — a leak with no "+
					"issue is a leak nobody is going to fix", key, ex.issue)
			}
		case boundedByConstruction:
			// The obligation here is the argument in `why`, which no test can
			// check. The floor below is that there IS one, at length: the entries
			// this guard rejects are the one-word ones.
		}
		if len(strings.Fields(ex.why)) < 8 {
			t.Errorf("exemption %s has a reason of %d word(s) (%q) — an exemption states the KEY SPACE and "+
				"WHO WRITES IT, because that is the claim being made", key, len(strings.Fields(ex.why)), ex.why)
		}
	}

	if len(unevicted) > 0 {
		t.Errorf("these long-lived collections are shrunk by nothing anywhere in the module: %v.\n\n"+
			"A map or slice on a type that lives as long as the process, with a key or an append rate that "+
			"grows with traffic, is a leak that reports nothing until the pod is OOM-killed. On the "+
			"execution path that means orders in flight at an unknown state and a restart that has to "+
			"reconcile them — which is what Producer.sequence cost, keyed by order id, in the platform's "+
			"highest-throughput map (#805); one service out it was the gateway's per-principal quota maps, "+
			"on the only route that accepts an order (#834).\n\n"+
			"Give it an evictor (bus.Producer.gcSequence, bus.DedupWindow.gc and middleware.bucketSet.gc "+
			"are the shapes the estate already uses), or add it to mapEvictionExempt with the class that "+
			"fits and an argument that its key space is bounded BY CONSTRUCTION — not that it looks small "+
			"today. If it IS unbounded and you are not fixing it here, file the issue and use "+
			"deferredLeak: an enumerated leak is the point of this guard being estate-wide.", unevicted)
	}

	// DEAD-ENTRY ARM: an exemption that matches no field, or one whose field has
	// since grown an evictor, is a claim nobody is checking any more. In
	// particular it is how a deferredLeak survives the fix that retired it.
	for key, ex := range mapEvictionExempt {
		if usedExempt[key] {
			continue
		}
		t.Errorf("exemption %q (%s) was never applied — either it names no map or slice field on a "+
			"mutex-guarded type, or that field now HAS an evictor. Delete it: an exemption nobody checks "+
			"reads as a decision somebody made.", key, ex.why)
	}

	// REACHABILITY ARM: the crude half of limitation (1) above. A field is
	// credited to the METHOD that shrinks it; if nothing in the estate's non-test
	// source ever names that method, the evictor is dead code and the field is
	// leaking behind a green guard. This does not prove the evictor runs — see
	// the limitations — it proves the estate still calls it at all.
	var orphaned []string
	for key, methods := range est.shrinkMethod {
		// ANY live evictor is enough. domain.Portfolio.positions is shrunk by
		// both Forget (which nothing outside a test calls) and ClearPositions
		// (which risk/state's snapshot apply does) — reporting the field on the
		// strength of the dead one would be this arm inventing a leak.
		live, names := false, make([]string, 0, len(methods))
		for m := range methods {
			names = append(names, m)
			if est.calls(pkgOfKey(key), m) {
				live = true
			}
		}
		if live || len(names) == 0 {
			continue
		}
		sort.Strings(names)
		orphaned = append(orphaned, key+" (evictor(s) "+strings.Join(names, ", ")+")")
	}
	if len(orphaned) > 0 {
		sort.Strings(orphaned)
		t.Errorf("these collections are credited to an evictor that NO non-test source in the module "+
			"calls: %v.\n\nAn evictor nothing invokes is dead code, and this guard would otherwise pass "+
			"on it — deleting a sweep's call site while leaving the sweep behind was measured to leave "+
			"the pre-#842 version of this guard green. Either call it, or remove it and account for the "+
			"field honestly.", orphaned)
	}

	t.Logf("estate: %d package(s), %d mutex-guarded collection field(s) (%d map, %d slice), %d shrunk, "+
		"%d exempt", est.packages, len(est.fields), est.kindCount("map"), est.kindCount("slice"),
		len(est.shrunk), len(mapEvictionExempt))
}

func pkgOfKey(key string) string {
	if i := strings.Index(key, ": "); i >= 0 {
		return key[:i]
	}
	return key
}

// estate is one walk's findings, keyed "pkgdir: Type.field" throughout.
type estate struct {
	fields         map[string]string // key → "map" | "slice"
	shrunk         map[string]bool
	shrinkMethod   map[string]map[string]bool // key → the method that shrinks it
	namesCalled    map[string]bool            // every function/method name called anywhere
	namesCalledIn  map[string]map[string]bool // pkgdir → the names called inside that package
	durableSibling map[string]bool            // pkgdir → declares a Postgres* type
	files          map[string]int             // pkgdir → non-test files parsed
	unattributed   []string
	packages       int
}

// calls reports whether anything in the estate invokes a method of this name
// that could be pkg's own.
//
// AN UNEXPORTED NAME IS RESOLVED INSIDE ITS OWN PACKAGE ONLY, and that is not a
// nicety. With a single estate-wide name set, deleting the compliance
// sayOnce sweep's ONLY call site left this guard green — web-bff's session store
// has a sweep() of its own, and it vouched for a sweep that no longer ran. That
// was measured, as a surviving mutation, before this method existed.
//
// An exported name can legitimately be called from any package (risk/state calls
// domain.Portfolio.ClearPositions), so it keeps the estate-wide set. That is this
// arm's remaining coarseness: two packages with an exported evictor of the same
// name can still vouch for each other.
func (e *estate) calls(pkg, method string) bool {
	if method == "" {
		return true
	}
	if unicode.IsUpper(rune(method[0])) {
		return e.namesCalled[method]
	}
	return e.namesCalledIn[pkg][method]
}

func (e *estate) kindCount(kind string) int {
	n := 0
	for _, k := range e.fields {
		if k == kind {
			n++
		}
	}
	return n
}

// scanEvictionEstate parses every non-test .go file in the module and returns
// the mutex-guarded map and slice fields it holds, which of them something
// shrinks through their own receiver, and the supporting facts the arms above
// check. Directories are filtered by skipWalkDir — .claude/worktrees is another
// agent's FULL CHECKOUT and reading it would let one worktree's edits decide
// this one's verdict.
func scanEvictionEstate(t *testing.T, root string) *estate {
	t.Helper()

	e := &estate{
		fields:         map[string]string{},
		shrunk:         map[string]bool{},
		shrinkMethod:   map[string]map[string]bool{},
		namesCalled:    map[string]bool{},
		namesCalledIn:  map[string]map[string]bool{},
		durableSibling: map[string]bool{},
		files:          map[string]int{},
	}

	var dirs []string
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skipWalkDir(d) || (d.IsDir() && d.Name() == "testdata") {
			return filepath.SkipDir
		}
		if d.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(dirs)

	for _, dir := range dirs {
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			t.Fatalf("rel %s: %v", dir, err)
		}
		rel = filepath.ToSlash(rel)

		paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatalf("glob %s: %v", dir, err)
		}
		fset := token.NewFileSet()
		var files []*ast.File
		for _, p := range paths {
			if strings.HasSuffix(p, "_test.go") {
				continue
			}
			// Mode 0: comments are not attached, so nothing here can match its own
			// prose — and build constraints are not applied, on purpose, so a
			// tagged file cannot hide a leak.
			f, perr := parser.ParseFile(fset, p, nil, 0)
			if perr != nil {
				t.Fatalf("parse %s: %v", p, perr)
			}
			files = append(files, f)
		}
		if len(files) == 0 {
			continue
		}
		e.packages++
		e.files[rel] = len(files)

		local := map[string]bool{} // field NAMES tracked in this package
		for _, f := range files {
			ast.Inspect(f, func(n ast.Node) bool {
				ts, ok := n.(*ast.TypeSpec)
				if !ok {
					return true
				}
				if strings.HasPrefix(ts.Name.Name, "Postgres") {
					e.durableSibling[rel] = true
				}
				st, ok := ts.Type.(*ast.StructType)
				// LONG-LIVED IS "GUARDED BY A MUTEX", and that is a discriminator
				// rather than a proxy. A collection that needs a lock is one more
				// than one goroutine reaches, which means it belongs to something
				// the process holds — a Producer, a window, a registry. A map on a
				// per-message value like Message.Headers dies with the message and
				// cannot leak, and the first draft of this guard flagged exactly
				// that before this arm existed.
				if !ok || !isMutexGuarded(st) {
					return true
				}
				for _, fl := range st.Fields.List {
					kind := collectionKind(fl.Type)
					if kind == "" {
						continue
					}
					for _, name := range fl.Names {
						e.fields[rel+": "+ts.Name.Name+"."+name.Name] = kind
						local[name.Name] = true
					}
				}
				return true
			})
		}

		// PASS 1: helpers that shrink a map or slice PARAMETER. Eviction through a
		// parameter is the false positive the #842 audit found first —
		// marketedge/book's applyLevels calls delete(side, key) on a side it was
		// HANDED — and a scan that only looks for delete(x.field, …) reports that
		// book as leaking.
		shrinksParam := map[string]map[int]bool{}
		for _, f := range files {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				for i, pn := range collectionParams(fn) {
					if pn == "" || !shrinksIdent(fn.Body, pn) {
						continue
					}
					if shrinksParam[fn.Name.Name] == nil {
						shrinksParam[fn.Name.Name] = map[int]bool{}
					}
					shrinksParam[fn.Name.Name][i] = true
				}
			}
		}

		// PASS 2: attribute shrink sites to the enclosing method's receiver. A
		// closure a method RETURNS is still inside that decl — which is where
		// quota.acquire's release func deletes from — so it attributes too.
		//
		// Attribution is by receiver rather than by field name because field names
		// repeat: the middleware package held `buckets` on TWO types and only one
		// ever got an evictor, so a name-keyed check would have let the fixed one
		// vouch for the unfixed one and gone green over a live leak.
		for _, f := range files {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				recvName, recvType := receiverOf(fn)
				fresh := freshLocals(fn.Body)
				lazyInit := lazyInitAssignments(fn.Body)
				credit := func(sel *ast.SelectorExpr) bool {
					base, isIdent := sel.X.(*ast.Ident)
					if !isIdent || recvName == "" || base.Name != recvName {
						return false
					}
					key := rel + ": " + recvType + "." + sel.Sel.Name
					e.shrunk[key] = true
					if e.shrinkMethod[key] == nil {
						e.shrinkMethod[key] = map[string]bool{}
					}
					e.shrinkMethod[key][fn.Name.Name] = true
					return true
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.CallExpr:
						name := evictionCalleeName(x.Fun)
						if name != "" {
							e.namesCalled[name] = true
							if e.namesCalledIn[rel] == nil {
								e.namesCalledIn[rel] = map[string]bool{}
							}
							e.namesCalledIn[rel][name] = true
						}
						if id, isIdent := x.Fun.(*ast.Ident); isIdent &&
							(id.Name == "delete" || id.Name == "clear") && len(x.Args) > 0 {
							sel, isSel := x.Args[0].(*ast.SelectorExpr)
							if !isSel {
								return true // a local map cannot outlive its scope
							}
							if !credit(sel) && local[sel.Sel.Name] {
								e.unattributed = append(e.unattributed,
									rel+": "+exprString(sel)+" in "+fn.Name.Name)
							}
							return true
						}
						// The receiver's own field, handed to a helper that shrinks it.
						for i, a := range x.Args {
							if !shrinksParam[name][i] {
								continue
							}
							if sel, isSel := a.(*ast.SelectorExpr); isSel {
								credit(sel)
							}
						}
					case *ast.AssignStmt:
						// x.f = x.f[i:] and x.f = append(x.f[:0], x.f[i:]...) are how
						// this estate trims a retention window — trades.Tape does the
						// second — and neither contains a delete(.
						for i, lhs := range x.Lhs {
							sel, isSel := lhs.(*ast.SelectorExpr)
							if !isSel || i >= len(x.Rhs) || lazyInit[x] {
								continue
							}
							if truncates(x.Rhs[i]) || replacesWholesale(x.Rhs[i], fresh) {
								credit(sel)
							}
						}
					}
					return true
				})
			}
		}
	}
	return e
}

// collectionKind reports "map", "slice" or "" for a struct field's type.
func collectionKind(t ast.Expr) string {
	switch x := t.(type) {
	case *ast.MapType:
		return "map"
	case *ast.ArrayType:
		if x.Len == nil {
			return "slice"
		}
	}
	return ""
}

// truncates reports whether an assignment's right-hand side can only make the
// collection SMALLER — a reslice, or nil. `make(...)` is deliberately NOT here:
// a constructor writing s.m = make(map[...]) would otherwise credit itself with
// an evictor it does not have, and that is the exact shape this guard exists to
// refuse.
func truncates(rhs ast.Expr) bool {
	found := false
	ast.Inspect(rhs, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SliceExpr:
			found = true
		case *ast.Ident:
			if x.Name == "nil" {
				found = true
			}
		}
		return !found
	})
	return found
}

// replacesWholesale reports whether an assignment hands the field a FRESH
// collection — make(...), a composite literal, or a local built from one — which
// drops every key the old one held. balancerecon.View rebuilds its whole asset
// map that way per announcement (next := map[string]*big.Rat{} … v.assets =
// next), and risk domain.Portfolio.ClearPositions does the make(...) form; a
// scan that only looks for delete( calls both of them leaks.
func replacesWholesale(rhs ast.Expr, fresh map[string]bool) bool {
	switch x := rhs.(type) {
	case *ast.CompositeLit:
		return collectionKind(x.Type) != ""
	case *ast.CallExpr:
		if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "make" && len(x.Args) > 0 {
			return collectionKind(x.Args[0]) != ""
		}
	case *ast.Ident:
		return fresh[x.Name]
	}
	return false
}

// freshLocals names the locals in a body that were built EMPTY — make(...) or a
// composite literal — so assigning one over a field reads as a reset rather than
// as an alias of something that was already full.
func freshLocals(body *ast.BlockStmt) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range as.Lhs {
			id, isIdent := lhs.(*ast.Ident)
			if !isIdent || i >= len(as.Rhs) {
				continue
			}
			if replacesWholesale(as.Rhs[i], nil) {
				out[id.Name] = true
			}
		}
		return true
	})
	return out
}

// lazyInitAssignments are the assignments guarded by a nil test — a LAZY
// INITIALISER, which drops nothing and must not be read as an evictor. Without
// this arm any leaking map could buy itself a pass by growing a nil check:
// if m.entries == nil { m.entries = make(...) } would otherwise credit itself.
func lazyInitAssignments(body *ast.BlockStmt) map[*ast.AssignStmt]bool {
	out := map[*ast.AssignStmt]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || !comparesToNil(ifs.Cond) {
			return true
		}
		ast.Inspect(ifs.Body, func(m ast.Node) bool {
			if as, isAssign := m.(*ast.AssignStmt); isAssign {
				out[as] = true
			}
			return true
		})
		return true
	})
	return out
}

// comparesToNil reports whether a condition tests anything against nil.
func comparesToNil(cond ast.Expr) bool {
	bin, ok := cond.(*ast.BinaryExpr)
	if !ok {
		return false
	}
	for _, side := range []ast.Expr{bin.X, bin.Y} {
		if id, isIdent := side.(*ast.Ident); isIdent && id.Name == "nil" {
			return true
		}
	}
	return comparesToNil(bin.X) || comparesToNil(bin.Y)
}

// shrinksIdent reports whether a body deletes from, clears or truncates the
// named local (a parameter).
func shrinksIdent(body *ast.BlockStmt, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && (id.Name == "delete" || id.Name == "clear") &&
				len(x.Args) > 0 {
				if a, ok := x.Args[0].(*ast.Ident); ok && a.Name == name {
					found = true
				}
			}
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && id.Name == name && i < len(x.Rhs) &&
					truncates(x.Rhs[i]) {
					found = true
				}
			}
		}
		return true
	})
	return found
}

// collectionParams returns one entry per parameter, holding the parameter's name
// where it is a map or a slice and "" otherwise, so an index lines up with the
// call site's argument list.
func collectionParams(fn *ast.FuncDecl) []string {
	var out []string
	if fn.Type.Params == nil {
		return out
	}
	for _, f := range fn.Type.Params.List {
		isCollection := collectionKind(f.Type) != ""
		if len(f.Names) == 0 {
			out = append(out, "")
			continue
		}
		for _, n := range f.Names {
			if isCollection {
				out = append(out, n.Name)
			} else {
				out = append(out, "")
			}
		}
	}
	return out
}

// calleeName is the called function or method's name, for both f() and x.f().
func evictionCalleeName(fun ast.Expr) string {
	switch x := fun.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	case *ast.IndexExpr: // generic instantiation: f[T]()
		return evictionCalleeName(x.X)
	case *ast.IndexListExpr:
		return evictionCalleeName(x.X)
	}
	return ""
}

// receiverOf returns a method's receiver variable name and its base type name,
// unwrapping a pointer and a GENERIC receiver — Memory[T] and ttlStore[K, V]
// both resolved to "" before #842, so every delete they made was reported as
// unattributable and two correctly-evicting stores read as leaks.
// Both are empty for a plain function.
func receiverOf(fn *ast.FuncDecl) (name, typeName string) {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return "", ""
	}
	f := fn.Recv.List[0]
	if len(f.Names) == 1 {
		name = f.Names[0].Name
	}
	typ := f.Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	switch g := typ.(type) {
	case *ast.IndexExpr:
		typ = g.X
	case *ast.IndexListExpr:
		typ = g.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		typeName = id.Name
	}
	if name == "" || typeName == "" {
		return "", ""
	}
	return name, typeName
}

// isMutexGuarded reports whether a struct holds a sync.Mutex or sync.RWMutex —
// this guard's test for "the process holds this, and more than one goroutine
// reaches it".
func isMutexGuarded(st *ast.StructType) bool {
	for _, f := range st.Fields.List {
		sel, ok := f.Type.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		pkg, isIdent := sel.X.(*ast.Ident)
		if !isIdent || pkg.Name != "sync" {
			continue
		}
		if sel.Sel.Name == "Mutex" || sel.Sel.Name == "RWMutex" {
			return true
		}
	}
	return false
}
