package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/services/market-ingest/internal/config"
)

// THE VOLUME-PROFILE POSTURE IS A NUMBER, NOT A SILENCE (#897).
//
// A market-ingest with no session floor configured publishes no profile, and from
// the OMS's side that is indistinguishable from an instrument nobody measures —
// the refusal a desk reads says NO_VOLUME_PROFILE either way. The gauge is the
// only thing that separates them, which is why it is asserted here rather than
// left to be noticed.

func volProfileLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type nopClient struct{}

func (nopClient) Publish(context.Context, bus.Message) error { return nil }
func (nopClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (nopClient) Close() error { return nil }

func testProducer(t *testing.T) *bus.Producer {
	t.Helper()
	p, err := bus.NewProducer(nopClient{}, bus.ProducerConfig{
		Source: "market-ingest", ProducerVersion: "test", Tenant: "__system__",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	return p
}

// gaugeValue reads one gauge out of a registry by name. It returns -1 when the
// metric is absent, which is a distinct answer from 0 — the whole point of the
// posture is that a deployment which does not publish SAYS SO with a number, and
// a missing metric would leave a dashboard reading "no data" for both states.
func gaugeValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		if len(f.GetMetric()) == 0 {
			t.Fatalf("%s is registered with no series", name)
		}
		return f.GetMetric()[0].GetGauge().GetValue()
	}
	return -1
}

func obsFor(t *testing.T) *observability.Provider {
	t.Helper()
	return &observability.Provider{Registry: prometheus.NewRegistry()}
}

// AN UNCONFIGURED SESSION FLOOR TURNS THE FEATURE OFF, EXPORTS A ZERO, AND DOES
// NOT FAIL THE PROCESS.
//
// Not starting would be wrong: a volume profile can be rebuilt from a tape that
// repeats every day, so unlike the ingestion-coverage record (#591) nothing is
// permanently lost by running without one. What must not happen is silence.
func TestVolumeProfileCollector_UnconfiguredIsOffAndSaysSoAsANumber(t *testing.T) {
	obs := obsFor(t)
	c, err := volumeProfileCollector(config.Config{}, testProducer(t), obs, volProfileLogger())
	if err != nil {
		t.Fatalf("an unset session floor should turn the fold OFF, not fail the process: %v", err)
	}
	if c != nil {
		t.Fatal("a collector was built with no session floor — volprofile.Config refuses to " +
			"invent one, so this would either have panicked or been given a number nobody chose")
	}
	if got := gaugeValue(t, obs.Registry, "kanz_market_ingest_volume_profile_publishing"); got != 0 {
		t.Fatalf("the posture gauge reads %v, want 0 — without it a deployment that publishes no "+
			"profile and one whose instruments are merely unmeasured are the same silence, and the "+
			"refusal a desk reads names the market in both cases", got)
	}
}

// A CONFIGURED FLOOR BUILDS THE FOLD AND EXPORTS A ONE.
func TestVolumeProfileCollector_ConfiguredPublishes(t *testing.T) {
	obs := obsFor(t)
	c, err := volumeProfileCollector(
		config.Config{Tenant: "__system__", VolumeProfileMinSessions: 5},
		testProducer(t), obs, volProfileLogger())
	if err != nil {
		t.Fatalf("volumeProfileCollector: %v", err)
	}
	if c == nil {
		t.Fatal("no collector was built with a session floor configured")
	}
	if got := gaugeValue(t, obs.Registry, "kanz_market_ingest_volume_profile_publishing"); got != 1 {
		t.Fatalf("the posture gauge reads %v, want 1", got)
	}
	if got := c.Store().String(); !strings.Contains(got, "minSessions:5") {
		t.Errorf("the fold reports %q — the configured floor did not reach volprofile.Config, so "+
			"this deployment would assert a shape from whatever history it happened to hold", got)
	}
}

// A NAMED-BUT-UNHONOURABLE CONFIGURATION IS A HARD REFUSAL TO START.
//
// The distinction from the OFF case above is the whole of it: nobody setting a
// value and this process silently ignoring one somebody DID set are different
// failures, and only the second is a lie. A bucket width that does not divide the
// session is volprofile.New's own refusal, surfaced here rather than swallowed.
func TestVolumeProfileCollector_ANamedButUnhonourableSettingRefusesToStart(t *testing.T) {
	obs := obsFor(t)
	_, err := volumeProfileCollector(
		config.Config{Tenant: "__system__", VolumeProfileMinSessions: 5, VolumeProfileBucket: 7 * 60 * 1e9},
		testProducer(t), obs, volProfileLogger())
	if err == nil {
		t.Fatal("a bucket width that does not divide the session was accepted — the last bin of " +
			"every session would be short and its share understated by exactly the amount nobody " +
			"would notice")
	}
	if got := gaugeValue(t, obs.Registry, "kanz_market_ingest_volume_profile_publishing"); got != 0 {
		t.Errorf("the posture gauge reads %v after a refused configuration, want 0", got)
	}
}
