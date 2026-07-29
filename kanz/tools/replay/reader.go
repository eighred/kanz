package replay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
)

// Range bounds a per-partition read. Exactly one of StartOffset / StartTime
// may be set (zero values mean "earliest"); exactly one of EndOffset /
// EndTime must be set. EndOffset is inclusive; EndTime is exclusive,
// matching the [start, end) convention of common windowing.
//
// Replay is a snapshot of history, not a tail. The reader probes each
// partition's high-water mark once, at seek time, and stops at
// min(EndOffset, HWM-1) regardless of which end bound was supplied: the
// caller's end bound bounds the request, the high-water mark bounds
// reality, and the read stops at whichever comes first. A read that
// blocked waiting for a message beyond the end of the log at seek time
// would be tailing, not replaying — and a DR restore that never returns
// is useless.
type Range struct {
	StartOffset *int64
	StartTime   *time.Time
	EndOffset   *int64
	EndTime     *time.Time
}

func (r Range) validate() error {
	if r.StartOffset != nil && r.StartTime != nil {
		return errors.New("replay: Range.StartOffset and Range.StartTime are mutually exclusive")
	}
	if r.EndOffset != nil && r.EndTime != nil {
		return errors.New("replay: Range.EndOffset and Range.EndTime are mutually exclusive")
	}
	if r.EndOffset == nil && r.EndTime == nil {
		return errors.New("replay: Range requires an end bound (EndOffset or EndTime)")
	}
	if r.StartOffset != nil && *r.StartOffset < 0 {
		return errors.New("replay: Range.StartOffset must be >= 0")
	}
	if r.EndOffset != nil && *r.EndOffset < 0 {
		return errors.New("replay: Range.EndOffset must be >= 0")
	}
	if r.StartOffset != nil && r.EndOffset != nil && *r.EndOffset < *r.StartOffset {
		return errors.New("replay: Range.EndOffset must be >= StartOffset")
	}
	if r.StartTime != nil && r.EndTime != nil && !r.EndTime.After(*r.StartTime) {
		return errors.New("replay: Range.EndTime must be after StartTime")
	}
	return nil
}

// Config drives Reader.
type Config struct {
	// Brokers is the Kafka bootstrap list; matches bus.KafkaConfig.Brokers.
	Brokers []string
	// Topic is the source topic per kanz-schemas/docs/subject-taxonomy.md.
	Topic string
	// Range bounds the per-partition read.
	Range Range
	// Partitions optionally restricts the read to a subset; empty means
	// every partition discovered on the topic.
	Partitions []int
	// PerPartitionBuffer is the size of each goroutine's send channel. 64 is
	// a reasonable default; bursts above this back-pressure the fetcher.
	PerPartitionBuffer int
	// PartitionDialTimeout caps the partition-discovery dial. 10s default.
	PartitionDialTimeout time.Duration
}

// MalformedFrameError wraps a per-message unframe failure with the Kafka
// coordinates of the offending message. Downstream consumers (e.g. the
// EVT-20b Pipeline) use errors.As to discriminate this skippable per-message
// failure from a fatal source error: replay tooling logs and continues on
// MalformedFrameError; any other non-EOF error aborts the run.
type MalformedFrameError struct {
	Topic     string
	Partition int
	Offset    int64
	Err       error
}

func (e *MalformedFrameError) Error() string {
	return fmt.Sprintf("replay: unframe %s/%d@%d: %v", e.Topic, e.Partition, e.Offset, e.Err)
}

func (e *MalformedFrameError) Unwrap() error { return e.Err }

// Event is one replayed message — the unframed envelope, the payload bytes,
// and the Kafka coordinates needed to reproduce the read or feed a downstream
// publisher. Headers carries the on-the-wire transport headers (including
// Nats-Msg-Id and any Kanz-DLQ-* metadata if the source topic is a DLQ).
type Event struct {
	Envelope  *envelopepb.Envelope
	Payload   []byte
	Topic     string
	Partition int
	Offset    int64
	KafkaTime time.Time
	Key       []byte
	Headers   map[string]string
}

// Reader streams events from a finite log range. It is a one-shot iterator:
// call Next until io.EOF, then Close. Reader holds no consumer-group state
// — replays never disturb live consumer positions.
type Reader struct {
	cfg       Config
	startOnce sync.Once
	startErr  error
	out       chan result
	cancel    context.CancelFunc
	closeOnce sync.Once
	closed    chan struct{}
}

type result struct {
	ev  Event
	err error // non-nil for per-message failures (e.g. unframe); io.EOF unused — Reader signals EOF by closing out.
}

