// compliance binary entrypoint (COMP-01). Runs the post-trade monitor: it keeps
// each portfolio's mandate current from the mandate ConfigChanged stream
// (COMP-01f), re-evaluates books on every position change (COMP-01d), emits a
// FACT-grade ComplianceBreach on a passive breach (feeding AUTO-01), and records
// every decision to the audit stream (COMP-01e). Without COMPLIANCE_NATS_URL it
// serves HTTP/probes only (no consumption).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/cashview"
	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/compliancebus"
	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/compliance/internal/api"
	"github.com/eighred/kanz/services/compliance/internal/config"
	"github.com/eighred/kanz/services/compliance/internal/monitor"
	"github.com/eighred/kanz/services/compliance/internal/server"
	"github.com/eighred/kanz/services/compliance/internal/store"
)

// A POST-TRADE BREACH RECORD THAT NEVER REACHED THE AUDIT TRAIL (#622).
//
// The monitor writes it best-effort and that is right — an audit-sink outage
// must not become a trading outage. What was missing is this number, and the
// failure has a shape that makes a log line insufficient: the record's
// EventTime comes from the decision's evaluated_at and the producer refuses a
// zero, so an upstream that forgets to stamp it makes EVERY record fail. A
// sink failing every time looks exactly like a sink that is quiet (#245) —
// the audit trail empty while trading continues and every probe green.
//
// SEEDED AT ZERO so "no records lost" and "no metric" are different answers.
var breachRecordsLost = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "kanz_compliance_breach_records_lost_total",
	Help: "Post-trade breach decisions that could not be written to the audit trail. Non-zero " +
		"means the compliance record is incomplete while monitoring continues.",
})

func main() {
	// The lifecycle lives in run() because os.Exit skips defers: every defer
	// run() registers fires before this line. The non-zero code is what makes a
	// fatal halt distinguishable from a graceful SIGTERM — both otherwise exit 0
	// with reason "Completed" in the pod's termination record (#266).
	// 2 = startup failure, 1 = run loop died after startup, 0 = clean shutdown.
	os.Exit(run())
}

func run() int {
	cfg, err := config.Load()
	if err != nil {
		slog.Default().Error("config load failed", "err", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Records WHY we are stopping so the exit code can say so. Every site that
	// calls fatal.Raise below already brought the process down with stop(); the
	// only thing added is that the reason survives to the exit status (#266).
	fatal := lifecycle.NewFatal(stop)

	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})
	obs, err := observability.New(ctx, observability.Config{
		ServiceName:    cfg.Source,
		ServiceVersion: version.String(),
		OTLPEndpoint:   cfg.OTLPEndpoint,
		SampleRatio:    1,
	}, base)
	if err != nil {
		slog.Default().Error("observability init failed", "err", err)
		return 2
	}
	logger := obs.Logger
	slog.SetDefault(logger)
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = obs.Shutdown(shutCtx)
	}()

	readiness := &server.Readiness{}
	httpSrv := httpserver.New(cfg.Listen, server.New(readiness, logger, server.WithMetrics(obs.MetricsHandler())), httpserver.Standard())
	go func() {
		logger.Info("compliance listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			fatal.Raise(err)
		}
	}()

	var runErr error
	if cfg.NATSURL != "" {
		if err := runConsumers(ctx, cfg, readiness, logger, obs, fatal); err != nil {
			logger.Error("compliance consumers stopped with error", "err", err)
			runErr = err
		}
	} else {
		readiness.Set(true)
		// AND THE MANDATE-CHANGE SURFACE IS NOT MOUNTED EITHER (#562), which is
		// said here rather than left to a 404 somebody meets later. Approving a
		// mandate change publishes a FACT; with no spine there is nothing to
		// publish to, and a route that accepts a second signature and drops it is
		// strictly worse than one that is absent.
		logger.Warn("no COMPLIANCE_NATS_URL set — serving HTTP/probes only: no consumption, and " +
			"NO mandate-change surface, so this deployment answers 404 to POST /v1/portfolios/" +
			"{id}/mandate and nobody can change a mandate through it")
		<-ctx.Done()
		readiness.Set(false)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}

	// The run loop's own error and any fatal raised from a goroutine answer the
	// same question — why is this process stopping — so they meet here, after
	// every shutdown step above has run. Raise(nil) is a no-op mark.
	fatal.Raise(runErr)
	return fatal.Code()
}

