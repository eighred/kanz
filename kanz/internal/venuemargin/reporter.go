package venuemargin

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	collateralpb "github.com/eighred/kanz/kanz-schemas-go/collateral/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/pkg/bus"
)

// ErrUndated is returned when a source hands back an observation with no
// observation time.
//
// REFUSED, NOT STAMPED WITH now(). Substituting the local clock would make a
// figure of unknown age look freshly observed, and the freshness bound — the
// only thing standing between a control and a ten-minute-old margin ratio during
// a fast move — would then never fire on it. An undated observation is the one
// case where publishing nothing is strictly better than publishing.
var ErrUndated = errors.New("venuemargin: observation carries no observation time — refusing to publish an undatable margin figure")

// Reporter polls a venue for its own margin state and publishes it as a FACT.
//
// IT IS A REPORTER AND NOT A CALCULATOR. Everything it puts on the wire came out
// of execution.VenueMarginSource, which came out of the exchange. Where the
// exchange said nothing, this drops the quantity and records WHY in the
// observation's coverage; it never fills a gap from Kanz's own book, and the
// gaps are the point — a consumer refuses on them.
type Reporter struct {
	src     execution.VenueMarginSource
	pub     execution.Publisher
	venue   string
	account string
	tenant  string
	now     func() time.Time

	onError     func(err error)
	onUncovered func(reason string, count int)
}

// ReporterConfig configures a Reporter.
type ReporterConfig struct {
	Source  execution.VenueMarginSource
	Pub     execution.Publisher
	Venue   string
	Account string
	Tenant  string
	Now     func() time.Time
	// OnError is called when a poll fails or an observation is refused. Nil ⇒
	// silent, which is only ever right in a test: in production the difference
	// between "the venue reports no margin" and "we could not ask" is the whole
	// value of this feed.
	OnError func(err error)
	// OnUncovered is called once per poll per reason with how many quantities the
	// venue did not report. It fires on the OBSERVATION, not on a later lookup,
	// because that is where the fact exists — by lookup time the quantity is
	// simply absent and indistinguishable from one this venue never carries.
	OnUncovered func(reason string, count int)
}

// NewReporter builds a Reporter. A nil Source or Pub yields a nil Reporter,
// whose Run and Report are no-ops: the composition root's Announce is what makes
// that state visible, and duplicating the warning here would give an operator
// two half-answers to the same question.
func NewReporter(cfg ReporterConfig) *Reporter {
	if cfg.Source == nil || cfg.Pub == nil {
		return nil
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Reporter{
		src: cfg.Source, pub: cfg.Pub, venue: cfg.Venue, account: cfg.Account,
		tenant: cfg.Tenant, now: cfg.Now,
		onError: cfg.OnError, onUncovered: cfg.OnUncovered,
	}
}

// Run polls until ctx is cancelled. interval <= 0 ⇒ DefaultInterval.
func (r *Reporter) Run(ctx context.Context, interval time.Duration) {
	if r == nil {
		return
	}
	if interval <= 0 {
		interval = DefaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.Report(ctx); err != nil && ctx.Err() == nil && r.onError != nil {
				r.onError(err)
			}
		}
	}
}

// Report runs one observation and publishes it.
//
// IT PUBLISHES EVEN WHEN THE VENUE REPORTED NOTHING USABLE. An observation whose
// coverage says contributed = 0 is not noise: it is the record that we ASKED and
// the exchange told us nothing, which is a different operational state from this
// feed being silent, and the two are indistinguishable if a fruitless poll
// publishes nothing. The consumer refuses on it either way — what changes is
// whether anybody can tell why.
func (r *Reporter) Report(ctx context.Context) error {
	if r == nil {
		return nil
	}
	obs, err := r.src.MarginState(ctx)
	if err != nil {
		return err
	}
	msg, err := r.build(obs)
	if err != nil {
		return err
	}
	return r.pub.Publish(ctx, bus.Event{
		Subject: Subject, EventType: Subject,
		EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "accounting",
		// EventTime IS THE VENUE'S OBSERVATION TIME, not the publish time. A
		// consumer that bounded freshness on the envelope would otherwise measure
		// how recently we published rather than how recently the exchange looked,
		// and a poller that keeps running against a frozen account would keep
		// refreshing an unchanging number's apparent age.
		EventTime:    obs.ObservedAt.UTC(),
		PartitionKey: r.account,
		TenantID:     r.tenant,
		Payload:      msg,
	})
}

