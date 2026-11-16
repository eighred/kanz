package config

import (
	"errors"
	"reflect"
	"testing"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/lineage/internal/graph"
	"github.com/eighred/kanz/services/lineage/internal/openlineage"
)

func fixtureEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"LINEAGE_NATS_URL", "LINEAGE_OPENLINEAGE_URL", "LINEAGE_EVENT_INDEX_MAX",
		"LINEAGE_POLICY_FILE", "LINEAGE_GOVERNANCE_FILE", "LINEAGE_SUBJECTS", "SPIFFE_ENDPOINT_SOCKET",
		"LINEAGE_TENANT", "LINEAGE_SOURCE", "LINEAGE_LISTEN", "LINEAGE_OTLP_ENDPOINT", "LINEAGE_LOG_LEVEL",
		"LINEAGE_CONSUMER_GROUP"} {
		t.Setenv(name, "")
	}
	t.Setenv("LINEAGE_DATASET_MAX", "16")
	t.Setenv("LINEAGE_EDGE_MAX", "32")
	t.Setenv("LINEAGE_STRING_BYTES_MAX", "65536")
}
func TestGlobalTopologyBudgetsRequireExplicitPositiveChoices(t *testing.T) {
	for _, name := range []string{"LINEAGE_DATASET_MAX", "LINEAGE_EDGE_MAX", "LINEAGE_STRING_BYTES_MAX"} {
		for _, value := range []string{"", "0", "-1", "no", "1.0", "18446744073709551616"} {
			t.Run(name+"/"+value, func(t *testing.T) {
				fixtureEnv(t)
				t.Setenv(name, value)
				if _, err := Load(); err == nil {
					t.Fatal("missing/invalid global limit looked healthy")
				}
			})
		}
	}
}
func TestEventIndexRetainsItsDefaultAndRefusesUnbounded(t *testing.T) {
	fixtureEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GraphLimits.EventIndexMax != graph.DefaultEventIndexCapacity || cfg.Tenant != bus.SystemTenant {
		t.Fatal("event cap or decision fallback changed")
	}
	if cfg.GraphLimits.DatasetMax != 16 || cfg.GraphLimits.EdgeMax != 32 || cfg.GraphLimits.StringBytesMax != 65536 {
		t.Fatal("explicit global limits not preserved")
	}
	if !reflect.DeepEqual(cfg.Subjects, []string{">"}) {
		t.Fatal("legacy subject default changed")
	}
	for _, value := range []string{"0", "-1", "wrong"} {
		t.Setenv("LINEAGE_EVENT_INDEX_MAX", value)
		if _, err := Load(); err == nil {
			t.Fatal("invalid event cap accepted")
		}
	}
}
func TestLegacyUnscopedSinkRefusesStartup(t *testing.T) {
	for _, url := range []string{"https://catalog.invalid", "/", " "} {
		t.Run(url, func(t *testing.T) {
			fixtureEnv(t)
			t.Setenv("LINEAGE_OPENLINEAGE_URL", url)
			if _, err := Load(); !errors.Is(err, openlineage.ErrUnscopedSink) {
				t.Fatalf("unscoped URL not refused: %v", err)
			}
		})
	}
}
func TestExplicitStreamScopesArePreserved(t *testing.T) {
	fixtureEnv(t)
	t.Setenv("LINEAGE_SUBJECTS", "order.order.filled,tfact.acme.order.order.filled")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Subjects, []string{"order.order.filled", "tfact.acme.order.order.filled"}) {
		t.Fatal("explicit stream scopes rewritten")
	}
}
