package execution

import (
	"context"
	"errors"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"testing"
)

type discrepancyTestPublisher struct{ err error }

func (p *discrepancyTestPublisher) Publish(context.Context, bus.Event) error { return p.err }

func TestDiscrepancyPublisherCountsDeliveryNotRepair(t *testing.T) {
	inner := &discrepancyTestPublisher{}
	p := NewDiscrepancyPublisher(inner, prometheus.NewRegistry()).(*discrepancyPublisher)
	if err := p.Publish(t.Context(), bus.Event{Subject: SubjectStateHealed}); err != nil {
		t.Fatal(err)
	}
	inner.err = errors.New("broker unavailable")
	if err := p.Publish(t.Context(), bus.Event{Subject: SubjectBalanceRecon}); !errors.Is(err, inner.err) {
		t.Fatal("publish failure hidden")
	}
	if testutil.ToFloat64(p.outcomes.WithLabelValues("order", "published")) != 1 || testutil.ToFloat64(p.outcomes.WithLabelValues("balance", "failed")) != 1 || testutil.ToFloat64(p.outcomes.WithLabelValues("balance", "published")) != 0 {
		t.Fatal("outcomes conflated")
	}
}
