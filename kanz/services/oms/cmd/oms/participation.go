package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/services/oms/internal/order"
	"github.com/eighred/kanz/services/oms/internal/tape"
)

// THE REALISED-PARTICIPATION SPINE, AT THE OMS END (#1007).
//
// # Why this is a file rather than a dozen lines in runConsumers
//
// The same two reasons volprofile.go gives: composition_root_length_test.go
// ratchets runConsumers, and the DELIVERY POLICY here is a load-bearing decision
// that does not belong inside a function already wiring nine other folds.
//
// # SubscribeReplay, NOT SubscribeBroadcast
//
// This fold is read ONCE PER DECISION, at the moment a worked parent goes
// terminal, over that parent's WHOLE working window — an hour, a day. Every
// candle on the estate shares one subject string, so DeliverLastPerSubject would
// hand a pod exactly one candle for one instrument and every participation
// measurement would come back UNOBSERVABLE forever, on a live feed. Replay folds
// the subject's retained history, so a pod that has just booted can speak for
// the window a parent it inherited was worked over.
//
// # WHAT IT STILL CANNOT DO, STATED SO A GREEN MEASUREMENT IS NOT OVERREAD
//
// The MARKET stream retains 24h (infra/nats/bootstrap-job.yaml), so a parent
// worked over a longer window than that, on a pod that then rolled, has minutes
// nothing here can vouch for. That surfaces as PARTIAL or UNOBSERVABLE on the
// FACT and in the coverage counter — never as a rate computed from the half that
// was retained, which would be a participation figure for a window nobody
// watched.

// bindRealisedTape builds the realised-volume fold and registers what says
// whether it is working.
//
// The subscription is NOT started here: the caller owns the goroutine and the
// join, so this cannot become a background fold nothing waits for.
func bindRealisedTape(obs *observability.Provider, logger *slog.Logger) *tape.Fold {
	f := tape.NewFold(0)

	// THE REFUSAL AND SERIES COUNTS ARE THE COVERAGE PAIR. A fold that has
	// received nothing and one that has rejected every message both answer "that
	// window is unobservable" to every question, and only these separate a quiet
	// feed from a producer this build cannot read — the same distinction
	// bindVolumeProfiles registers for the forecast curve.
	obs.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_oms_realised_volume_series",
		Help: "Instrument/venue series for which this OMS holds at least one 1-minute candle. " +
			"Zero means no worked order's REALISED participation can be measured here, so no " +
			"participation cap can be shown to have held.",
	}, func() float64 { _, _, _, series := f.Stats(); return float64(series) }))
	obs.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_oms_realised_volume_candles_folded",
		Help: "1-minute candles this OMS has folded since start.",
	}, func() float64 { folded, _, _, _ := f.Stats(); return float64(folded) }))
	obs.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_oms_realised_volume_candles_refused",
		Help: "Candle messages this OMS could not read — undecodable, unnamed, or carrying a " +
			"Decimal outside the exponent domain. Non-zero with a quiet fold means the producer " +
			"is speaking and this build is refusing it.",
	}, func() float64 { _, refused, _, _ := f.Stats(); return float64(refused) }))

	logger.Info("oms will fold the realised candle series for participation measurement",
		"subject", tape.Subject, "retention", tape.DefaultRetention)
	return f
}

// participationCounters registers the two signals the participation measurement
// exports, and returns them in the order the service binds them.
//
// # THEY ARE REGISTERED UNCONDITIONALLY, NOT BEHIND A FEED OR A BROKER BRANCH
//
// Registering a collector inside `if feed != nil` has shipped twice on this
// estate (#973, #963) and the failure is always the same: the deployment with no
// feed exports NO SERIES, so every rule written over the counter compares
// against an empty vector and is silent in exactly the state it was written for.
// An OMS with no tape bound must export a RISING `unobservable`, because that is
// the finding.
func participationCounters(obs *observability.Provider) (participations, capExceeded *prometheus.CounterVec) {
	participations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kanz_oms_participation_measurements_total",
		Help: "Terminal DECISIONS by whether their REALISED participation could be measured. " +
			"not_worked = the order was not worked as a schedule, so it has no intervals; " +
			"measured = every interval it traded in had a candle behind it; partial = some did; " +
			"unobservable = none did, so its participation cap has been neither confirmed nor " +
			"contradicted. NOT a rate of zero — an interval with no candle is UNKNOWN, and a " +
			"forecast is most wrong exactly when the tape is thin.",
	}, []string{"quality"})
	capExceeded = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kanz_oms_participation_cap_exceeded_total",
		Help: "Worked decisions whose measured WORST interval exceeded the participation cap they " +
			"were admitted under. POV enforces its cap against the forecast volume profile at " +
			"admission; this is the count of times the tape came in thinner than that forecast " +
			"and the children were sized for volume that did not arrive. Every one of these is a " +
			"control that did not hold on a market that was signalled to.",
	}, []string{"venue", "instrument"})
	obs.Registry.MustRegister(participations, capExceeded)

	// SEEDED AT ZERO FOR EVERY QUALITY, for the reason executionAttributionCounter
	// seeds its outcomes: a CounterVec exports no series for a label value it has
	// never been incremented with, so an OMS four minutes old and one that has
	// measured nothing all week are the same observation — nothing — unless the
	// zeros are there. The list comes from order.ParticipationQualities rather
	// than being retyped, so a quality added in the service cannot be live in the
	// code and absent from the metric.
	//
	// THE BREACH COUNTER IS NOT SEEDED AND MUST NOT BE. Its labels are a venue
	// and an instrument, which are only knowable from a decision that actually
	// breached — there is no honest zero to write, and inventing one would put a
	// venue/instrument pair on a dashboard that this OMS may never have traded.
	// The rule over it is `increase(...) > 0`, which needs no seed: an event
	// counter that has never fired has nothing to say.
	for _, q := range order.ParticipationQualities {
		participations.WithLabelValues(q)
	}
	return participations, capExceeded
}

// foldRealisedTape runs the candle subscription until ctx ends.
//
// armed fires once the retained history that existed at subscribe time has been
// folded — before which an empty fold means NOTHING rather than "this market was
// not observed". Said out loud when it is empty, because from then on every
// participation measurement this pod publishes is UNOBSERVABLE and that reads
// from a report exactly like a market nobody trades.
func foldRealisedTape(ctx context.Context, consumer *bus.Consumer, f *tape.Fold, logger *slog.Logger) error {
	armed := func() {
		folded, refused, _, series := f.Stats()
		logger.Info("oms has folded the retained candle history",
			"subject", tape.Subject, "series", series, "candles", folded, "refused", refused)
		if series == 0 {
			logger.Warn("NO 1-MINUTE CANDLE REACHED THIS OMS — every worked order's REALISED "+
				"participation will be published as UNOBSERVABLE, so no participation cap can be "+
				"shown to have held. Either no market-ingest is publishing candles, or this "+
				"account holds no subscribe grant on the subject",
				"subject", tape.Subject, "refused", refused)
		}
	}
	err := consumer.SubscribeReplay(ctx, tape.Subject, f.Handle, armed)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
