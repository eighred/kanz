package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/eighred/kanz/internal/fillfact"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/transport"
)

const replayArmTimeout = 2 * time.Minute

type factWatch struct {
	cancel context.CancelFunc
	done   chan struct{}
	client *bus.NATSClient
	mesh   *transport.Mesh
}

// watchCapitalFacts drains retained history before returning. The gateway POST
// must happen only after this function succeeds; otherwise a fast venue response
// can be committed before the evidence subscriptions exist.
func watchCapitalFacts(ctx context.Context, cfg config, collector *factCollector) (*factWatch, error) {
	mesh, err := transport.NewMesh(ctx, cfg.spiffeSocket)
	if err != nil {
		return nil, fmt.Errorf("capitalpath: acquire workload identity: %w", err)
	}
	client, err := bus.DialNATS(ctx, bus.NATSConfig{
		URL: cfg.natsURL, Name: "capitalpath-certifier", TLSConfig: mesh.Client,
		MaxReconnects: 3,
	})
	if err != nil {
		_ = mesh.Close()
		return nil, fmt.Errorf("capitalpath: dial mTLS event spine: %w", err)
	}
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		_ = client.Close()
		_ = mesh.Close()
		return nil, fmt.Errorf("capitalpath: create FACT consumer: %w", err)
	}

	subCtx, cancel := context.WithCancel(ctx)
	watch := &factWatch{cancel: cancel, done: make(chan struct{}), client: client, mesh: mesh}
	subjects := [...]string{
		subjectAccepted, subjectRejected, subjectRouted,
		fillfact.SubjectPartiallyFilled, fillfact.SubjectFilled, subjectPortfolioCash,
	}
	errCh := make(chan error, len(subjects))
	var wg sync.WaitGroup
	for _, subject := range subjects {
		ready := make(chan struct{})
		var readyOnce sync.Once
		wg.Add(1)
		go func(subject string) {
			defer wg.Done()
			err := consumer.SubscribeReplay(subCtx, subject, collector.handle(subject), func() {
				readyOnce.Do(func() { close(ready) })
			})
			if err != nil && !errors.Is(err, context.Canceled) {
				subscribeErr := fmt.Errorf("capitalpath: subscribe %s: %w", subject, err)
				collector.fail(subscribeErr)
				select {
				case errCh <- subscribeErr:
				default:
				}
			}
		}(subject)
		timer := time.NewTimer(replayArmTimeout)
		select {
		case <-ready:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case err := <-errCh:
			timer.Stop()
			cancel()
			wg.Wait()
			_ = client.Close()
			_ = mesh.Close()
			return nil, err
		case <-timer.C:
			cancel()
			wg.Wait()
			_ = client.Close()
			_ = mesh.Close()
			return nil, fmt.Errorf("capitalpath: retained replay for %s did not arm within %s", subject, replayArmTimeout)
		}
	}
	go func() {
		wg.Wait()
		close(watch.done)
	}()
	collector.armLive()
	return watch, nil
}

func (w *factWatch) close() {
	if w == nil {
		return
	}
	w.cancel()
	<-w.done
	_ = w.client.Close()
	_ = w.mesh.Close()
}
