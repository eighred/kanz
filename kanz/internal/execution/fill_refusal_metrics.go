package execution

import "github.com/prometheus/client_golang/prometheus"

// NewFillRefusedCounter publishes the initial overfill sample so a later
// refusal can be observed as an increase rather than a newly appearing series.
func NewFillRefusedCounter(venue string) *prometheus.CounterVec {
	counter := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "kanz_venue_fill_refused_total",
		Help:        "Venue execution reports this adapter refused to publish as a fill FACT, by reason.",
		ConstLabels: prometheus.Labels{"venue": venue},
	}, []string{"reason"})
	counter.WithLabelValues(RefusalKind(ErrVenueOverfill))
	return counter
}
