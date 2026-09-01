package volprofilefeed

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/pit"
)

// THE CONSUMER HALF: a point-in-time registry of published profiles (#897).
//
// # What it is for, in one sentence
//
// So that a parent order can name a curve and every pod resolves the same one.
//
// # Why it retains VERSIONS rather than a latest
//
// A latest-only fold would be a cache, and a cache is exactly what breaks the
// property this exists to provide. A parent admitted at 23:00 is worked past
// midnight; a session completes, a new profile is published, and a pod holding
// only the newest curve re-derives that parent's schedule against a market it was
// never sized for. services/oms/internal/order.authorizeChild then compares the
// driver's own child against the new schedule as an exact rational and refuses it.
//
// So this keeps every version inside a horizon and resolves BY VERSION. The
// newest is available too — admission has to pick one — but nothing after
// admission reads it.
//
// # Retention, and what happens past it
//
// pit.Put bounds each series' version list to a horizon measured back from the
// NEWEST retained version, which is internal/pit's whole reason for existing
// (#871) and is why there is no prune written here: a second implementation of
// "retain to a horizon and release what it drops" is how #862's clear() came to be
// missing from one of two copies.
//
// A PIN OLDER THAN THE HORIZON RESOLVES TO NOTHING, AND THAT IS UNKNOWN RATHER
// THAN THE NEAREST CURVE. The parent then stops advancing, loudly — the driver
// reports an unworkable schedule on every tick and the failure is counted — which
// is the fail-closed direction. Substituting a neighbouring version would be
// worse in the way this estate names first: the order would keep working, against
// a curve nobody pinned, and every downstream reader would attribute the fills to
// a schedule that was never derived. The horizon is therefore an operational
// parameter with a rule attached: it must exceed the longest working window this
// deployment admits.

// DefaultRetention is how far back published versions are kept.
//
// SEVEN DAYS, AND THE NUMBER COMES FROM WHAT IT HAS TO OUTLIVE. A pinned version
// must still resolve for as long as the parent that pinned it is being worked, and
// a working window is bounded in practice by the driver-tick admission rule rather
// than by anything shorter — services/oms/internal/order.refuseUndrivableSchedule
// caps the SLICE rate, not the window. A week is comfortably longer than any
// window an execution desk works a single parent over, and the cost of the margin
// is a few dozen curves per series: this holds decoded shapes, not messages.
//
// It is deliberately LONGER than the 24h the MARKET stream retains. Retention on
// the bus bounds what a COLD pod can rebuild; this bounds what a running one can
// resolve. Making them equal would mean a version aged off the registry at the
// same moment it aged off the stream, so nothing could tell a pin the deployment
// outlived from one it never received.
const DefaultRetention = 7 * 24 * time.Hour

// ErrRegistry is what the fold refuses with.
var ErrRegistry = errors.New("volprofilefeed: registry")

// Registry folds published profiles into a version-addressed store.
//
// Safe for concurrent use: the bus handler folds on the delivery goroutine while
// admission and the schedule driver read on theirs.
type Registry struct {
	mu sync.RWMutex
	// series is keyed by (instrument, venue) and holds that series' retained
	// versions, ascending by as-of.
	//
	// THE KEY SPACE COMES OFF THE WIRE, so it is bounded by eviction rather than
	// by construction: a producer that published a typo'd instrument would
	// otherwise leave an entry behind forever. gcLocked drops a series once its
	// last version has aged past the horizon, which is the same bound the versions
	// themselves are under.
	series map[Series][]pit.Version[Profile]

	retention time.Duration
	tenant    string

	// folded, refused and evicted are counters an operator reads to tell a feed
	// that is quiet from one that is being rejected. A registry that dropped
	// messages silently would answer "no profile" with exactly the confidence of
	// one that had folded every message correctly.
	folded, refused, evicted int64
}

// NewRegistry returns an empty registry retaining versions for retention.
//
// A ZERO retention IS DefaultRetention, NOT "FOREVER". The reading that turns a
// missing option into an unbounded map is the same "nothing configured looks like
// checked, and fine" failure this estate refuses everywhere else, and here it
// would grow a map keyed off the wire.
func NewRegistry(retention time.Duration) *Registry {
	if retention <= 0 {
		retention = DefaultRetention
	}
	return &Registry{series: make(map[Series][]pit.Version[Profile]), retention: retention}
}

// Handle folds one published profile off the bus. It is a bus.EventHandler.
//
// # A MESSAGE THIS BUILD CANNOT READ IS DROPPED, LOUDLY, AND NEVER NACKED
//
// The replay/broadcast class has an unbounded MaxDeliver, so a message this build
// cannot decode will not become decodable by being redelivered — returning an
// error would redeliver one bad message forever while every message BEHIND it went
// unread, and a registry stuck three entries into a replay reports "no profile"
// with the same confidence as one that folded all of them. Dropping it and
// counting it is the honest failure, and it is exactly the stance
// internal/prediction/registry.BusFollower takes on the model log.
//
// THERE IS NO TENANT CHECK, AND THAT IS A DECISION. A volume profile is universal
// market data: every tenant's BTC-USDT trades on the same book, and the shape is
// measured from a public tape. It is folded into no tenant-scoped store and it
// names no portfolio, account or order, so there is nothing here for a
// cross-tenant message to corrupt — the same ground on which the OMS already folds
// the price spine and accounting folds its FX feed. What it MUST NOT do is reach
// a tenant-scoped decision unlabelled, and it does not: a schedule is derived from
// the pinned version, which is stamped on the tenant's own order.
func (r *Registry) Handle(_ context.Context, env *envelopepb.Envelope, payload []byte) error {
	var pb marketpb.VolumeProfile
	if err := proto.Unmarshal(payload, &pb); err != nil {
		r.count(&r.refused)
		return nil
	}
	prof, err := Decode(&pb)
	if err != nil {
		r.count(&r.refused)
		return nil
	}
	// THE ENVELOPE'S TENANT IS RECORDED RATHER THAN CHECKED, so an operator can
	// see WHOSE producer is feeding this registry without the fold acquiring an
	// authority it does not need.
	r.mu.Lock()
	if r.tenant == "" {
		r.tenant = env.GetTenantId()
	}
	r.mu.Unlock()

	r.Fold(prof)
	return nil
}

