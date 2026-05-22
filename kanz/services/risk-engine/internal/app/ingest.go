// Package app holds the risk-engine service's runtime wiring — the
// composition root that connects the bus to the risk module's engine.
// The risk *logic* lives in kanz/internal/risk/* (RISK-04..11, ORCH-01b);
// this package owns process concerns: subscription fan-out, lifecycle,
// and (later) bootstrap + recompute scheduling.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/kanz-eng/kanz/internal/risk/ingest"
	"github.com/kanz-eng/kanz/pkg/bus"
)

// DefaultConsumerGroup is the durable consumer name the risk-engine
// subscribes under. One group across all state subjects: each (subject,
// group) is its own durable consumer, all owned by this service, so
// scaling to N replicas spreads partitions within the group.
const DefaultConsumerGroup = "risk-engine"

// stateSubjects are the three risk state subjects the engine ingests
// (subject == event_type per subject-taxonomy §1). The Ingestor's
// dispatch keys on env.EventType, so a subject and its event_type match.
var stateSubjects = []string{
	ingest.EventTypePortfolioRevalued,
	ingest.EventTypePositionChanged,
	ingest.EventTypePortfolioSnapshot,
}

// Ingest subscribes the risk state subjects and feeds every delivery
// through the RISK-04 Ingestor into the RISK-05 Applier (state.Store).
//
// # Per-aggregate ordering
//
// The three subjects are subscribed concurrently — a portfolio's
// PortfolioState arrives on one subject and its PositionStates on
// another, on independent goroutines. Cross-subject ordering is NOT
// preserved by the bus (it only guarantees per-partition order). The
// per-aggregate ordering invariant (RISK-05) is upheld downstream: the
// state.Store holds a per-portfolio mutex that serializes every apply
// for one portfolio regardless of which subscription delivered it. This
// wiring therefore needs no cross-subject coordination of its own.
type Ingest struct {
	consumer *bus.Consumer
	handler  bus.EventHandler
	group    string
	logger   *slog.Logger
}

// NewIngest builds the ingestion wiring. consumer is the envelope-aware
// bus.Consumer (caller chooses transport + retry/DLQ policy); applier is
// the engine's state.Store. group defaults to DefaultConsumerGroup when
// empty.
func NewIngest(consumer *bus.Consumer, applier ingest.Applier, group string, logger *slog.Logger) (*Ingest, error) {
	if consumer == nil {
		return nil, errors.New("app: consumer is nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	ingestor, err := ingest.NewIngestor(applier)
	if err != nil {
		return nil, err
	}
	if group == "" {
		group = DefaultConsumerGroup
	}
	return &Ingest{
		consumer: consumer,
		handler:  ingestor.Handler,
		group:    group,
		logger:   logger,
	}, nil
}

// Run subscribes every state subject and blocks until ctx is canceled
// (graceful shutdown, ORCH-01e) or a subscription fails. Each subject
// runs on its own goroutine because bus.Consumer.Subscribe blocks. The
// first non-cancellation error cancels the siblings and is returned —
// fail-fast, so a single broken subscription brings the ingester down
// rather than silently running degraded.
func (i *Ingest) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	for _, subject := range stateSubjects {
		wg.Add(1)
		go func(subject string) {
			defer wg.Done()
			i.logger.Info("risk-engine subscribing", "subject", subject, "group", i.group)
			err := i.consumer.Subscribe(ctx, subject, i.group, i.handler)
			if err != nil && !errors.Is(err, context.Canceled) {
				once.Do(func() {
					firstErr = fmt.Errorf("subscribe %s: %w", subject, err)
					cancel()
				})
			}
		}(subject)
	}
	wg.Wait()
	return firstErr
}
