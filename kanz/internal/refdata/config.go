package refdata

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/eighred/kanz/internal/env"
)

// THE WIRING LIVES HERE, NOT IN THREE SERVICES' CONFIG PACKAGES.
//
// Three composition roots need this classifier — the OMS pre-trade gate, the
// compliance post-trade monitor and the risk engine's scenario shocks — and
// each would otherwise carry its own copy of "read a URL, parse an interval,
// decide what an empty value means". AGENTS.md names where that ends: 17
// services each had their own secret() and 15 were wrong. One reader, three
// callers, and the meaning of an unset variable is decided once.

// Config is a deployment's reference-data wiring.
type Config struct {
	// DatamasterURL is the security master's root. EMPTY IS A POSTURE, NOT AN
	// OVERSIGHT: it means this deployment has no reference-data source, and the
	// composition root then passes a NIL Classifier rather than an empty one, so
	// that "none wired" and "wired but does not know this instrument" stay two
	// different violations with two different operator actions.
	DatamasterURL string

	// RefreshInterval is how often the composition root runs a Refresh cycle. It
	// is the ceiling on how long a cold instrument stays refused, so it is short
	// relative to RefreshTTL rather than equal to it.
	RefreshInterval time.Duration

	// Options are the cache's bounds. Left zero, the package defaults apply.
	Options Options
}

// DefaultRefreshInterval is how often a composition root runs a cycle. It is
// well under DefaultRefreshTTL because it also governs LATENCY TO FIRST ANSWER:
// an instrument first seen at t is refused until the next cycle, and that
// window is a mandate this pod cannot evaluate.
const DefaultRefreshInterval = time.Minute

// LoadConfig reads the wiring from the environment under a service's prefix —
// LoadConfig("OMS") reads OMS_DATAMASTER_URL and OMS_REFDATA_REFRESH_INTERVAL.
//
// A MALFORMED INTERVAL IS AN ERROR, never a fallback to the default. An
// operator who typed "60" meaning a minute has said something specific and
// wrong; running on the default instead would start cleanly and behave in a way
// nothing on the pod explains. This is the same defect #692 files against
// datamaster's own schedule parsing, and it is not reintroduced here.
func LoadConfig(prefix string) (Config, error) {
	c := Config{
		DatamasterURL:   env.Or(prefix+"_DATAMASTER_URL", ""),
		RefreshInterval: DefaultRefreshInterval,
	}
	if raw, ok := env.Lookup(prefix + "_REFDATA_REFRESH_INTERVAL"); ok {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s_REFDATA_REFRESH_INTERVAL: %q is not a duration "+
				"(want e.g. \"60s\", \"2m\"): %w", prefix, raw, err)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("%s_REFDATA_REFRESH_INTERVAL: %q leaves the reference cache "+
				"never refreshed, so every instrument stays permanently unresolved and every "+
				"SECTOR, ISSUER and ASSET_CLASS mandate is refused", prefix, raw)
		}
		c.RefreshInterval = d
	}
	return c, nil
}

// Wired reports whether this deployment has a reference-data source at all.
func (c Config) Wired() bool { return c.DatamasterURL != "" }

// NewCache builds the cache for a service. tenant is the tenant this deployment
// serves and subject names the calling service in datamaster's log. It returns
// (nil, nil) when no master is configured — the caller then passes a nil
// Classifier, which is the honest statement of that posture.
func (c Config) NewCache(tenant, subject string) (*Cache, error) {
	if !c.Wired() {
		return nil, nil
	}
	src, err := NewHTTPSource(c.DatamasterURL, tenant, subject)
	if err != nil {
		return nil, err
	}
	return NewCache(src, c.Options)
}

// LogPosture states, at startup, which of the two postures this pod is in.
//
// IT IS NOT A COURTESY LINE. The armed case prints the staleness bound because
// that is the number that explains a refusal hours later; the unarmed case
// prints at WARN and says what it costs, because a deployment that simply never
// mentioned reference data is how a compliance control stays unarmed for a year.
// The metric half of this — a gauge an alert can read — belongs to the caller,
// which owns the registry.
func (c Config) LogPosture(logger *slog.Logger, service string) {
	if !c.Wired() {
		logger.Warn("NO INSTRUMENT CLASSIFIER: this deployment has no reference-data source, so "+
			"every mandate rule naming the SECTOR, ISSUER or ASSET_CLASS dimension will be "+
			"REFUSED as unverifiable, and every named stress scenario's sector shocks will "+
			"refuse rather than return an unshocked book",
			"service", service, "set", "<PREFIX>_DATAMASTER_URL")
		return
	}
	o, err := c.Options.withDefaults()
	if err != nil {
		// Unreachable in practice — NewCache validates the same thing and the
		// caller has already handled its error — but a posture line that silently
		// printed the wrong bounds would be worse than one that says it cannot.
		logger.Warn("instrument classifier bounds are invalid", "service", service, "err", err)
		return
	}
	logger.Info("instrument classifier armed",
		"service", service,
		"datamaster", c.DatamasterURL,
		"refresh_interval", c.RefreshInterval.String(),
		"refresh_ttl", o.RefreshTTL.String(),
		// The one an operator needs at 3am: past this, an instrument stops being
		// classified and its mandate starts refusing.
		"served_until_stale", o.MaxAge.String(),
		"max_entries", o.MaxEntries,
	)
}