// Fold records one decoded profile.
//
// IDEMPOTENT AT THE AS-OF, because pit.Put replaces a version at the same instant
// rather than appending beside it — so a replay that redelivers the same message
// rebuilds the same registry, which is what makes a cold pod's fold equal a warm
// pod's.
func (r *Registry) Fold(p Profile) {
	if p.Series.InstrumentID == "" || p.Series.Venue == "" || p.Version == "" || p.AsOf.IsZero() {
		r.count(&r.refused)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	vs, dropped := pit.Put(r.series[p.Series], p.AsOf.UTC(), p, r.retention)
	r.series[p.Series] = vs
	r.folded++
	r.evicted += int64(dropped)
	r.gcLocked()
}

// gcLocked drops any series whose versions have all aged out.
//
// CALLED ON EVERY FOLD rather than from a sweep goroutine, which is the shape
// pkg/bus.Producer.gcSequence and bus.DedupWindow.gc already use here: a sweep
// nobody runs is an evictor that does not evict, and this way the bound holds
// without a second lifecycle to wire, forget, or lose on a pod that never reaches
// its first tick.
//
// It is O(series) on a map bounded by the instruments a market-data edge folds —
// tens, not thousands — and it only ever finds work when pit.Put has emptied a
// list, which needs an entire horizon of silence on that series.
func (r *Registry) gcLocked() {
	for k, vs := range r.series {
		if len(vs) == 0 {
			delete(r.series, k)
		}
	}
}

// Current is the newest retained version for a series.
//
// IT IS READ AT ADMISSION AND NOWHERE ELSE. Admission is the one moment a
// schedule may be planned against "whatever is newest", because that is the moment
// the choice is RECORDED — every later derivation resolves the version that was
// recorded. A driver or a child-admission check reading this instead would
// re-plan a parent mid-flight, which is the divergence the pin exists to prevent.
func (r *Registry) Current(s Series) (Profile, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	vs := r.series[s]
	if len(vs) == 0 {
		return Profile{}, false
	}
	return copyProfile(vs[len(vs)-1].V), true
}

// Resolve returns the exact version a parent order pinned.
//
// NO NEAREST-MATCH AND NO FALLBACK. A version this registry does not hold is
// UNKNOWN — the pin has aged past the retention horizon, or this pod has not
// finished its replay, or the producer never published it — and all three must
// reach a schedule as a refusal rather than as a neighbouring curve. Answering
// with the closest one would keep the order working against a market it was never
// sized for, and every fill would be attributed to a schedule that was never
// derived.
func (r *Registry) Resolve(s Series, version string) (Profile, bool) {
	if version == "" {
		return Profile{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, v := range r.series[s] {
		if v.V.Version == version {
			return copyProfile(v.V), true
		}
	}
	return Profile{}, false
}

// Stats reports how many profiles were folded, how many were refused, how many
// versions were evicted, and how many series are held.
//
// REFUSED IS THE ONE THAT MATTERS. A registry that folds nothing and a registry
// that rejects everything both answer "no profile" to every question, and only
// this number separates a quiet feed from a producer this build cannot read.
func (r *Registry) Stats() (folded, refused, evicted int64, series int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.folded, r.refused, r.evicted, len(r.series)
}

// Versions is how many versions are retained for a series. Zero for a series
// nobody has published, which is the same answer as one whose versions have all
// aged out — both are UNKNOWN to a schedule.
func (r *Registry) Versions(s Series) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.series[s])
}

// String reports the three facts that decide whether anything can be answered at
// all.
func (r *Registry) String() string {
	folded, refused, evicted, series := r.Stats()
	return fmt.Sprintf("volprofilefeed.Registry{series:%d, folded:%d, refused:%d, evicted:%d, retention:%s}",
		series, folded, refused, evicted, r.retention)
}

func (r *Registry) count(n *int64) {
	r.mu.Lock()
	*n++
	r.mu.Unlock()
}

// copyProfile hands out the caller's own curve.
//
// *big.Rat IS A POINTER, and a registry returning its own would let one reader's
// Mul silently rewrite the shape every later reader schedules against — with
// nothing to say so, and with the two pods this design exists to keep in agreement
// disagreeing for a reason no log records.
func copyProfile(p Profile) Profile {
	if p.Expected == nil {
		return p
	}
	out := p
	out.Expected = make([]*big.Rat, len(p.Expected))
	for i, e := range p.Expected {
		out.Expected[i] = new(big.Rat).Set(e)
	}
	return out
}
