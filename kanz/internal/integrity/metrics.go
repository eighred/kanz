package integrity

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is the DATA-08 Prometheus exporter (OBS-01b): a thin translation
// layer that turns the DATA-01..05 detector results into the exact
// kanz_data_* series the dashboards (kanz/infra/observability/dashboards) and
// DATA-09 alert rules query. The detectors stay pure in-memory classifiers;
// the orchestrator that owns them calls these Observe* methods per result so
// the metric names + label sets are the single contract between detection and
// the dashboards. The names/labels here MUST match the dashboards' README
// table verbatim — OBS-01e pins them so a rename can't silently break a panel.
type Metrics struct {
	now func() time.Time

	stalenessLag     *prometheus.GaugeVec   // subject, partition_key
	lastEventAge     *prometheus.GaugeVec   // subject, partition_key
	gapMissing       *prometheus.CounterVec // subject, partition_key, source
	reconcilePending *prometheus.GaugeVec   // transport
	reconcileDiscrep *prometheus.CounterVec // seen_on, missing_on
	reconcileLatency prometheus.Histogram   // (no labels)
	driftScore       *prometheus.GaugeVec   // feature, metric
	driftThreshold   *prometheus.GaugeVec   // feature, metric
	qualityEvents    *prometheus.CounterVec // kind, severity, subject
}

// NewMetrics constructs the exporter and registers every collector on reg
// (the OBS-01a Provider.Registry). It panics if a series is already
// registered — registration is process-init, and a duplicate is a wiring bug.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		now: time.Now,
		stalenessLag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kanz_data_staleness_lag_seconds",
			Help: "ingestion_time - event_time (DATA-02 Lag).",
		}, []string{"subject", "partition_key"}),
		lastEventAge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kanz_data_last_event_age_seconds",
			Help: "now - last_event_time — stale-frontier age (DATA-02).",
		}, []string{"subject", "partition_key"}),
		gapMissing: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_data_gap_missing_total",
			Help: "missing producer_sequence values detected (DATA-01 MissingCount).",
		}, []string{"subject", "partition_key", "source"}),
		reconcilePending: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kanz_data_reconcile_pending",
			Help: "events awaiting cross-transport confirmation (DATA-05 PendingCount).",
		}, []string{"transport"}),
		reconcileDiscrep: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_data_reconcile_discrepancies_total",
			Help: "one-sided events that aged past MatchDeadline (DATA-05).",
		}, []string{"seen_on", "missing_on"}),
		reconcileLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "kanz_data_reconcile_match_latency_seconds",
			Help:    "wall-clock NATS↔Kafka match gap (DATA-05 MatchLatency).",
			Buckets: []float64{0.1, 0.5, 1, 2.5, 5, 10, 30, 60},
		}),
		driftScore: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kanz_data_drift_score",
			Help: "latest drift score (DATA-04 Score).",
		}, []string{"feature", "metric"}),
		driftThreshold: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kanz_data_drift_threshold",
			Help: "configured alarm threshold (DATA-04 Threshold).",
		}, []string{"feature", "metric"}),
		qualityEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_data_quality_events_total",
			Help: "DataQualityEvents published (DATA-07), by kind/severity.",
		}, []string{"kind", "severity", "subject"}),
	}
	reg.MustRegister(
		m.stalenessLag, m.lastEventAge, m.gapMissing,
		m.reconcilePending, m.reconcileDiscrep, m.reconcileLatency,
		m.driftScore, m.driftThreshold, m.qualityEvents,
	)
	return m
}

// ObserveStaleness records one StalenessResult: the lag gauge and the
// stale-frontier age (now - LastEventTime). StalenessUnknown carries no
// subject, so it is dropped rather than emitting an empty-label series.
func (m *Metrics) ObserveStaleness(res StalenessResult) {
	if res.Level == StalenessUnknown {
		return
	}
	m.stalenessLag.WithLabelValues(res.Subject, res.PartitionKey).Set(res.Lag.Seconds())
	m.lastEventAge.WithLabelValues(res.Subject, res.PartitionKey).Set(m.now().Sub(res.LastEventTime).Seconds())
}

// ObserveGap records a detected sequence gap. Non-gap statuses are no-ops
// (only a gap loses data); the missing count is added so a burst of gaps
// accumulates. subject = the stream (envelope event_type).
func (m *Metrics) ObserveGap(res GapResult) {
	if res.Status != StatusGap {
		return
	}
	m.gapMissing.WithLabelValues(res.Key.EventType, res.Key.PartitionKey, res.Key.Source).
		Add(float64(res.MissingCount()))
}

// ObserveReconcile records the match latency when an event is confirmed on
// both transports. Other statuses are no-ops; pending is a snapshot gauge set
// via SetReconcilePending, and discrepancies come from the sweep.
func (m *Metrics) ObserveReconcile(res ReconcileResult) {
	if res.Status == ReconcileMatched {
		m.reconcileLatency.Observe(res.MatchLatency.Seconds())
	}
}

// SetReconcilePending sets the pending gauge for one transport. The periodic
// sweep job (which owns the Reconciler) supplies the per-side count — the
// Reconciler's PendingCount is the aggregate, so the caller breaks it down by
// the side each pending entry was seen on.
func (m *Metrics) SetReconcilePending(transport Transport, count int) {
	m.reconcilePending.WithLabelValues(transport.String()).Set(float64(count))
}

// ObserveDiscrepancies increments the discrepancy counter for each one-sided
// event the sweep aged out, labelled by which transport saw it / missed it.
func (m *Metrics) ObserveDiscrepancies(discrepancies []Discrepancy) {
	for _, d := range discrepancies {
		m.reconcileDiscrep.WithLabelValues(d.SeenOn.String(), d.MissingOn.String()).Inc()
	}
}

// ObserveDrift records a drift assessment: the latest score and the
// configured threshold (the alert rules compare them with `> on(feature,
// metric)`, so both must be exported, drifted or not).
func (m *Metrics) ObserveDrift(res DriftResult) {
	m.driftScore.WithLabelValues(res.Feature, string(res.Metric)).Set(res.Score)
	m.driftThreshold.WithLabelValues(res.Feature, string(res.Metric)).Set(res.Threshold)
}

// ObserveQualityEvent counts one published DataQualityEvent (DATA-07). kind ∈
// {gap,staleness,drift}, severity ∈ {warning,critical}; passed as strings to
// keep this exporter decoupled from the observation schema (the Publisher owns
// that vocabulary).
func (m *Metrics) ObserveQualityEvent(kind, severity, subject string) {
	m.qualityEvents.WithLabelValues(kind, severity, subject).Inc()
}
