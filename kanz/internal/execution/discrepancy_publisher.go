package execution

import (
	"context"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/prometheus/client_golang/prometheus"
)

type discrepancyPublisher struct {
	inner    Publisher
	outcomes *prometheus.CounterVec
}

// NewDiscrepancyPublisher measures observed discrepancies, not repairs. A
// successful publish establishes delivery only; it never means money was fixed.
func NewDiscrepancyPublisher(inner Publisher, reg prometheus.Registerer) Publisher {
	outcomes := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kanz_venue_discrepancy_publish_total", Help: "Venue discrepancy publication outcomes; observations require investigation, never imply repair."}, []string{"kind", "result"})
	reg.MustRegister(outcomes)
	for _, kind := range []string{"order", "balance"} {
		for _, result := range []string{"published", "failed"} {
			outcomes.WithLabelValues(kind, result)
		}
	}
	return &discrepancyPublisher{inner, outcomes}
}

func (p *discrepancyPublisher) Publish(ctx context.Context, e bus.Event) error {
	err := p.inner.Publish(ctx, e)
	kind := ""
	switch e.Subject {
	case SubjectStateHealed:
		kind = "order"
	case SubjectBalanceRecon:
		kind = "balance"
	}
	if kind != "" {
		result := "published"
		if err != nil {
			result = "failed"
		}
		p.outcomes.WithLabelValues(kind, result).Inc()
	}
	return err
}
