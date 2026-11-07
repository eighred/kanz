package bus

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

func TestRetainedStreamPolicyRefusesEvictionAndUnknownProfiles(t *testing.T) {
	want := jetstream.StreamConfig{Name: "FIXTURE", Subjects: []string{"fixture.requested", "fixture.published"}, Retention: jetstream.LimitsPolicy, Storage: jetstream.FileStorage, Discard: jetstream.DiscardNew, MaxBytes: 64 << 20, MaxMsgs: 10000, MaxMsgSize: 65536, MaxMsgsPerSubject: -1, MaxConsumers: 1, Replicas: 1}
	base := want
	if !retentionMatches(base, want) {
		t.Fatal("required profile refused")
	}
	mutations := map[string]func(*jetstream.StreamConfig){
		"operator-delete-disabled": func(c *jetstream.StreamConfig) { c.DenyDelete = true },
		"operator-purge-disabled":  func(c *jetstream.StreamConfig) { c.DenyPurge = true },
		"age":                      func(c *jetstream.StreamConfig) { c.MaxAge = time.Minute },
		"discard-old":              func(c *jetstream.StreamConfig) { c.Discard = jetstream.DiscardOld },
		"unbounded-bytes":          func(c *jetstream.StreamConfig) { c.MaxBytes = -1 },
		"unbounded-count":          func(c *jetstream.StreamConfig) { c.MaxMsgs = -1 },
		"message-cap":              func(c *jetstream.StreamConfig) { c.MaxMsgSize = -1 },
		"per-subject-eviction":     func(c *jetstream.StreamConfig) { c.MaxMsgsPerSubject = 1 },
		"interest-retention":       func(c *jetstream.StreamConfig) { c.Retention = jetstream.InterestPolicy },
		"volatile":                 func(c *jetstream.StreamConfig) { c.Storage = jetstream.MemoryStorage },
		"extra-subject":            func(c *jetstream.StreamConfig) { c.Subjects = append(c.Subjects, "other.>") },
		"missing-subject":          func(c *jetstream.StreamConfig) { c.Subjects = c.Subjects[:1] },
		"rollup":                   func(c *jetstream.StreamConfig) { c.AllowRollup = true },
		"ttl":                      func(c *jetstream.StreamConfig) { c.AllowMsgTTL = true },
		"ack-disabled":             func(c *jetstream.StreamConfig) { c.NoAck = true },
		"transform": func(c *jetstream.StreamConfig) {
			c.SubjectTransform = &jetstream.SubjectTransformConfig{Source: "fixture.>", Destination: "other.>"}
		},
		"sealed":            func(c *jetstream.StreamConfig) { c.Sealed = true },
		"mirror":            func(c *jetstream.StreamConfig) { c.Mirror = &jetstream.StreamSource{Name: "OTHER"} },
		"inactive-consumer": func(c *jetstream.StreamConfig) { c.ConsumerLimits.InactiveThreshold = time.Second },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := base
			changed.Subjects = append([]string(nil), base.Subjects...)
			mutate(&changed)
			if retentionMatches(changed, want) {
				t.Fatal("weakened profile accepted")
			}
		})
	}
}