// NewReader validates cfg but does no I/O. The first call to Next discovers
// partitions and starts the fetcher goroutines.
func NewReader(cfg Config) (*Reader, error) {
	if len(cfg.Brokers) == 0 {
		return nil, errors.New("replay: at least one broker required")
	}
	if cfg.Topic == "" {
		return nil, errors.New("replay: topic required")
	}
	if err := cfg.Range.validate(); err != nil {
		return nil, err
	}
	for _, p := range cfg.Partitions {
		if p < 0 {
			return nil, fmt.Errorf("replay: partition %d invalid", p)
		}
	}
	if cfg.PerPartitionBuffer <= 0 {
		cfg.PerPartitionBuffer = 64
	}
	if cfg.PartitionDialTimeout <= 0 {
		cfg.PartitionDialTimeout = 10 * time.Second
	}
	return &Reader{cfg: cfg, closed: make(chan struct{})}, nil
}

// Next returns the next event. io.EOF means the range is fully drained on
// every partition. A non-nil, non-EOF error from per-message failure (e.g.
// malformed EventFrame) is surfaced with the Event populated only with Kafka
// coordinates so the caller can log + skip; bus-level errors abort the read.
func (r *Reader) Next(ctx context.Context) (Event, error) {
	r.startOnce.Do(func() { r.startErr = r.start(ctx) })
	if r.startErr != nil {
		return Event{}, r.startErr
	}
	select {
	case <-ctx.Done():
		return Event{}, ctx.Err()
	case res, ok := <-r.out:
		if !ok {
			return Event{}, io.EOF
		}
		return res.ev, res.err
	}
}

// Close cancels in-flight fetchers and waits for them to drain. Safe to call
// multiple times. Pending Next calls will see io.EOF or ctx.Err().
func (r *Reader) Close() error {
	r.closeOnce.Do(func() {
		if r.cancel != nil {
			r.cancel()
		}
		<-r.closed
	})
	return nil
}

func (r *Reader) start(parent context.Context) error {
	parts, err := r.resolvePartitions(parent)
	if err != nil {
		return err
	}
	if len(parts) == 0 {
		// Nothing to read — signal EOF immediately.
		r.out = make(chan result)
		close(r.out)
		close(r.closed)
		return nil
	}

	ctx, cancel := context.WithCancel(parent)
	r.cancel = cancel
	r.out = make(chan result, r.cfg.PerPartitionBuffer*len(parts))

	var wg sync.WaitGroup
	for _, p := range parts {
		wg.Add(1)
		go func(partition int) {
			defer wg.Done()
			r.readPartition(ctx, partition)
		}(p)
	}
	go func() {
		wg.Wait()
		close(r.out)
		close(r.closed)
	}()
	return nil
}

