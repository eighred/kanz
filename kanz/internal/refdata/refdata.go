// Package refdata is the platform's ONE instrument reference-data source: the
// thing this estate did not have, and whose absence made a sector mandate
// unable to fire and a named stress scenario return the book it was asked to
// shock (#640).
//
// # What was missing, precisely
//
// Two Classifier interfaces existed — compliance.Classifier (issuer / sector /
// asset class, for the COMP-01 rule engine) and factor.Classifier (sector /
// asset class, for the RISK-06 exposure and the scenario shocks) — and the only
// implementation of either was an in-memory map whose sole constructors were
// _test.go files. Every production seam was passed nil. The refusals that now
// stand in for the missing data (compliance.unresolvedDimension,
// v1.ErrScenarioUnresolvable) stopped those controls LYING; they did not make
// them work. This package is what makes them work.
//
// # One implementation, two interface shapes
//
// The two interfaces ask the same question — what is this instrument, as of
// when — in two vocabularies, because they were written by two subsystems that
// could not share a type without one importing the other. That is a reason for
// two INTERFACES, never for two sources. A second reference-data lookup is how
// a sector fix stops spreading: one of them gets the taxonomy right and the
// other does not, and nothing reports the disagreement. So Cache resolves once,
// into one Record, and Compliance and Factor are thin projections of it.
//
// # Why the hot path never does I/O
//
// The compliance rule evaluators call Classify with context.TODO — they take
// only a *Candidate, so no request context, no deadline and no cancellation
// reach them. A Classifier that dialled a service from there would put an
// unbounded, uncancellable HTTP call inside the OMS pre-trade gate, once per
// holding per order, on the path an order is admitted on. It would also couple
// order admission to datamaster's availability, which is a trading outage
// caused by a reference-data read.
//
// So Lookup is a MEMORY READ and nothing else. The cache is filled out-of-band
// by Refresh, which the composition root drives on a ticker in a goroutine it
// joins. This package starts no goroutines of its own and holds no timers, so
// there is nothing here to leak — the lifetime of every background cycle
// belongs to the frame whose deferred closers unwind on SIGTERM.
//
// # It is demand-driven, so it is bounded by the book and not by the universe
//
// A security master is large and this platform needs the part of it somebody
// actually holds. Lookup records a MISS as a WANT; the next Refresh fetches the
// wants and the residents whose entries have aged out, and drops residents
// nothing has asked about for IdleTTL. The resident set therefore converges on
// the instruments the estate is evaluating, with MaxEntries as a backstop
// rather than as the working bound.
//
// # Cold, stale and unknown are three refusals, not one silence
//
// A miss returns ok=false, which the rule engine turns into "this dimension
// cannot be resolved for every holding" and names the instruments. That is the
// correct answer while the cache is cold — the same stance the OMS reference-
// mark source already takes, where a freshly started pod refuses market orders
// until the first tick for that instrument — and it self-heals within one
// refresh interval. An entry older than MaxAge stops being served at all, so a
// datamaster outage that outlives the staleness bound degrades to refusals
// rather than to a sector map nobody has confirmed in hours.
package refdata

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Sector is an instrument's industry classification — the source of the SECTOR
// exposure dimension. It mirrors reference.v1.SectorClassification and
// datamaster's master.Sector; the taxonomy is carried because "10" means
// different industries in GICS and in ICB, and a bucket key that dropped it
// would merge two taxonomies into one concentration number.
type Sector struct {
	Taxonomy, Code, Name string
}

// IsZero reports whether the master resolved no sector for this instrument.
func (s Sector) IsZero() bool { return s.Taxonomy == "" && s.Code == "" }

// Key is the bucket string a concentration or restriction rule groups on, and
// "" when the sector is unset — which every caller must treat as unresolved
// rather than as a bucket, because "" would otherwise collect every
// unclassified holding into one enormous group that no limit names.
func (s Sector) Key() string {
	if s.IsZero() {
		return ""
	}
	return s.Taxonomy + ":" + s.Code
}

// Record is one instrument's resolved classification — the golden record's
// classification half, as datamaster's survivorship left it.
type Record struct {
	InstrumentID string
	// AssetClass is the reference.v1 vocabulary without the enum prefix
	// ("EQUITY", "FIXED_INCOME"), which is the form both Classifier interfaces
	// bucket on.
	AssetClass string
	Sector     Sector
	// IssuerID is the issuing entity — the ISSUER single-name concentration
	// dimension. EMPTY IS A REAL ANSWER for FX and broad-index instruments, and
	// it is indistinguishable from "the vendor did not report one", so a rule
	// that needs an issuer refuses on it rather than bucketing under "".
	IssuerID string
	// AsOf is the effective time of the snapshot the master resolved. It is what
	// makes a point-in-time question answerable-or-refusable instead of silently
	// answered with today's classification.
	AsOf time.Time
}