// runConsumers wires the bus producer + monitor and subscribes the mandate and
// position streams. The mandate consumer feeds the registry the monitor resolves
// against, so it is subscribed first.
func runConsumers(ctx context.Context, cfg config.Config, readiness *server.Readiness, logger *slog.Logger, obs *observability.Provider, fatal *lifecycle.Fatal) error {
	busMetrics := bus.NewBusMetrics(obs.Registry)

	// SEC-M3: the production broker requires a client SVID; a nil TLSConfig is a
	// plaintext client it refuses at the handshake.
	mesh, err := transport.NewMesh(ctx, cfg.SPIFFESocket)
	if err != nil {
		return err
	}
	defer func() { _ = mesh.Close() }()
	logger.Info("bus transport", "mtls", mesh.Enabled())
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source, TLSConfig: mesh.Client, Metrics: busMetrics})
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source:          cfg.Source,
		ProducerVersion: version.String(),
		Metrics:         busMetrics,
	})
	if err != nil {
		return err
	}

	// ACT TWO OF #410, ON ITS OWN LISTENER (#562).
	//
	// A mandate is the control every order is checked against, and until now
	// changing one took two INVOCATIONS of cmd/kanz-mandate rather than two
	// people: both ran on one operator's machine under one SVID, so the proposal
	// file was a carrier and not a signature. These routes are the propose/approve
	// pair as two SEPARATELY AUTHENTICATED requests, which is what makes a
	// unilateral change preventable rather than merely detectable.
	//
	// MOUNTED HERE, INSIDE THE BUS BRANCH, because the approve route publishes a
	// ConfigChanged FACT and there is nothing to publish to without a spine. A
	// probes-only deployment answers 404 to these paths, which is true; a route
	// that took a second signature and dropped it would be the silent failure the
	// control exists to end.
	//
	// THE PROPOSAL STORE IS IN-PROCESS, AND THAT IS A STATED POSTURE. This
	// deployment is replicas: 1 with strategy Recreate — a correctness bound the
	// monitor already cannot cross (see infra/deploy/compliance-deploy.yaml) — so
	// the multi-replica failure that makes an in-process store wrong elsewhere,
	// the second signature landing on a pod that never saw the first, cannot arise
	// without breaking that pin first. What it does cost is said out loud below.
	proposals := store.NewMemoryProposals()
	logger.Info("compliance: mandate-change surface armed (#562, #410 act two)",
		"addr", cfg.APIListen,
		"proposal_store", "in-process",
		"note", "a restart drops every PENDING mandate proposal — nothing was published, the "+
			"mandate in force is unchanged, and the proposer must propose again. Valid only at "+
			"replicas: 1, which this deployment is pinned to for the monitor's own reasons.")
	mandateSrv := httpserver.New(cfg.APIListen,
		api.New(proposals, comp.NewPublisher(producer), logger), httpserver.Standard())
	go func() {
		logger.Info("compliance mandate API listening", "addr", cfg.APIListen)
		if err := mandateSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// FATAL, unlike a dead metrics listener. This is the only surface through
			// which a mandate can be changed by two people; a process that keeps
			// serving probes while it is gone reports healthy with the control
			// unreachable, which is the shape #535 spent an issue on.
			logger.Error("compliance mandate API failed — no mandate can be changed through this "+
				"pod", "err", err)
			fatal.Raise(err)
		}
	}()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = mandateSrv.Shutdown(shutCtx)
	}()
	go purgeLapsedProposals(ctx, proposals, logger)

	// COMP-01f: mandate registry fed from the shared ConfigChanged stream, the
	// point-in-time source the monitor resolves against.
	mandateReg := comp.NewMandateRegistry(comp.WithMandateLogger(logger))
	// A mandate this monitor consumed and could not apply (#619). The stream is
	// compacted, so the failure is permanent until the mandate is republished: the
	// portfolio stops being evaluated post-trade, and nothing else says so.
	mandateRejected := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_compliance_mandate_rejected_total",
		Help: "Mandate messages this process consumed and could not apply. Each one leaves a " +
			"portfolio unevaluated until the mandate is republished.",
	})
	obs.Registry.MustRegister(mandateRejected)
	mandateConsumer := comp.NewMandateConsumer(mandateReg, logger,
		comp.WithMandateRejectionObserver(func(string, string) { mandateRejected.Inc() }))

	// COMP-01e: decisions to the audit stream. COMP-01d: the post-trade monitor.
	recorder := compliancebus.NewBusRecorder(producer, logger)
	breachEmitter := monitor.NewEmitter(producer)
	// THE INSTRUMENT CLASSIFIER (#640) — the same cache the OMS gate wires, for
	// the same reason and against the same source. This monitor re-evaluates LIVE
	// BOOKS rather than orders, so the consequence of it being unwired was a
	// monitor reporting a fund CLEAN against a sector or issuer exclusion it
	// could not evaluate; unresolvedDimension made that a violation instead, and
	// this makes it an evaluation.
	//
	// Nil when no master is configured, deliberately — see the OMS's comment for
	// why "none wired" must not be spelled like "wired and does not know this
	// instrument".
	refCache, err := cfg.RefData.NewCache(cfg.Tenant, "svc:compliance")
	if err != nil {
		logger.Error("compliance: the instrument classifier refused its configuration", "err", err)
		return err
	}
	cfg.RefData.LogPosture(logger, "compliance")
	var classifier comp.Classifier
	if refCache != nil {
		classifier = refCache.Compliance()
	}
	refreshFailures := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_instrument_reference_refresh_failures_total",
		Help: "Reference-data refresh cycles that reported at least one failed lookup. Registered " +
			"before anything can fail so \"none\" is a zero series rather than a missing one.",
	})
	obs.Registry.MustRegister(refreshFailures)
	// ZERO IS A READABLE ANSWER (#622): the gauge exists whether or not a master
	// is wired, so an alert asking "is any classifier armed" finds a series.
	obs.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_instrument_classifier_wired",
		Help: "1 when this pod has a reference-data source for instrument classification. ZERO " +
			"MEANS EVERY SECTOR, ISSUER AND ASSET_CLASS MANDATE RULE IS REFUSED as unverifiable.",
	}, func() float64 {
		if refCache == nil {
			return 0
		}
		return 1
	}))
	obs.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_instrument_classifier_resolved",
		Help: "Instruments this pod can currently classify. Held against " +
			"kanz_instrument_classifier_wanted it is the difference between a warming cache and " +
			"a reference-data gap.",
	}, func() float64 {
		if refCache == nil {
			return 0
		}
		return float64(refCache.Stats().Resolved)
	}))
	obs.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_instrument_classifier_wanted",
		Help: "Instruments asked about and not yet resolved. Each one is a holding whose " +
			"classified-dimension mandate rules are being REFUSED right now.",
	}, func() float64 {
		if refCache == nil {
			return 0
		}
		return float64(refCache.Stats().Wanted)
	}))

	// Registered before anything can drop: a plain Counter exports at zero the
	// moment it is registered, which is what makes "none lost" a readable answer
	// rather than a missing series (#622).
	obs.Registry.MustRegister(breachRecordsLost)

	// WHAT THE POST-TRADE BOOK IS WORTH (#787). A named builder, because while
	// these were locals here nothing could assert that the mark fold got the
	// configured staleness bound or that a cash announcement re-evaluates at all.
	// See valuation.go.
	valuation := buildPostTradeValuation(cfg, obs.Registry, logger)

	mon := monitor.NewMonitor(comp.NewEngine(nil), mandateReg, classifier, breachEmitter, recorder, logger,
		monitor.WithDroppedRecordObserver(func() { breachRecordsLost.Inc() }),
		monitor.WithCashSource(valuation.Cash),
		monitor.WithMarkSource(valuation.Marks))

	consumer, err := bus.NewConsumer(client, bus.WithBusMetrics(busMetrics), bus.WithDLQ(client))
	if err != nil {
		return err
	}

	type sub struct {
		subject string
		handler bus.EventHandler
	}
	// The mandate registry arms by BROADCAST — see the OMS's comment. A durable group
	// here meant a restarted compliance pod came back with an empty registry and
	// silently governed nothing (EXEC-M13).
	// THE MONITOR ARMS ITSELF AT BOOT, exactly as the mandate registry does (EXEC-M20).
	//
	// Its book was filled by a durable consumer GROUP, which resumes at its last ack — so
	// a restarted monitor came back with an EMPTY book and rebuilt it only as new position
	// FACTs happened to arrive. An instrument that did not trade again was simply gone, and
	// a fund holding an instrument its mandate FORBIDS looked compliant, because the holding
	// was not there to see. The control did not fail; it went blind.
	//
	// SubscribeBroadcast delivers DeliverLastPerSubject over the compacted POSITION stream,
	// so a booting pod learns the CURRENT state of every holding in one read. (This is why
	// the service stays at replicas: 1 — broadcast means every pod folds every position and
	// would emit its own duplicate breach FACT. Lifting that pin is a separate decision.)
	var subs []sub
	for _, s := range cfg.MonitorSubjects() {
		subs = append(subs, sub{s, mon.Handle})
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	// THE REFERENCE-DATA REFRESH CYCLE (#640). While it is not running, every
	// classified dimension the monitor evaluates is refused. It logs and retries
	// rather than terminating: a monitor that stops is a fund nothing is watching
	// at all, which is strictly worse than one whose sector rules are refusing.
	// The first cycle runs before the first tick so a fresh pod is not blind for
	// a whole interval. Joined, like every goroutine in this frame.
	if refCache != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(cfg.RefData.RefreshInterval)
			defer ticker.Stop()
			for {
				if n, err := refCache.Refresh(ctx); err != nil {
					refreshFailures.Inc()
					logger.Error("compliance: the instrument reference refresh reported failures — "+
						"the holdings it could not resolve will have their SECTOR, ISSUER and "+
						"ASSET_CLASS mandate rules refused",
						"err", err, "installed_despite_failures", n)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("compliance arming the mandate registry", "subject", comp.SubjectMandateAll)
		// SubscribeBroadcastReady, not SubscribeBroadcast: mandateReg.Arm fires only once
		// DeliverLastPerSubject has drained, i.e. once every portfolio's mandate in force
		// has actually been folded — see the wait loop below for why that distinction is
		// the whole fix.
		err := consumer.SubscribeBroadcastReady(ctx, comp.SubjectMandateAll, mandateConsumer.Handle, mandateReg.Arm)
		if err != nil && !errors.Is(err, context.Canceled) {
			once.Do(func() {
				firstErr = err
				cancel()
			})
		}
	}()
	for _, s := range subs {
		wg.Add(1)
		go func(s sub) {
			defer wg.Done()
			logger.Info("compliance arming the post-trade book", "subject", s.subject)
			err := consumer.SubscribeBroadcast(ctx, s.subject, s.handler)
			if err != nil && !errors.Is(err, context.Canceled) {
				once.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}(s)
	}

	// THE CASH SPINE. Broadcast, like the OMS's: every replica needs the whole
	// balance picture, and a durable group would give one pod the announcements
	// and leave the others valuing books against cash they never saw.
	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("compliance folding the cash spine (broadcast)", "subject", cashview.Subject)
		err := consumer.SubscribeBroadcast(ctx, cashview.Subject, valuation.CashHandler(mon))
		if err != nil && !errors.Is(err, context.Canceled) {
			once.Do(func() {
				firstErr = err
				cancel()
			})
		}
	}()
	// THE PRICE SPINE. No re-evaluation per tick, deliberately: that would couple
	// compliance evaluation to market-data volume, the one rate on this platform
	// nobody controls. The sweep below is what turns a folded mark into a verdict,
	// at a cost bounded by the number of BOOKS rather than the number of ticks.
	for _, subject := range cfg.PriceSubjects {
		wg.Add(1)
		go func(subject string) {
			defer wg.Done()
			logger.Info("compliance folding the price spine (broadcast)", "subject", subject)
			err := consumer.SubscribeBroadcast(ctx, subject, valuation.Marks.Handle)
			if err != nil && !errors.Is(err, context.Canceled) {
				once.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}(subject)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		reevaluateBooks(ctx, mon, cfg.ReevaluateInterval, logger)
	}()

	// READINESS MUST WAIT ON THE MANDATE REPLAY, NOT ON THE SUBSCRIPTION GOROUTINE HAVING
	// STARTED (EXEC-M13).
	//
	// This used to call readiness.Set(true) immediately after LAUNCHING the mandate
	// subscription goroutine above, not after its replay had folded. The post-trade monitor
	// resolves every book re-evaluation against this same registry, and OMS_REQUIRE_MANDATE's
	// fail-open default means the OMS side of this control admits an unmandated portfolio
	// unconstrained — so in the window between this pod reporting Ready and the mandate
	// registry actually catching up, this monitor would have re-evaluated positions against
	// "no mandate in force" for every portfolio, exactly the way a restarted OMS gate did in
	// EXEC-M13 ("a restarted OMS came back with an empty registry and its gate passed every
	// order"), narrowed here from "only on a broken durable-group replay" to "a race on every
	// single rolling restart". The control did not fail loudly. It went blind silently while
	// the health check said everything was fine.
	//
	// This mirrors webhook-ingest's position-cache wait loop
	// (services/webhook-ingest/cmd/webhook-ingest/main.go) exactly, rather than inventing a new
	// shape: poll Armed() on a short tick with a <-ctx.Done() escape, so a shutdown signal that
	// arrives before the replay lands falls through to graceful shutdown WITHOUT ever reporting
	// ready — a pod that never learned which mandates are in force must not be told it is
	// healthy. On a fresh install with zero mandates published, SubscribeBroadcastReady's ready()
	// fires as soon as the (empty) backlog drains, so this does not deadlock a first deployment;
	// it only closes the race on a populated one.
	//
	// This runs concurrently with the post-trade book subscriptions already launched into wg
	// above — they keep running in their own goroutines regardless of how long this wait takes,
	// so a slow mandate replay delays only the readiness flip, never the rest of the subscription
	// group.
mandateArmWait:
	for !mandateReg.Armed() {
		select {
		case <-ctx.Done():
			// Shutting down before the replay landed. Fall through to wg.Wait() below
			// WITHOUT reporting ready: this pod never learned which mandates are in force.
			break mandateArmWait
		case <-time.After(50 * time.Millisecond):
		}
	}
	if mandateReg.Armed() {
		// ARMED IS NOT COMPLETE (#619) — see the OMS composition root for why this
		// still reports ready rather than taking the service down over one bad
		// mandate. Here the narrow failure is that the affected portfolio is not
		// EVALUATED post-trade; the monitor refuses to check it and says so, rather
		// than re-evaluating a live book and reporting it clean.
		if !mandateReg.Complete() {
			logger.Error("mandate registry armed WITH GAPS — some portfolios have a published mandate "+
				"this pod could not apply and are NOT BEING MONITORED until it is republished",
				"subject", comp.SubjectMandateAll,
				"unreadable_portfolios", mandateReg.Rejections(),
				"unattributable_drops", mandateReg.Dropped())
		} else {
			logger.Info("mandate registry armed — mandates in force are known", "subject", comp.SubjectMandateAll)
		}
		readiness.Set(true)
	}

	wg.Wait()
	readiness.Set(false)
	return firstErr
}

// proposalRetention is how long a mandate proposal nobody signed stays LISTED
// after it lapses, and purgeInterval is how often the sweep runs.
//
// THE RETENTION IS NOT ZERO, and that is the #563 lesson applied before it could
// be repeated. A lapsed proposal is the only record that a change was proposed
// and died unsigned — for a mandate that means the portfolio is still governed by
// the old constraint — so purging on expiry would leave the proposer inferring
// the outcome from an absence. A week is long enough for somebody to come back
// from leave and find it.
const (
	proposalRetention = 7 * 24 * time.Hour
	purgeInterval     = time.Hour
)

// purgeLapsedProposals bounds the in-process store.
//
// WITHOUT IT THE STORE ONLY EVER GROWS. A proposal nobody signs is never claimed,
// so every unsigned mandate change would rest in this pod's memory until it
// restarted — and "the process restarts eventually" is not a retention policy, it
// is the absence of one.
func purgeLapsedProposals(ctx context.Context, proposals store.ProposalStore, logger *slog.Logger) {
	ticker := time.NewTicker(purgeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := proposals.PurgeLapsed(ctx, time.Now().UTC().Add(-proposalRetention))
			if err != nil {
				logger.Error("compliance: cannot purge lapsed mandate proposals", "err", err)
				continue
			}
			if n > 0 {
				// COUNTED AND NAMED. These are mandate changes two people never
				// completed; a silent sweep would make "nobody proposed it" and
				// "somebody proposed it and it died" the same absence again.
				logger.Info("compliance: purged mandate proposals that lapsed unsigned",
					"count", n, "retention", proposalRetention)
			}
		}
	}
}