// resolvePartitions returns the partition IDs to read. If cfg.Partitions is
// set we trust it (deduped + sorted); otherwise we dial the broker and list.
func (r *Reader) resolvePartitions(ctx context.Context) ([]int, error) {
	if len(r.cfg.Partitions) > 0 {
		seen := make(map[int]struct{}, len(r.cfg.Partitions))
		out := make([]int, 0, len(r.cfg.Partitions))
		for _, p := range r.cfg.Partitions {
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
		sort.Ints(out)
		return out, nil
	}
	dialer := &kafka.Dialer{Timeout: r.cfg.PartitionDialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", r.cfg.Brokers[0])
	if err != nil {
		return nil, fmt.Errorf("replay: dial broker: %w", err)
	}
	defer conn.Close()
	parts, err := conn.ReadPartitions(r.cfg.Topic)
	if err != nil {
		return nil, fmt.Errorf("replay: list partitions for %q: %w", r.cfg.Topic, err)
	}
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		out = append(out, p.ID)
	}
	sort.Ints(out)
	return out, nil
}

// readPartition drains one partition's range into r.out. Per-partition order
// is preserved; cross-partition is arrival-order (Kafka gives no global
// ordering — event-class-rules §1).
func (r *Reader) readPartition(ctx context.Context, partition int) {
	kr := kafka.NewReader(kafka.ReaderConfig{
		Brokers:   r.cfg.Brokers,
		Topic:     r.cfg.Topic,
		Partition: partition,
		MinBytes:  1,
		MaxBytes:  10 << 20,
		// No GroupID — direct partition read, no consumer-group state mutated.
	})
	defer kr.Close()

	if err := r.seekStart(ctx, kr); err != nil {
		r.send(ctx, Event{Topic: r.cfg.Topic, Partition: partition}, fmt.Errorf("replay: seek partition %d: %w", partition, err))
		return
	}

	// Replay is a snapshot of history, not a tail: probe the high-water mark
	// once, at seek time, and never read past it. A message that does not
	// exist yet will never satisfy an EndOffset/EndTime filter, so without
	// this cap a range whose bound lies beyond the log's current end blocks
	// in FetchMessage until the caller's context dies — a hang, not a
	// termination.
	hwm, err := r.partitionHighWaterMark(ctx, partition)
	if err != nil {
		r.send(ctx, Event{Topic: r.cfg.Topic, Partition: partition}, fmt.Errorf("replay: probe high-water mark for partition %d: %w", partition, err))
		return
	}
	if hwm == 0 {
		return // partition has no messages at all; nothing to read.
	}
	endOffset := hwm - 1 // HWM is the offset of the next (unwritten) message.
	if r.cfg.Range.EndOffset != nil && *r.cfg.Range.EndOffset < endOffset {
		endOffset = *r.cfg.Range.EndOffset
	}
	if off := kr.Offset(); off >= 0 && off > endOffset {
		return // the requested start is beyond every message this snapshot will ever see.
	}

	// Kafka message timestamps are millisecond-resolution on the wire. A
	// caller's Range bound is an ordinary Go time.Time and routinely carries
	// sub-millisecond precision (e.g. from time.Now()). A message written
	// with a timestamp that exactly equals an exclusive EndTime round-trips
	// through Kafka truncated to whole milliseconds and comes back strictly
	// *before* the untruncated bound, so the exclusive check fails to
	// exclude it. Truncate both bounds to the same resolution Kafka actually
	// stores so equal-at-the-boundary compares equal, not before.
	var startTime, endTime *time.Time
	if r.cfg.Range.StartTime != nil {
		t := r.cfg.Range.StartTime.Truncate(time.Millisecond)
		startTime = &t
	}
	if r.cfg.Range.EndTime != nil {
		t := r.cfg.Range.EndTime.Truncate(time.Millisecond)
		endTime = &t
	}

	for {
		m, err := kr.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) {
				return
			}
			r.send(ctx, Event{Topic: r.cfg.Topic, Partition: partition}, fmt.Errorf("replay: fetch partition %d: %w", partition, err))
			return
		}
		if m.Offset > endOffset {
			return
		}
		atCap := m.Offset >= endOffset

		switch {
		case startTime != nil && m.Time.Before(*startTime):
			// A seek is a positioning hint, not a guarantee — don't trust it
			// blindly. Anything that lands before the requested start is
			// dropped without stopping the read.
		case endTime != nil && !m.Time.Before(*endTime):
			// Reached the exclusive end of the time window.
			return
		default:
			ev := Event{
				Topic:     m.Topic,
				Partition: m.Partition,
				Offset:    m.Offset,
				KafkaTime: m.Time,
				Key:       m.Key,
			}
			if len(m.Headers) > 0 {
				ev.Headers = make(map[string]string, len(m.Headers))
				for _, h := range m.Headers {
					ev.Headers[h.Key] = string(h.Value)
				}
			}
			env, payload, uerr := bus.Unframe(m.Value)
			if uerr != nil {
				r.send(ctx, ev, &MalformedFrameError{
					Topic: ev.Topic, Partition: ev.Partition, Offset: ev.Offset, Err: uerr,
				})
			} else {
				ev.Envelope = env
				ev.Payload = payload
				r.send(ctx, ev, nil)
			}
		}

		if atCap {
			return
		}
	}
}

// partitionHighWaterMark returns the offset of the next message that would
// be written to partition — i.e. one past the last message currently on the
// log. It dials the partition leader directly (the same ListOffsets seam
// SetOffsetAt already uses) rather than hand-rolling a protocol call.
func (r *Reader) partitionHighWaterMark(ctx context.Context, partition int) (int64, error) {
	dialer := &kafka.Dialer{Timeout: r.cfg.PartitionDialTimeout}
	var lastErr error
	for _, broker := range r.cfg.Brokers {
		conn, err := dialer.DialLeader(ctx, "tcp", broker, r.cfg.Topic, partition)
		if err != nil {
			lastErr = err
			continue
		}
		hwm, err := conn.ReadLastOffset()
		conn.Close()
		if err != nil {
			lastErr = err
			continue
		}
		return hwm, nil
	}
	return 0, fmt.Errorf("dialing all brokers, last error: %w", lastErr)
}

func (r *Reader) seekStart(ctx context.Context, kr *kafka.Reader) error {
	switch {
	case r.cfg.Range.StartOffset != nil:
		return kr.SetOffset(*r.cfg.Range.StartOffset)
	case r.cfg.Range.StartTime != nil:
		return kr.SetOffsetAt(ctx, *r.cfg.Range.StartTime)
	default:
		return kr.SetOffset(kafka.FirstOffset)
	}
}

func (r *Reader) send(ctx context.Context, ev Event, err error) {
	select {
	case <-ctx.Done():
	case r.out <- result{ev: ev, err: err}:
	}
}
