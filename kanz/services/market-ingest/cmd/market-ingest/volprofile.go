package main

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/volprofilefeed"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/services/market-ingest/internal/config"
)

// THE VOLUME-PROFILE POSTURE (#897).
//
// # Why this is a function and not four lines in run()
//
// Because the OFF state has to be as loud as the ON state, and saying so takes
// more room than building the thing. A market-ingest with no session floor
// configured publishes no profile, so every VWAP and POV order in the estate is
// refused at admission under NO_VOLUME_PROFILE — a refusal whose text names the
// MARKET, because from the OMS's side "nobody has measured this instrument" and
// "the edge was never told how much history to require" are the same silence.
//
// The gauge is what separates them, and it is the same device
// services/accounting's entry_source_posture.go uses for the corporate-action
// feed it does not have: a deployment that does not do a thing must say so as a
// number, or a zero downstream means neither.

// volumeProfileCollector builds the fold, or reports that this deployment does
// not publish one.
//
// A nil collector with a nil error is the OFF state, and pkg/alpha.Runner reads
// it as "do not fold, do not sweep, publish nothing". An error is a
// MISCONFIGURATION — a floor that was named and could not be honoured — and the
// composition root refuses to start on it, because a value somebody set and this
// process silently ignored is worse than one nobody set.
func volumeProfileCollector(cfg config.Config, pub volprofilefeed.Publisher,
	obs *observability.Provider, logger *slog.Logger) (*volprofilefeed.Collector, error) {

	publishing := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "kanz_market_ingest_volume_profile_publishing",
		Help: "1 when this market-ingest folds and publishes an intraday volume profile, 0 when " +
			"MARKET_INGEST_VOLUME_PROFILE_MIN_SESSIONS is unset. At 0 no VWAP or POV order can be " +
			"admitted anywhere on the platform, and the refusal names the market rather than this.",
	})
	obs.Registry.MustRegister(publishing)

	if cfg.VolumeProfileMinSessions <= 0 {
		publishing.Set(0)
		logger.Warn("NO INTRADAY VOLUME PROFILE IS PUBLISHED BY THIS DEPLOYMENT — "+
			"MARKET_INGEST_VOLUME_PROFILE_MIN_SESSIONS is unset, so the fold is not built and the "+
			"OMS will refuse every VWAP and POV order under NO_VOLUME_PROFILE. Set it to the "+
			"number of completed sessions this desk requires before it will schedule an order "+
			"against a measured curve; there is deliberately no default, because that number is "+
			"an execution-policy decision and inventing one would either refuse a healthy profile "+
			"or let a two-day sample set a month's schedule",
			"metric", "kanz_market_ingest_volume_profile_publishing")
		return nil, nil
	}

	c, err := volprofilefeed.NewCollector(volprofilefeed.Config{
		Publisher: pub,
		// The SAME tenant every other market.v1 FACT this service publishes
		// carries. A profile belongs to no fund — every tenant's BTC-USDT trades
		// on the same book — so it rides the market-data tenant exactly as the
		// candles and the book snapshots do.
		Tenant:      cfg.Tenant,
		MinSessions: cfg.VolumeProfileMinSessions,
		Bucket:      cfg.VolumeProfileBucket,
		Horizon:     cfg.VolumeProfileHorizon,
		Logger:      logger,
	})
	if err != nil {
		publishing.Set(0)
		return nil, err
	}
	publishing.Set(1)
	logger.Info("intraday volume profile publishing",
		"subject", volprofilefeed.Subject,
		"min_sessions", cfg.VolumeProfileMinSessions,
		"fold", c.Store().String())
	return c, nil
}
