package bus

import (
	"github.com/nats-io/nats.go/jetstream"
	"testing"
	"time"
)

func TestRegisteredDefaultDurableRejectsStaleContract(t *testing.T) {
	const subject, group = "lake.snapshot.>", "archiver"
	tuning := tuningForSubject(subject)
	valid := func() jetstream.ConsumerConfig {
		return jetstream.ConsumerConfig{Durable: durableName(group, subject), FilterSubject: subject, AckPolicy: jetstream.AckExplicitPolicy, DeliverPolicy: jetstream.DeliverAllPolicy, AckWait: tuning.AckWait, MaxDeliver: tuning.MaxDeliver, MaxAckPending: tuning.MaxAckPending}
	}
	if !registeredDefaultDurableMatches(valid(), subject, group) {
		t.Fatal("native default refused")
	}
	for name, change := range map[string]func(*jetstream.ConsumerConfig){
		"volatile state":       func(c *jetstream.ConsumerConfig) { c.MemoryStorage = true },
		"original-time replay": func(c *jetstream.ConsumerConfig) { c.ReplayPolicy = jetstream.ReplayOriginalPolicy },
		"wrong durable":        func(c *jetstream.ConsumerConfig) { c.Durable = "wrong" },
		"wrong filter":         func(c *jetstream.ConsumerConfig) { c.FilterSubject = "other.>" },
		"multiple filters":     func(c *jetstream.ConsumerConfig) { c.FilterSubjects = []string{subject, "other.>"} },
		"no ack":               func(c *jetstream.ConsumerConfig) { c.AckPolicy = jetstream.AckNonePolicy },
		"latest only":          func(c *jetstream.ConsumerConfig) { c.DeliverPolicy = jetstream.DeliverNewPolicy },
		"retained override":    func(c *jetstream.ConsumerConfig) { c.MaxDeliver = -1 },
		"ack wait":             func(c *jetstream.ConsumerConfig) { c.AckWait = time.Second },
		"pending":              func(c *jetstream.ConsumerConfig) { c.MaxAckPending++ },
		"expiry":               func(c *jetstream.ConsumerConfig) { c.InactiveThreshold = time.Second },
		"push":                 func(c *jetstream.ConsumerConfig) { c.DeliverSubject = "other" },
		"headers only":         func(c *jetstream.ConsumerConfig) { c.HeadersOnly = true },
		"backoff":              func(c *jetstream.ConsumerConfig) { c.BackOff = []time.Duration{time.Second} },
		"paused":               func(c *jetstream.ConsumerConfig) { at := time.Now().Add(time.Hour); c.PauseUntil = &at },
	} {
		t.Run(name, func(t *testing.T) {
			c := valid()
			change(&c)
			if registeredDefaultDurableMatches(c, subject, group) {
				t.Fatal("stale durable accepted")
			}
		})
	}
}
