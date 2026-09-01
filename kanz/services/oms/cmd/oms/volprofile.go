package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/volprofilefeed"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
)

// THE VOLUME-PROFILE SPINE, AT THE OMS END (#897).
//
// # Why this is a file rather than four lines in runConsumers
//
// Two reasons, and the first is arithmetic:
// test/arch/composition_root_length_test.go ratchets runConsumers and it has
// tens of lines of headroom. The second is that the subscription's DELIVERY
// POLICY is the load-bearing decision here, and a decision worth this much
// explanation does not belong inside a function that already wires eight others.
//
// # SubscribeReplay, NOT SubscribeBroadcast
//
// Every other replicated-state fold in this service — the cash spine, the price
// spine, risk measures, venue margin — is SubscribeBroadcast, which delivers the
// LAST message per subject and is exactly right for them: each is a projection of
// a current value, and the newest one is the whole answer.
//
// A volume profile is not. This registry is version-ADDRESSED: a parent order
// pins the profile version its schedule was derived against, and a pod must be
// able to resolve that version for as long as the parent is being worked — which
// spans session boundaries and therefore spans several versions. Every profile on
// the subject shares one subject string, so DeliverLastPerSubject would hand a
// cold pod ONE curve for ONE instrument and nothing else: every other series
// would read as "nobody has measured this", and every parent pinned to anything
// but the single newest message would stop advancing.
//
// SubscribeReplay folds the subject's whole retained history, so a pod that has
// just booted holds what the pod it replaced held — which is the entire reason
// #897 chose a published FACT over a per-pod fold.
//
// # WHAT IT STILL CANNOT DO, STATED SO THE PIN IS NOT OVERREAD
//
// The MARKET stream retains 24h (infra/nats/bootstrap-job.yaml). A pod that boots
// can therefore rebuild only the versions published inside that window. A parent
// pinned to a version older than the retention — one worked across more than a
// day, on a pod that then rolled — resolves to nothing, and a volume-driven
// schedule refuses on it. That is the fail-closed direction and it is loud (the
// driver reports an unworkable schedule and kanz_oms_schedule_failures_total
// moves), but it is a real bound and lengthening the stream's retention is what
// would move it, not anything in this file.

// bindVolumeProfiles builds the registry and starts the fold.
//
// It returns the registry to bind into the order service, and a function to run
// the subscription. The subscription is NOT started here: the caller owns the
// goroutine and the join, so this cannot become a background fold nothing waits
// for.
func bindVolumeProfiles(obs *observability.Provider, logger *slog.Logger) *volprofilefeed.Registry {
	reg := volprofilefeed.NewRegistry(0)

	// THE REFUSAL COUNT IS THE ONE THAT MATTERS. A registry that has folded
	// nothing and one that has rejected everything both answer "no profile" to
	// every question, and only this separates a quiet feed from a producer this
	// build cannot read — which is exactly the "nothing configured" / "checked,
	// and fine" distinction, at the consumer end of a wire.
	obs.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_oms_volume_profiles_series",
		Help: "Instrument/venue series for which this OMS holds at least one published intraday " +
			"volume profile. Zero means no VWAP or POV order can be admitted here.",
	}, func() float64 { _, _, _, series := reg.Stats(); return float64(series) }))
	obs.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_oms_volume_profiles_folded",
		Help: "Published intraday volume profiles this OMS has folded since start.",
	}, func() float64 { folded, _, _, _ := reg.Stats(); return float64(folded) }))
	obs.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_oms_volume_profiles_refused",
		Help: "Published intraday volume profiles this OMS could not read — a decode failure, or " +
			"a message whose version does not hash to its own content. Non-zero with a quiet " +
			"registry means the producer is speaking and this build is refusing it.",
	}, func() float64 { _, refused, _, _ := reg.Stats(); return float64(refused) }))

	logger.Info("oms will fold the intraday volume profile spine",
		"subject", volprofilefeed.Subject, "retention", volprofilefeed.DefaultRetention)
	return reg
}

// foldVolumeProfiles runs the subscription until ctx ends.
//
// A REPLAY SUBSCRIPTION BLOCKS FOR THE LIFE OF THE PROCESS, which is what makes
// the registry CONVERGE rather than merely start correct: the backlog is folded
// first and then new versions keep arriving. armed fires once the history that
// existed at subscribe time has been folded — before which an empty registry
// means NOTHING rather than "no profile exists".
func foldVolumeProfiles(ctx context.Context, consumer *bus.Consumer,
	reg *volprofilefeed.Registry, logger *slog.Logger) error {

	armed := func() {
		_, _, _, series := reg.Stats()
		logger.Info("oms has folded the retained volume-profile history",
			"subject", volprofilefeed.Subject, "series", series, "registry", reg.String())
		if series == 0 {
			// SAID OUT LOUD, because from here on an empty registry is
			// indistinguishable from a market nobody has measured — and the
			// refusal a desk sees will name the market.
			logger.Warn("NO INTRADAY VOLUME PROFILE REACHED THIS OMS — every VWAP and POV order "+
				"will be refused at admission under NO_VOLUME_PROFILE. Either no market-ingest "+
				"is publishing (MARKET_INGEST_VOLUME_PROFILE_MIN_SESSIONS unset there), or this "+
				"account holds no subscribe grant on the subject",
				"subject", volprofilefeed.Subject)
		}
	}
	err := consumer.SubscribeReplay(ctx, volprofilefeed.Subject, reg.Handle, armed)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
