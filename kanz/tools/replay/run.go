package replay

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	"github.com/kanz-eng/kanz/pkg/bus"
)

// ReplayPrefix is the reserved top-level subject prefix from
// kanz-schemas/docs/subject-taxonomy.md §6. Every replayed event lands under
// `{ReplayPrefix}{run_id}.{original_event_type}` so live sinks subscribing
// to live subjects never see replayed traffic — that is the namespace half
// of EVT-20b's isolation. EVT-20c adds defense-in-depth via the
// QUALITY_FLAG_REPLAYED envelope stamp + live-validator hard-reject.
const ReplayPrefix = "replay."

// dlqPrefix mirrors §6's reserved DLQ namespace. Replay must never wrap a
// DLQ event: those are terminal poison, not source material for a re-run.
const dlqPrefix = "dlq."

// RunID identifies one replay run. UUIDv7-derived, hex-encoded (no dashes)
// so the value is a single subject segment with no escaping subtleties on
// either NATS or Kafka. Time-sortable: lexicographic order on the hex matches
// chronological order of run creation — useful for operator audit trails.
type RunID string

// NewRunID generates a fresh run identifier. UUIDv7 (RFC 9562) encodes the
// Unix-ms timestamp in the high 48 bits, so hex-encoded UUIDv7 strings sort
// in creation order.
func NewRunID() RunID {
	u := uuid.Must(uuid.NewV7())
	return RunID(hex.EncodeToString(u[:]))
}

// SubjectFor returns the replay-namespaced subject for an original three-segment
// event_type (e.g. `market.equity.trade`): `replay.{runID}.{original}`. The
// rule is enforced both ways — an original that already lives under a
// reserved prefix is rejected so we never produce `replay.X.replay.Y.…` or
// `replay.X.dlq.Y.…` chains.
func SubjectFor(runID RunID, original string) (string, error) {
	if runID == "" {
		return "", errors.New("replay: runID is empty")
	}
	if original == "" {
		return "", errors.New("replay: original subject is empty")
	}
	if strings.HasPrefix(original, ReplayPrefix) {
		return "", fmt.Errorf("replay: cannot wrap a replay-namespaced subject %q", original)
	}
	if strings.HasPrefix(original, dlqPrefix) {
		return "", fmt.Errorf("replay: cannot wrap a DLQ subject %q", original)
	}
	return ReplayPrefix + string(runID) + "." + original, nil
}

// ConsumerGroup returns the per-run consumer-group identifier. Replay-scoped
// consumers MUST use this so their offsets / durable consumer state can
// never collide with live consumer groups on the same broker — that is the
// consumer-group half of EVT-20b's isolation.
func ConsumerGroup(runID RunID) (string, error) {
	if runID == "" {
		return "", errors.New("replay: runID is empty")
	}
	return "replay-" + string(runID), nil
}

// Source is the upstream of a Pipeline. *Reader (EVT-20a) is the canonical
// implementation; in-memory fakes satisfy this for tests. Next mirrors the
// Reader contract: (Event, nil) on success, (_, io.EOF) on exhaustion,
// (Event{kafka-coords}, *MalformedFrameError) on per-message unframe failure
// (recoverable — Pipeline logs + skips), any other non-nil error fatal.
type Source interface {
	Next(ctx context.Context) (Event, error)
}

// Stats summarizes a Pipeline.Run.
type Stats struct {
	// Published counts events successfully republished under the replay
	// namespace.
	Published uint64
	// Malformed counts source events that failed to unframe (the
	// MalformedFrameError path); they are logged + skipped, not fatal.
	Malformed uint64
}

// Pipeline drains a Source into a bus.Publisher under the replay namespace.
// Each event's destination subject is `replay.{RunID}.{envelope.EventType}`
// (via SubjectFor); the original Kafka partition key, transport headers, and
// envelope body ride through unchanged. The envelope is re-marshaled into a
// fresh EventFrame so the Pipeline owns the wire bytes — EVT-20c stamps
// QUALITY_FLAG_REPLAYED on ev.Envelope before re-marshal without touching
// this layer.
//
// Headers (notably Nats-Msg-Id) propagate as-is: the destination stream's
// dedup window catches duplicate deliveries from the source range, while
// being per-stream-scoped means no cross-stream interference with the
// original live publication.
type Pipeline struct {
	RunID     RunID
	Source    Source
	Publisher bus.Publisher
	// Logger receives one-line warnings for malformed/skipped frames. nil
	// falls back to slog.Default().
	Logger *slog.Logger
}

// Run drives the pipeline until the Source returns io.EOF (success), the
// context is cancelled, or a fatal error occurs (Source error other than
// MalformedFrameError, or any Publisher error). On fatal error Run returns
// the partial Stats with the wrapped error.
func (p *Pipeline) Run(ctx context.Context) (Stats, error) {
	var stats Stats
	if p.RunID == "" {
		return stats, errors.New("replay: Pipeline.RunID is empty")
	}
	if p.Source == nil {
		return stats, errors.New("replay: Pipeline.Source is nil")
	}
	if p.Publisher == nil {
		return stats, errors.New("replay: Pipeline.Publisher is nil")
	}
	log := p.Logger
	if log == nil {
		log = slog.Default()
	}

	for {
		ev, err := p.Source.Next(ctx)
		if errors.Is(err, io.EOF) {
			return stats, nil
		}
		if err != nil {
			var mfe *MalformedFrameError
			if errors.As(err, &mfe) {
				atomic.AddUint64(&stats.Malformed, 1)
				log.Warn("replay: skipping malformed frame",
					"topic", mfe.Topic, "partition", mfe.Partition,
					"offset", mfe.Offset, "err", mfe.Err)
				continue
			}
			return stats, fmt.Errorf("replay: source: %w", err)
		}
		if ev.Envelope == nil {
			return stats, errors.New("replay: source returned nil envelope on success path")
		}
		subj, sErr := SubjectFor(p.RunID, ev.Envelope.EventType)
		if sErr != nil {
			return stats, sErr
		}
		// EVT-20c: stamp QUALITY_FLAG_REPLAYED before re-marshal so the wire
		// bytes carry the flag. Live consumers (bus.Validate) hard-reject it;
		// replay consumers (bus.ValidateReplay) require it.
		StampReplayed(ev.Envelope)
		body, mErr := proto.Marshal(&envelopepb.EventFrame{
			Envelope: ev.Envelope,
			Payload:  ev.Payload,
		})
		if mErr != nil {
			return stats, fmt.Errorf("replay: re-marshal frame at %s/%d@%d: %w",
				ev.Topic, ev.Partition, ev.Offset, mErr)
		}
		if pErr := p.Publisher.Publish(ctx, bus.Message{
			Subject: subj,
			Key:     ev.Key,
			Body:    body,
			Headers: ev.Headers,
		}); pErr != nil {
			return stats, fmt.Errorf("replay: publish %s/%d@%d → %s: %w",
				ev.Topic, ev.Partition, ev.Offset, subj, pErr)
		}
		atomic.AddUint64(&stats.Published, 1)
	}
}
