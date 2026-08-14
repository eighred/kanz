// Package riskview is the OMS's local copy of what the risk engine has computed
// (#438).
//
// # Why the OMS holds risk state at all
//
// Order admission runs, in order: unmarshal, domain check, entitlement,
// duplicate/resume, then the compliance gate. It never asks the risk engine
// anything. Risk on this platform is a downstream OBSERVER of orders, never a
// gate in front of them — so an order can be inside every mandate rule and still
// take the portfolio through its VaR limit, and the platform will admit it,
// execute it, and only then compute how bad it was.
//
// # It does NOT call the risk engine
//
// #438 rules that out for a reason worth restating: a gRPC round trip into a full
// revaluation puts an unbounded, externally-owned latency in front of every
// order, and when risk-engine is degraded you get the worst available failure —
// orders queuing behind a service that is not on the trading path.
// internal/risk/degraded.go exists precisely because degradation is expected.
//
// So this folds risk.portfolio.measures_computed, which risk already publishes,
// and the gate checks arithmetic on a local map: microseconds, no network, and a
// risk-engine outage cannot stop trading — it can only make this view go stale,
// which the freshness bound below turns into a refusal rather than a silent pass.
//
// # Stale is not absent, and neither is zero
//
// A measure is UNKNOWN when it was never announced, or when the last
// announcement is older than the bound. Both are refusals at the rule, never
// substitutions: treating an unknown VaR as zero would admit every order while a
// mandate declares a VaR limit — a control that reports success. That is #261's
// shape exactly, and the reason ErrPositionsNotArmed exists one layer over.
package riskview

import (
	"context"
	"math/big"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/pkg/bus"
)

// Subject is where the risk engine announces computed measures.
//
// DECLARED HERE AND IN internal/risk/publish, because Go's internal-package rule
// forbids the OMS importing the risk module's publisher — and
// test/arch/risk_boundary_test.go enforces that boundary deliberately. A typo
// would leave this view subscribed to a subject nobody publishes, and the only
// symptom would be every risk limit refusing forever with "unknown", which reads
// like a working control.
const Subject = "risk.portfolio.measures_computed"

// DefaultMaxAge bounds how old a measure may be and still gate an order.
//
// A RISK NUMBER THAT CANNOT BE SHOWN CURRENT IS NOT A RISK NUMBER. Measures are
// derived state, recomputed on position change and on a schedule; a lost
// announcement or a stalled engine must not leave the gate checking this
// morning's VaR against this afternoon's book. Aging out yields UNKNOWN, which
// the rule refuses on — the same stance cashview and balancerecon take.
const DefaultMaxAge = 15 * time.Minute

type snapshot struct {
	values map[string]*big.Rat
	asOf   time.Time
}

// View is the folded risk state, keyed by portfolio.
type View struct {
	mu     sync.RWMutex
	byPF   map[string]snapshot
	now    func() time.Time
	maxAge time.Duration

	onStale func(portfolio, measure string, age time.Duration)
}

// Option customizes a View.
type Option func(*View)

// WithClock injects the clock (tests).
func WithClock(now func() time.Time) Option {
	return func(v *View) {
		if now != nil {
			v.now = now
		}
	}
}

// WithMaxAge overrides DefaultMaxAge. Non-positive ⇒ no bound, which is only
// ever right in a test.
func WithMaxAge(d time.Duration) Option { return func(v *View) { v.maxAge = d } }

// WithOnStale is called when a lookup finds a measure too old to gate on.
func WithOnStale(fn func(portfolio, measure string, age time.Duration)) Option {
	return func(v *View) { v.onStale = fn }
}

// New returns an empty view. Until an announcement arrives every measure is
// UNKNOWN, so a portfolio whose risk has never been computed refuses a declared
// risk limit rather than passing it.
func New(opts ...Option) *View {
	v := &View{byPF: map[string]snapshot{}, now: time.Now, maxAge: DefaultMaxAge}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

// Handle folds a domain.v1.RiskMeasureSet. It is a bus.EventHandler.
//
// A LEVEL, NOT A DELTA: this REPLACES the portfolio's measures. Merging would
// leave a measure the engine has stopped producing showing its last value
// forever — and a stale VaR that never ages out is worse than none, because the
// freshness bound would never fire on it.
func (v *View) Handle(_ context.Context, _ *envelopepb.Envelope, payload []byte) error {
	var msg domainpb.RiskMeasureSet
	if proto.Unmarshal(payload, &msg) != nil {
		// A permanent defect in these bytes: nacking replays them forever, while
		// the measures simply age out to unknown and the gate refuses.
		return nil
	}
	// OUT-OF-DOMAIN EXPONENTS ARE REFUSED BEFORE ANY NUMBER IS READ (#95):
	// dec.FromProto materialises 10^abs(exponent).
	if _, in := dec.InDomainDeep(&msg); !in {
		return nil
	}
	pf := msg.GetPortfolioId()
	if pf == "" {
		return nil
	}
	next := make(map[string]*big.Rat, len(msg.GetMeasures()))
	for _, m := range msg.GetMeasures() {
		if m.GetName() == "" {
			continue
		}
		next[m.GetName()] = dec.FromProto(m.GetValue())
	}
	v.mu.Lock()
	v.byPF[pf] = snapshot{values: next, asOf: msg.GetAsOf().AsTime().UTC()}
	v.mu.Unlock()
	return nil
}

// Measure returns the portfolio's current value for a named measure, or
// ok=false when it is UNKNOWN — never announced, not in the last announcement,
// or too old to be shown current.
//
// THE THREE UNKNOWNS ARE ONE ANSWER ON PURPOSE. A caller must not be able to
// treat "the engine does not compute this" differently from "the engine has
// stopped": both mean this gate cannot vouch for the number, and a rule that
// distinguished them would eventually pass one of them.
func (v *View) Measure(portfolioID, name string) (*big.Rat, bool) {
	v.mu.RLock()
	snap, seen := v.byPF[portfolioID]
	v.mu.RUnlock()
	if !seen {
		return nil, false
	}
	if v.maxAge > 0 {
		if age := v.now().UTC().Sub(snap.asOf); age > v.maxAge {
			if v.onStale != nil {
				v.onStale(portfolioID, name, age)
			}
			return nil, false
		}
	}
	val, held := snap.values[name]
	if !held || val == nil {
		return nil, false
	}
	return new(big.Rat).Set(val), true
}

// Stats reports how many portfolios are held and how many are current, for the
// posture gauge. "Held but not current" is the state an operator needs to see
// before a risk limit starts refusing everything.
func (v *View) Stats() (held, live int) {
	now := v.now().UTC()
	v.mu.RLock()
	defer v.mu.RUnlock()
	for _, s := range v.byPF {
		held++
		if v.maxAge <= 0 || now.Sub(s.asOf) <= v.maxAge {
			live++
		}
	}
	return held, live
}

var _ bus.EventHandler = (&View{}).Handle
