package bus

import (
	"context"
	"github.com/nats-io/nats.go/jetstream"
)

// RequireRegisteredDefaultDurable proves an existing same-account durable has
// the native default delivery contract. It does not create it, substitute this
// client's tuning override, or establish that another process is currently live.
func (c *NATSClient) RequireRegisteredDefaultDurable(ctx context.Context, stream, subject, group string) error {
	if stream == "" || subject == "" || group == "" {
		return ErrDurableRetention
	}
	consumer, err := c.js.Consumer(ctx, stream, durableName(group, subject))
	if err != nil {
		return ErrDurableRetention
	}
	info, err := consumer.Info(ctx)
	if err != nil || info.Name != durableName(group, subject) || !registeredDefaultDurableMatches(info.Config, subject, group) {
		return ErrDurableRetention
	}
	return nil
}

func registeredDefaultDurableMatches(have jetstream.ConsumerConfig, subject, group string) bool {
	tuning := tuningForSubject(subject)
	return have.Durable == durableName(group, subject) && filterOf(have) == subject &&
		have.AckPolicy == jetstream.AckExplicitPolicy && have.DeliverPolicy == jetstream.DeliverAllPolicy &&
		have.ReplayPolicy == jetstream.ReplayInstantPolicy && !have.MemoryStorage && have.PriorityPolicy == 0 &&
		have.AckWait == tuning.AckWait && have.MaxDeliver == tuning.MaxDeliver && have.MaxAckPending == tuning.MaxAckPending &&
		len(have.BackOff) == 0 && have.DeliverSubject == "" && have.DeliverGroup == "" && !have.HeadersOnly &&
		have.InactiveThreshold == 0 && !have.FlowControl && have.IdleHeartbeat == 0 && have.RateLimit == 0 && have.OptStartSeq == 0 && have.OptStartTime == nil && have.PauseUntil == nil
}
