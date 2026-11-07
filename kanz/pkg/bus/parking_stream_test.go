package bus

import (
	"github.com/nats-io/nats.go/jetstream"
	"testing"
	"time"
)

func TestCanonicalParkingProfileRefusesUnknownOrChangedCustody(t *testing.T) {
	valid := func() jetstream.StreamConfig {
		return jetstream.StreamConfig{Name: "DLQ", Subjects: []string{"dlq.>"}, Retention: jetstream.LimitsPolicy, Storage: jetstream.FileStorage, MaxAge: 720 * time.Hour, MaxMsgSize: -1}
	}
	if !canonicalParkingMatches(valid(), 65536) {
		t.Fatal("canonical profile refused")
	}
	for name, change := range map[string]func(*jetstream.StreamConfig){
		"wrong stream":          func(c *jetstream.StreamConfig) { c.Name = "other" },
		"extra subject":         func(c *jetstream.StreamConfig) { c.Subjects = append(c.Subjects, "other.>") },
		"age changed":           func(c *jetstream.StreamConfig) { c.MaxAge = time.Hour },
		"memory":                func(c *jetstream.StreamConfig) { c.Storage = jetstream.MemoryStorage },
		"workqueue":             func(c *jetstream.StreamConfig) { c.Retention = jetstream.WorkQueuePolicy },
		"small message":         func(c *jetstream.StreamConfig) { c.MaxMsgSize = 65535 },
		"unknown message limit": func(c *jetstream.StreamConfig) { c.MaxMsgSize = 0 },
		"no ack":                func(c *jetstream.StreamConfig) { c.NoAck = true },
		"sealed":                func(c *jetstream.StreamConfig) { c.Sealed = true },
		"mirror":                func(c *jetstream.StreamConfig) { c.Mirror = &jetstream.StreamSource{Name: "other"} },
		"source":                func(c *jetstream.StreamConfig) { c.Sources = []*jetstream.StreamSource{{Name: "other"}} },
		"transform": func(c *jetstream.StreamConfig) {
			c.SubjectTransform = &jetstream.SubjectTransformConfig{Source: "dlq.>", Destination: "other.>"}
		},
		"republish": func(c *jetstream.StreamConfig) {
			c.RePublish = &jetstream.RePublish{Source: "dlq.>", Destination: "other.>"}
		},
		"ttl":    func(c *jetstream.StreamConfig) { c.AllowMsgTTL = true },
		"rollup": func(c *jetstream.StreamConfig) { c.AllowRollup = true },
	} {
		t.Run(name, func(t *testing.T) {
			c := valid()
			change(&c)
			if canonicalParkingMatches(c, 65536) {
				t.Fatal("changed parking custody accepted")
			}
		})
	}
}