// build converts an observation into the FACT, dropping every quantity the venue
// did not report and recording it as coverage evidence instead.
func (r *Reporter) build(obs execution.VenueMargin) (*collateralpb.VenueMarginState, error) {
	if r.venue == "" || r.account == "" {
		return nil, fmt.Errorf("venuemargin: refusing to publish margin state with no venue/account "+
			"(venue=%q account=%q) — an unattributed margin figure cannot be bound to the portfolio it "+
			"gates, and the account IS the liquidation boundary", r.venue, r.account)
	}
	if obs.ObservedAt.IsZero() {
		return nil, ErrUndated
	}

	msg := &collateralpb.VenueMarginState{
		Venue:          r.venue,
		VenueAccountId: r.account,
		ObservedAt:     timestamppb.New(obs.ObservedAt.UTC()),
		KnowledgeTime:  timestamppb.New(r.now().UTC()),
	}
	cov := &domainpb.InputCoverage{}
	excluded := map[string]int{}
	exclude := func(instrument, reason string) {
		cov.ExcludedCount++
		excluded[reason]++
		// BOUNDED SAMPLE, capped exactly as the risk measures cap theirs: the
		// identity of the first few is what an operator acts on, the count is what
		// tells them how bad it is, and an account with a thousand open positions
		// must not put a thousand entries in every observation.
		if len(cov.Exclusions) < maxExclusions {
			cov.Exclusions = append(cov.Exclusions, &domainpb.InputExclusion{
				InstrumentId: instrument, Reason: reason,
			})
		}
	}

	if d, ok := toDecimal(obs.MaintenanceMargin); ok {
		msg.MaintenanceMargin = d
		cov.Contributed++
	} else {
		exclude("", reasonFor(obs.MaintenanceMargin, SkipNoMaintenanceMargin))
	}
	if d, ok := toDecimal(obs.MarginRatio); ok {
		msg.MarginRatio = d
		cov.Contributed++
	} else {
		exclude("", reasonFor(obs.MarginRatio, SkipNoMarginRatio))
	}
	for _, p := range obs.Positions {
		if p.Symbol == "" {
			continue
		}
		d, ok := toDecimal(p.LiquidationPrice)
		if !ok {
			// NAMED, NOT DROPPED. The exclusion carries the symbol so an operator
			// can see which position's liquidation distance is unknowable. A
			// position silently absent from the list is one nobody can tell apart
			// from a position that does not exist.
			exclude(p.Symbol, reasonFor(p.LiquidationPrice, SkipNoLiquidationPrice))
			continue
		}
		msg.LiquidationPrices = append(msg.LiquidationPrices, &collateralpb.VenueLiquidationPrice{
			VenueSymbol: p.Symbol, Price: d,
		})
		cov.Contributed++
	}
	msg.Coverage = cov

	if r.onUncovered != nil {
		for reason, n := range excluded {
			r.onUncovered(reason, n)
		}
	}
	return msg, nil
}

// maxExclusions caps the sample carried on one observation. It mirrors
// v1.MaxInputExclusions rather than importing it: internal/risk/api/v1 is the
// query surface and pulling it into a venue adapter's dependency set to read one
// integer would couple the exchange connectors to the risk engine's API.
const maxExclusions = 32

// reasonFor distinguishes "the venue said nothing" from "the venue said
// something this platform cannot carry exactly".
//
// BOTH END AS UNKNOWN, and a consumer treats them identically — but they are
// different incidents. The first is the API not carrying the field; the second
// is #94, a figure past what a common.v1.Decimal represents, and it means
// somebody must widen a conversion rather than chase a venue.
func reasonFor(v *big.Rat, missing string) string {
	if v == nil {
		return missing
	}
	return SkipNotRepresentable
}

// toDecimal converts a venue figure, or reports that it cannot.
//
// ToProtoScaled, NOT ToProto (#94). ToProto wraps once the scaled coefficient
// passes an int64 — about 92.2 billion units at scale 8 — and these figures are
// denominated in whatever the account is valued in, which on a crypto venue is
// routinely a token trading in the trillions. A wrapped maintenance margin does
// not report a SMALLER requirement; it reports a different one, and it can turn
// an account about to be liquidated into one that looks comfortable.
//
// ToProtoScaled RAISES THE EXPONENT INSTEAD OF WRAPPING, so a huge figure comes
// back with the right magnitude and fewer significant digits — a rounded margin
// requirement, which is a usable answer. Its false is therefore very nearly
// unreachable (it needs an exponent past MaxInt32) and no test here can reach
// it. The branch stays because the alternative to an unreachable refusal is an
// unchecked bool on a collateral figure, and the exclusion it records costs
// nothing until the day the conversion changes.
func toDecimal(v *big.Rat) (*commonpb.Decimal, bool) {
	if v == nil {
		return nil, false
	}
	d, ok := dec.ToProtoScaled(v)
	if !ok {
		return nil, false
	}
	return d, true
}
