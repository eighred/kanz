// Package natsrebuild reconstructs the NATS live spine from the Kafka log of
// record (DR-01c). NATS is short-retention and rebuildable by design (EVT-08):
// after a region failover the DR NATS cluster is empty, so this drains a bounded
// recent window of the DR-replicated Kafka log (DR-01a) back onto the LIVE
// subjects, repopulating the spine for the services that fan out off it.
//
// It deliberately reuses the EVT-20 read-only replay.Reader as its source (no
// consumer-group mutation, bounded range) but is NOT the replay path: replay
// republishes onto the isolated `replay.{run}.*` namespace and stamps
// QUALITY_FLAG_REPLAYED so live sinks reject it. Reconstruction is the inverse —
// it republishes onto the ORIGINAL subject with the envelope UNCHANGED, so live
// consumers accept the events as normal. Handlers are idempotent (the platform
// premise, DEBT-02), so re-consuming a recent window after failover is safe.
package natsrebuild

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/tools/replay"
)

// Stats summarizes a Run.
type Stats struct {
	// Published counts events republished onto their live subject.
	Published uint64
	// Malformed counts source events that failed to unframe — logged + skipped,
	// not fatal (a single poison frame must not abort a DR rebuild).
	Malformed uint64
}

// Pipeline drains a replay.Source onto a bus.Publisher under the LIVE subject
// namespace. The destination subject is the envelope's own EventType (the
// 3-segment logical subject, subject-taxonomy §6), the envelope rides through
// untouched, and the partition key + transport headers propagate — notably
// Nats-Msg-Id, so the rebuilt stream's dedup window collapses any duplicate the
// bounded source range produces.
type Pipeline struct {
	Source    replay.Source
	Publisher bus.Publisher
	Logger    *slog.Logger
}

// Run drives the pipeline until the source is drained (io.EOF → success), the
// context is cancelled, or a fatal error occurs. A per-message unframe failure
// is counted and skipped; any other source error or a publish error is fatal
// and returns the partial Stats.
func (p *Pipeline) Run(ctx context.Context) (Stats, error) {
	var stats Stats
	if p.Source == nil {
		return stats, errors.New("natsrebuild: Source is nil")
	}
	if p.Publisher == nil {
		return stats, errors.New("natsrebuild: Publisher is nil")
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
			var mfe *replay.MalformedFrameError
			if errors.As(err, &mfe) {
				atomic.AddUint64(&stats.Malformed, 1)
				log.Warn("natsrebuild: skipping malformed frame",
					"topic", mfe.Topic, "partition", mfe.Partition,
					"offset", mfe.Offset, "err", mfe.Err)
				continue
			}
			return stats, fmt.Errorf("natsrebuild: source: %w", err)
		}
		if ev.Envelope == nil {
			return stats, errors.New("natsrebuild: source returned nil envelope on success path")
		}
		subj := ev.Envelope.GetEventType()
		if subj == "" {
			return stats, fmt.Errorf("natsrebuild: empty event_type at %s/%d@%d (cannot route to a live subject)",
				ev.Topic, ev.Partition, ev.Offset)
		}
		// Re-marshal the frame from the UNCHANGED envelope — no REPLAYED flag,
		// no namespace rewrite: this is a live re-publication, not a replay.
		body, mErr := proto.Marshal(&envelopepb.EventFrame{Envelope: ev.Envelope, Payload: ev.Payload})
		if mErr != nil {
			return stats, fmt.Errorf("natsrebuild: re-marshal frame at %s/%d@%d: %w",
				ev.Topic, ev.Partition, ev.Offset, mErr)
		}
		if pErr := p.Publisher.Publish(ctx, bus.Message{
			Subject: subj,
			Key:     ev.Key,
			Body:    body,
			Headers: ev.Headers,
		}); pErr != nil {
			return stats, fmt.Errorf("natsrebuild: publish %s/%d@%d → %s: %w",
				ev.Topic, ev.Partition, ev.Offset, subj, pErr)
		}
		atomic.AddUint64(&stats.Published, 1)
	}
}
