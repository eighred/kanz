package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/eighred/kanz/internal/env"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/lineage/internal/graph"
	"github.com/eighred/kanz/services/lineage/internal/openlineage"
)

type Config struct {
	Listen        string
	LogLevel      slog.Level
	NATSURL       string
	Source        string
	ConsumerGroup string
	// The legacy default remains ">". On a provisioned multi-stream topology
	// the existing bus refuses it (#698): choose explicit actual stream subject
	// scopes before rollout. This does not declare comprehensive ingestion.
	Subjects       []string
	GraphLimits    graph.Limits
	PolicyFile     string
	GovernanceFile string
	// Every nonempty legacy URL refuses startup until a real scoped sink
	// contract is reviewed. Empty selects the owned log emitter.
	OpenLineageURL string
	OTLPEndpoint   string
	// Tenant is ONLY the authorization decision recorder's fallback. It cannot
	// assign graph ownership; each admitted fact uses its envelope tenant.
	Tenant       string
	SPIFFESocket string
}

var DefaultSubjects = []string{">"}

func requiredPositiveInt(name string) (int, error) {
	n, err := strconv.Atoi(os.Getenv(name))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("config: %s requires an explicit positive integer", name)
	}
	return n, nil
}
func Load() (Config, error) {
	subjects := env.SplitList(os.Getenv("LINEAGE_SUBJECTS"))
	if len(subjects) == 0 {
		subjects = append([]string(nil), DefaultSubjects...)
	}
	limits := graph.Limits{EventIndexMax: graph.DefaultEventIndexCapacity}
	if v := os.Getenv("LINEAGE_EVENT_INDEX_MAX"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("config: LINEAGE_EVENT_INDEX_MAX must be positive")
		}
		limits.EventIndexMax = n
	}
	var err error
	if limits.DatasetMax, err = requiredPositiveInt("LINEAGE_DATASET_MAX"); err != nil {
		return Config{}, err
	}
	if limits.EdgeMax, err = requiredPositiveInt("LINEAGE_EDGE_MAX"); err != nil {
		return Config{}, err
	}
	limits.StringBytesMax, err = strconv.ParseUint(os.Getenv("LINEAGE_STRING_BYTES_MAX"), 10, 64)
	if err != nil || limits.StringBytesMax == 0 {
		return Config{}, fmt.Errorf("config: LINEAGE_STRING_BYTES_MAX requires an explicit positive integer")
	}
	if err := limits.Validate(); err != nil {
		return Config{}, err
	}
	if os.Getenv("LINEAGE_OPENLINEAGE_URL") != "" {
		return Config{}, openlineage.ErrUnscopedSink
	}
	return Config{
		Listen:   env.Or("LINEAGE_LISTEN", ":8086"),
		LogLevel: env.ParseLevelOr(env.Or("LINEAGE_LOG_LEVEL", "info"), slog.LevelInfo),
		NATSURL:  os.Getenv("LINEAGE_NATS_URL"), Source: env.Or("LINEAGE_SOURCE", "lineage"),
		ConsumerGroup: env.Or("LINEAGE_CONSUMER_GROUP", "lineage"), Subjects: subjects, GraphLimits: limits,
		PolicyFile: os.Getenv("LINEAGE_POLICY_FILE"), GovernanceFile: os.Getenv("LINEAGE_GOVERNANCE_FILE"),
		OTLPEndpoint: os.Getenv("LINEAGE_OTLP_ENDPOINT"), Tenant: env.Or("LINEAGE_TENANT", bus.SystemTenant),
		SPIFFESocket: os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
	}, nil
}
