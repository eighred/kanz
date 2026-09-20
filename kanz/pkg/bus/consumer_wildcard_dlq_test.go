package bus_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
)

func TestWildcardDLQUsesDeliveredSubjectAtEveryFailureBoundary(t *testing.T) {
	invalid := validEnvelope()
	invalid.EventId = ""
	for _, tc := range []struct {
		name     string
		body     []byte
		attempts string
	}{
		{"unframe", []byte("bad frame"), "0"},
		{"validation", frame(t, invalid, nil), "0"},
		{"handler", frame(t, validEnvelope(), []byte("payload")), "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const concrete = "market.crypto.unknown"
			sub := &oneShotSub{msg: bus.Message{Subject: concrete, Key: []byte("instrument"), Body: tc.body}}
			dlq := &captureClient{}
			consumer, err := bus.NewConsumer(sub, bus.WithDLQ(dlq))
			if err != nil {
				t.Fatal(err)
			}
			err = consumer.Subscribe(context.Background(), "market.>", "g", func(context.Context, *envelopepb.Envelope, []byte) error {
				if tc.name != "handler" {
					t.Fatal("invalid wire message dispatched")
				}
				return errors.New("unsupported event type")
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(dlq.sent) != 1 {
				t.Fatalf("parked %d messages", len(dlq.sent))
			}
			got := dlq.sent[0]
			if got.Subject != "dlq."+concrete || got.Headers[bus.HeaderDLQOriginalSubject] != concrete {
				t.Fatalf("wildcard filter leaked into DLQ/redrive address: %+v", got)
			}
			if got.Headers[bus.HeaderDLQAttempts] != tc.attempts || !bytes.Equal(got.Body, tc.body) || string(got.Key) != "instrument" {
				t.Fatal("DLQ lost original frame, partition key or failure-stage metadata")
			}
		})
	}
}

func TestWildcardDLQWithoutConcreteSubjectRefusesAcknowledgement(t *testing.T) {
	for _, subject := range []string{"", "market.>", "market.*.trade"} {
		sub := &oneShotSub{msg: bus.Message{Subject: subject, Body: []byte("bad frame")}}
		dlq := &captureClient{}
		consumer, _ := bus.NewConsumer(sub, bus.WithDLQ(dlq))
		err := consumer.Subscribe(context.Background(), "market.>", "g", func(context.Context, *envelopepb.Envelope, []byte) error { return nil })
		if err == nil || len(dlq.sent) != 0 {
			t.Fatalf("subject=%q: invalid DLQ destination acknowledged/published: %v", subject, err)
		}
	}
}

func TestPlanRedriveRefusesHistoricalWildcardDestination(t *testing.T) {
	for _, subject := range []string{"market.>", "market.*.trade"} {
		msg := parkedMsg(func(headers map[string]string) { headers[bus.HeaderDLQOriginalSubject] = subject })
		msg.Subject = "dlq." + subject
		if _, err := bus.PlanRedrive(msg, bus.RedriveOptions{}, time.Now()); !errors.Is(err, bus.ErrRedriveRefused) {
			t.Errorf("historical wildcard destination %q must require investigation, got %v", subject, err)
		}
	}
}

func TestWildcardDLQMetricsKeepSubscriptionCardinality(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := bus.NewBusMetrics(reg)
	dlq := &captureClient{}
	for i := range 10 {
		sub := &oneShotSub{msg: bus.Message{Subject: fmt.Sprintf("market.crypto.instrument%d", i), Body: frame(t, validEnvelope(), nil)}}
		consumer, _ := bus.NewConsumer(sub, bus.WithDLQ(dlq), bus.WithBusMetrics(metrics))
		if err := consumer.Subscribe(context.Background(), "market.>", "g", func(context.Context, *envelopepb.Envelope, []byte) error { return errors.New("refused") }); err != nil {
			t.Fatal(err)
		}
	}
	if len(dlq.sent) != 10 {
		t.Fatalf("parked %d of 10 deliveries", len(dlq.sent))
	}
	got, ok := counterByLabels(t, reg, "kanz_bus_dlq_parked_total", map[string]string{"subject": "market.>", "group": "g", "class": bus.ClassTransient})
	if !ok || got != 10 {
		t.Fatalf("subscription counter=%v, present=%v; concrete subjects must not create per-delivery metric labels", got, ok)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "kanz_bus_dlq_parked_total" && len(family.Metric) != 1 {
			t.Fatalf("DLQ created %d metric series for one subscription", len(family.Metric))
		}
	}
}
