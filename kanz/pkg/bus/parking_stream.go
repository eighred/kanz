package bus

import (
	"context"
	"github.com/nats-io/nats.go/jetstream"
	"slices"
	"time"
)

// RequireCanonicalParking checks the existing canonical DLQ route. It creates
// nothing. A valid profile does not guarantee every parking attempt fits:
// original headers and failure metadata still count toward broker limits. Such
// publication failures must leave the original delivery unacknowledged.
func (c *NATSClient) RequireCanonicalParking(ctx context.Context, source string, minimumMessageBytes int64) error {
	if source == "" || IsDLQSubject(source) || minimumMessageBytes <= 0 || c.conn.MaxPayload() < minimumMessageBytes {
		return ErrDurableRetention
	}
	name, err := c.streamForSubject(ctx, dlqSubject(source))
	if err != nil || name != "DLQ" {
		return ErrDurableRetention
	}
	stream, err := c.js.Stream(ctx, name)
	if err != nil {
		return ErrDurableRetention
	}
	info, err := stream.Info(ctx)
	if err != nil || !canonicalParkingMatches(info.Config, minimumMessageBytes) {
		return ErrDurableRetention
	}
	if (info.Config.MaxBytes > 0 && info.State.Bytes >= uint64(info.Config.MaxBytes)) || (info.Config.MaxMsgs > 0 && info.State.Msgs >= uint64(info.Config.MaxMsgs)) {
		return ErrDurableRetention
	}
	return nil
}

func canonicalParkingMatches(have jetstream.StreamConfig, minimumMessageBytes int64) bool {
	return have.Name == "DLQ" && slices.Equal(have.Subjects, []string{"dlq.>"}) &&
		have.Retention == jetstream.LimitsPolicy && have.Storage == jetstream.FileStorage && have.MaxAge == 720*time.Hour &&
		(have.MaxMsgSize == -1 || int64(have.MaxMsgSize) >= minimumMessageBytes) && !have.NoAck && !have.Sealed &&
		have.Mirror == nil && len(have.Sources) == 0 && have.SubjectTransform == nil && have.RePublish == nil &&
		!have.AllowRollup && !have.AllowMsgTTL && have.SubjectDeleteMarkerTTL == 0 && !have.AllowMsgCounter &&
		!have.AllowMsgSchedules && !have.AllowAtomicPublish && !have.AllowBatchPublish && have.Template == ""
}
