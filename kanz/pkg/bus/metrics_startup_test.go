package bus

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestBusStartupConfigurationSeedsTransitionsWithoutBacklog(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "unconfigured", true: "configured_before_connect"}[configured], func(t *testing.T) {
			reg := prometheus.NewRegistry()
			m := NewBusMetrics(reg)
			m.ConfigureClient("risk-engine", configured)
			families, err := reg.Gather()
			if err != nil {
				t.Fatal(err)
			}
			seen := map[string]int{}
			for _, family := range families {
				name := family.GetName()
				seen[name] = len(family.Metric)
				for _, metric := range family.Metric {
					for _, label := range metric.Label {
						if label.GetName() == "client" && label.GetValue() != "risk-engine" {
							t.Fatal("startup used a different client identity")
						}
					}
				}
				switch name {
				case "kanz_bus_configured":
					want := 0.0
					if configured {
						want = 1
					}
					if len(family.Metric) != 1 || family.Metric[0].GetGauge().GetValue() != want {
						t.Fatalf("configuration = %v", family)
					}
				case "kanz_bus_connected":
					if len(family.Metric) != 1 || family.Metric[0].GetGauge().GetValue() != 0 {
						t.Fatalf("invented connection: %v", family)
					}
				case "kanz_bus_connection_events_total":
					events := map[string]bool{}
					for _, metric := range family.Metric {
						if metric.GetCounter().GetValue() != 0 {
							t.Fatal("startup invented an event")
						}
						for _, label := range metric.Label {
							if label.GetName() == "event" {
								events[label.GetValue()] = true
							}
						}
					}
					if len(events) != 3 || !events["disconnected"] || !events["reconnected"] || !events["closed"] {
						t.Fatalf("transition inventory = %v", events)
					}
				case "kanz_bus_consumer_lag", "kanz_bus_pending_messages", "kanz_bus_backlog_poll_last_success_timestamp_seconds":
					t.Fatalf("startup fabricated a broker reading: %s", name)
				}
			}
			if seen["kanz_bus_configured"] != 1 || seen["kanz_bus_connected"] != 1 || seen["kanz_bus_connection_events_total"] != 3 {
				t.Fatalf("missing finite startup series: %v", seen)
			}
		})
	}
}

func TestConfigurationDoesNotResurrectUnknownBacklog(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewBusMetrics(reg)
	m.SetPending("order.order.filled", "risk-engine", "group", 17)
	m.SetConsumerLag("order.order.filled", "risk-engine", "0", "group", 17)
	m.forgetBacklog(BacklogPending, "order.order.filled", "risk-engine", "group")
	m.forgetBacklog(BacklogLag, "order.order.filled", "risk-engine", "group")
	m.ConfigureClient("risk-engine", true)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "kanz_bus_pending_messages" || family.GetName() == "kanz_bus_consumer_lag" {
			t.Fatal("unknown backlog was republished as a healthy zero")
		}
	}
}

func TestDeclaringConfigurationDoesNotEraseObservedConnectionEvents(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewBusMetrics(reg)
	m.ConfigureClient("risk-engine", true)
	m.setConnected("risk-engine", true, "reconnected")
	m.ConfigureClient("risk-engine", true)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		for _, metric := range family.Metric {
			if family.GetName() == "kanz_bus_connected" && metric.GetGauge().GetValue() != 1 {
				t.Fatal("declaration reset live connectivity")
			}
			if family.GetName() == "kanz_bus_connection_events_total" {
				for _, label := range metric.Label {
					if label.GetName() == "event" && label.GetValue() == "reconnected" && metric.GetCounter().GetValue() != 1 {
						t.Fatal("declaration reset observed counter")
					}
				}
			}
		}
	}
}