// Source fetches one instrument's record from the security master. datamaster's
// HTTP surface is the implementation (NewHTTPSource); the seam exists so the
// cache's behaviour is testable without a server, and so a deployment whose
// master speaks something else implements one thing rather than forking the
// cache.
//
// found=false means the master DEFINITIVELY does not hold this instrument — a
// 404, not a failure. An error means the question could not be asked, which is
// a different fact: the cache keeps a resident entry across an error and
// discards one across a not-found.
type Source interface {
	Fetch(ctx context.Context, instrumentID string) (rec Record, found bool, err error)
}

// Defaults for the cache's bounds. They are exported because the composition
// root logs them at startup: an operator reading "reference data armed" needs
// to know how stale an answer may be before it stops being served.
const (
	// DefaultRefreshTTL is how old a resident entry may get before Refresh
	// re-fetches it. Reference data is slowly-changing — a sector
	// reclassification is a quarterly event — so this is generous by market-data
	// standards.
	DefaultRefreshTTL = 15 * time.Minute

	// DefaultMaxAge is the hard staleness bound: past it an entry is NOT SERVED.
	// It must exceed DefaultRefreshTTL by enough that one ordinary failed cycle
	// does not start refusing orders, and be short enough that a sustained
	// datamaster outage does not leave a compliance control deciding on
	// yesterday's classification.
	DefaultMaxAge = 2 * time.Hour

	// DefaultIdleTTL is how long an entry nothing has asked about is kept. It
	// bounds the resident set by the estate's ACTIVE book rather than by the
	// universe the master holds.
	DefaultIdleTTL = 6 * time.Hour

	// DefaultMaxEntries is the backstop on the resident set. It is not the
	// working bound — IdleTTL is — and reaching it is reported, never absorbed.
	DefaultMaxEntries = 50_000

	// DefaultMaxFetchPerCycle bounds ONE refresh cycle's fetches so a cold start
	// against a large book cannot become a single unbounded burst at datamaster.
	// The remainder is fetched next cycle and counted meanwhile.
	DefaultMaxFetchPerCycle = 512
)

// maxWantedMultiple bounds the want set relative to MaxEntries. The want set is
// fed by whatever instrument ids arrive in a book, so it is the one map an
// upstream could grow without limit; bounding it to the same order as the
// resident set keeps a malformed book from becoming memory growth.
const maxWantedMultiple = 2

// Options tune the cache's bounds. A zero field takes the Default above, so a
// composition root that wants the estate's posture passes Options{}.
type Options struct {
	RefreshTTL       time.Duration
	MaxAge           time.Duration
	IdleTTL          time.Duration
	MaxEntries       int
	MaxFetchPerCycle int
	// Now overrides the clock. Tests pin it; production leaves it nil.
	Now func() time.Time
}

