package bus

import (
	"context"
	"errors"
	"slices"

	"github.com/nats-io/nats.go/jetstream"
)

// ErrDurableRetention reports an unmet opt-in requirement, not a change to global
// broker defaults. Capacity refuses new publication rather than evicting old
// unprocessed work; age, per-subject eviction and publish-side rollup are absent.
var ErrDurableRetention = errors.New("bus: required durable retention unavailable")

// RequireDurableRetention inspects the existing connection/account. It never
// creates/updates a stream or relaxes settings. Unknown/error profiles refuse.
func (c *NATSClient) RequireDurableRetention(ctx context.Context, want jetstream.StreamConfig) error {
	if want.Name == "" || len(want.Subjects) == 0 || len(want.Subjects) > 16 || want.MaxBytes <= 0 || want.MaxMsgs <= 0 || want.MaxMsgSize <= 0 || !retentionMatches(want, want) {
		return ErrDurableRetention
	}
	stream, err := c.js.Stream(ctx, want.Name)
	if err != nil {
		return ErrDurableRetention
	}
	info, err := stream.Info(ctx)
	if err != nil || !retentionMatches(info.Config, want) || info.State.Bytes >= uint64(want.MaxBytes) || info.State.Msgs >= uint64(want.MaxMsgs) {
		return ErrDurableRetention
	}
	for _, subject := range want.Subjects {
		name, err := c.streamForSubject(ctx, subject)
		if err != nil || name != want.Name {
			return ErrDurableRetention
		}
	}
	return nil
}

func retentionMatches(have, want jetstream.StreamConfig) bool {
	actual, expected := slices.Clone(have.Subjects), slices.Clone(want.Subjects)
	slices.Sort(actual)
	slices.Sort(expected)
	return have.Name == want.Name && slices.Equal(actual, expected) &&
		have.Retention == jetstream.LimitsPolicy && have.Storage == jetstream.FileStorage && have.Discard == jetstream.DiscardNew &&
		have.MaxBytes == want.MaxBytes && have.MaxMsgs == want.MaxMsgs && have.MaxMsgSize == want.MaxMsgSize &&
		have.MaxConsumers == want.MaxConsumers && have.Replicas == want.Replicas &&
		have.MaxAge == 0 && have.MaxMsgsPerSubject == -1 && !have.DiscardNewPerSubject && !have.NoAck && !have.Sealed &&
		!have.DenyDelete && !have.DenyPurge &&
		!have.AllowRollup && !have.AllowMsgTTL && have.SubjectDeleteMarkerTTL == 0 && !have.AllowMsgCounter &&
		!have.AllowMsgSchedules && !have.AllowAtomicPublish && !have.AllowBatchPublish && have.PersistMode == 0 &&
		have.Mirror == nil && len(have.Sources) == 0 && have.SubjectTransform == nil && have.RePublish == nil &&
		have.Template == "" && have.ConsumerLimits.InactiveThreshold == 0 && have.ConsumerLimits.MaxAckPending == 0
}