func (o Options) withDefaults() (Options, error) {
	if o.RefreshTTL <= 0 {
		o.RefreshTTL = DefaultRefreshTTL
	}
	if o.MaxAge <= 0 {
		o.MaxAge = DefaultMaxAge
	}
	if o.IdleTTL <= 0 {
		o.IdleTTL = DefaultIdleTTL
	}
	if o.MaxEntries <= 0 {
		o.MaxEntries = DefaultMaxEntries
	}
	if o.MaxFetchPerCycle <= 0 {
		o.MaxFetchPerCycle = DefaultMaxFetchPerCycle
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	// MaxAge AT OR BELOW RefreshTTL WOULD REFUSE EVERYTHING between two
	// successful cycles — a configuration that reads like tightened safety and is
	// a total outage of every classified dimension. It is refused at construction
	// rather than discovered when the first sector mandate is declared.
	if o.MaxAge <= o.RefreshTTL {
		return Options{}, fmt.Errorf("refdata: MaxAge (%s) must exceed RefreshTTL (%s), or an entry "+
			"stops being served before the cycle that would renew it can run", o.MaxAge, o.RefreshTTL)
	}
	// IdleTTL below MaxAge would evict an entry that is still being served, so a
	// held instrument would flip between resolved and refused on the idle sweep.
	if o.IdleTTL < o.MaxAge {
		return Options{}, fmt.Errorf("refdata: IdleTTL (%s) must be at least MaxAge (%s), or a held "+
			"instrument is evicted while its record is still servable", o.IdleTTL, o.MaxAge)
	}
	return o, nil
}

// entry is one resident record plus the two timestamps that decide its fate:
// when it was fetched (RefreshTTL, MaxAge) and when it was last asked for
// (IdleTTL).
type entry struct {
	rec        Record
	fetchedAt  time.Time
	lastWanted time.Time
}

// Stats is the cache's observable state. The composition root registers these
// as gauges — an estate where Resolved is zero and Wanted is climbing is one
// whose compliance controls are all refusing, and it must be visible without
// reading a log.
type Stats struct {
	// Resolved is the number of instruments currently answerable.
	Resolved int
	// Wanted is the number asked for and not yet fetched. Persistently non-zero
	// means Refresh is failing, or the master does not hold what the estate does.
	Wanted int
	// Stale is the resident entries past MaxAge — counted separately because
	// they are NOT served, so they are invisible in Resolved while still costing
	// memory and still explaining a refusal.
	Stale int
	// Overflowed counts admissions refused because MaxEntries or the want bound
	// was reached, since process start. Non-zero means the cache is smaller than
	// the estate's working set, and some dimension is being refused for a reason
	// that is not a reference-data gap.
	Overflowed uint64
	// LastRefresh is when a cycle last completed with no failures; zero if none
	// has.
	LastRefresh time.Time
}

// Cache is the demand-driven reference-data cache. It is safe for concurrent
// use: Lookup runs on every order-evaluating goroutine while Refresh runs on
// the composition root's ticker.
type Cache struct {
	src  Source
	opts Options

	mu         sync.Mutex
	byID       map[string]*entry
	wanted     map[string]struct{}
	overflowed uint64
	lastOK     time.Time
}

// NewCache builds the cache over a Source.
func NewCache(src Source, opts Options) (*Cache, error) {
	if src == nil {
		// A nil Source would make every Lookup a miss forever, which reads
		// downstream as "the classifier does not resolve these instruments" — a
		// reference-data gap an operator would go and investigate — when the truth
		// is that nothing was wired. Those are the two states this estate refuses
		// to spell the same way, so this is a construction error, and a deployment
		// with no master passes a nil Classifier instead, which says the true
		// thing.
		return nil, errors.New("refdata: NewCache needs a Source; a deployment with no reference-data " +
			"source passes a nil Classifier to the gate instead, so that \"none wired\" and " +
			"\"wired but unknown to it\" stay distinguishable")
	}
	o, err := opts.withDefaults()
	if err != nil {
		return nil, err
	}
	return &Cache{
		src:    src,
		opts:   o,
		byID:   make(map[string]*entry),
		wanted: make(map[string]struct{}),
	}, nil
}

// Lookup answers what the cache knows about an instrument as of a point in
// time, and records a miss as a want. It never blocks on I/O.
//
// THE asOf RULE IS "NEVER GUESS BACKWARDS". The security master holds ONE
// snapshot per instrument — the latest projection — so a question about a time
// BEFORE that snapshot's own as_of cannot be answered from it. Serving today's
// sector for a backtest as-of last quarter would silently apply a
// reclassification that had not happened, which is the confidently-wrong answer
// this whole path exists to avoid. A zero asOf means "now".
//
// AN UNDATED RECORD IS EXEMPT FROM THAT RULE, and the reason is worth stating
// because the stricter reading is the tempting one. datamaster's as_of comes
// from the vendor extract and is optional there, so a delivery with no date
// column produces golden records with no as_of. Refusing those for any
// non-zero asOf would refuse EVERY evaluation the estate makes — the compliance
// engine passes the evaluation time as Candidate.AsOf, so nothing ever asks the
// zero-asOf question in production — and the refusal would arrive as
// "unresolved_instruments: <the whole book>", sending an operator to load
// reference data that is already loaded. An undated record makes no claim about
// when it was effective; the gap that creates for a genuine historical query
// belongs to the MASTER, which would have to keep history to close it, and not
// to a cache that would only be hiding it behind a permanent outage.
func (c *Cache) Lookup(instrumentID string, asOf time.Time) (Record, bool) {
	if instrumentID == "" {
		return Record{}, false
	}
	now := c.opts.Now()

	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.byID[instrumentID]
	if !ok {
		c.want(instrumentID)
		return Record{}, false
	}
	// Demand is recorded even when the answer is refused: the instrument IS being
	// asked about, so it must not be evicted as idle, and a stale entry must be
	// renewed rather than dropped.
	e.lastWanted = now

	if now.Sub(e.fetchedAt) > c.opts.MaxAge {
		return Record{}, false
	}
	// A record dated AFTER the question describes a state the asker's clock had
	// not reached. Compared against the record's OWN as_of rather than against
	// fetchedAt, so no clock skew between this process and datamaster can turn a
	// live evaluation into a refusal.
	if !asOf.IsZero() && !e.rec.AsOf.IsZero() && e.rec.AsOf.After(asOf) {
		return Record{}, false
	}
	return e.rec, true
}

// want records a miss. The caller holds the lock.
func (c *Cache) want(instrumentID string) {
	if _, dup := c.wanted[instrumentID]; dup {
		return
	}
	if len(c.wanted) >= c.opts.MaxEntries*maxWantedMultiple {
		c.overflowed++
		return
	}
	c.wanted[instrumentID] = struct{}{}
}

// Refresh runs one cycle: it drops idle residents, then fetches the wants and
// the residents whose entries have aged past RefreshTTL. It returns how many
// records it installed.
//
// THE CYCLE IS NOT ALL-OR-NOTHING, unlike datamaster's own projector. There the
// rule is right because survivorship over a subset of vendors produces a
// DIFFERENT golden record; here each instrument is independent, and abandoning
// forty-nine good records because the fiftieth failed would refuse forty-nine
// mandates that could have been evaluated. Failures are collected and returned
// together, so a caller still learns the cycle was partial.
//
// I/O HAPPENS OUTSIDE THE LOCK. Lookup must not block behind a slow datamaster;
// that is the whole reason this type exists.
func (c *Cache) Refresh(ctx context.Context) (int, error) {
	now := c.opts.Now()
	due := c.plan(now)

	type result struct {
		id    string
		rec   Record
		found bool
	}
	var (
		got  []result
		errs []error
	)
	for _, id := range due {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		rec, found, err := c.src.Fetch(ctx, id)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
			continue
		}
		got = append(got, result{id: id, rec: rec, found: found})
	}

	installed := 0
	c.mu.Lock()
	for _, r := range got {
		// A want is cleared whether or not the master holds the instrument: asking
		// again every cycle for something datamaster has answered 404 on would turn
		// one unknown instrument into a permanent request loop. It re-enters the
		// want set the next time somebody asks about it, which is the signal that
		// makes re-asking worthwhile.
		delete(c.wanted, r.id)
		if !r.found {
			// NOT-FOUND EVICTS. An instrument the master no longer holds must stop
			// being classified from a record the master has withdrawn.
			delete(c.byID, r.id)
			continue
		}
		if e, ok := c.byID[r.id]; ok {
			e.rec, e.fetchedAt = r.rec, now
			installed++
			continue
		}
		if len(c.byID) >= c.opts.MaxEntries {
			c.overflowed++
			continue
		}
		c.byID[r.id] = &entry{rec: r.rec, fetchedAt: now, lastWanted: now}
		installed++
	}
	if len(errs) == 0 {
		c.lastOK = now
	}
	c.mu.Unlock()

	if len(errs) > 0 {
		return installed, fmt.Errorf("refdata: %d of %d reference lookups failed: %w",
			len(errs), len(due), errors.Join(errs...))
	}
	return installed, nil
}

// plan evicts idle residents and returns the ids this cycle should fetch, wants
// first — a want is an instrument some control is refusing on RIGHT NOW, while
// a resident past RefreshTTL is one still being answered correctly.
func (c *Cache) plan(now time.Time) []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	for id, e := range c.byID {
		if now.Sub(e.lastWanted) > c.opts.IdleTTL {
			delete(c.byID, id)
		}
	}

	wants := make([]string, 0, len(c.wanted))
	for id := range c.wanted {
		wants = append(wants, id)
	}
	stale := make([]string, 0, len(c.byID))
	for id, e := range c.byID {
		if now.Sub(e.fetchedAt) > c.opts.RefreshTTL {
			stale = append(stale, id)
		}
	}
	// Sorted so a cycle truncated by MaxFetchPerCycle is DETERMINISTIC. Go's map
	// order would otherwise make "which instruments does this pod know about"
	// vary between replicas evaluating the same book, and a mandate that passes
	// on one pod and refuses on another is the worst shape this can take.
	sort.Strings(wants)
	sort.Strings(stale)

	due := append(wants, stale...)
	if len(due) > c.opts.MaxFetchPerCycle {
		due = due[:c.opts.MaxFetchPerCycle]
	}
	return due
}

// Stats reports the cache's observable state.
func (c *Cache) Stats() Stats {
	now := c.opts.Now()
	c.mu.Lock()
	defer c.mu.Unlock()

	s := Stats{Wanted: len(c.wanted), Overflowed: c.overflowed, LastRefresh: c.lastOK}
	for _, e := range c.byID {
		if now.Sub(e.fetchedAt) > c.opts.MaxAge {
			s.Stale++
			continue
		}
		s.Resolved++
	}
	return s
}
