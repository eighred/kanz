package main

import (
	"context"
	"errors"
	"strings"
	"sync"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/tv-sync/internal/projection"
	"github.com/eighred/kanz/services/tv-sync/internal/server"
)

type factConsumerRouter struct{ facts, prices readyConsumer }

func (r factConsumerRouter) SubscribeReady(ctx context.Context, subject, group string, h bus.EventHandler, ready func()) error {
	for _, fact := range projection.Subjects() {
		if subject == fact {
			return r.facts.SubscribeReady(ctx, subject, group, h, ready)
		}
	}
	return r.prices.SubscribeReady(ctx, subject, group, h, ready)
}
func guardedFold(readiness *server.Readiness, tenant string, h bus.EventHandler) bus.EventHandler {
	var attempts sync.Mutex
	return func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
		if tenant == "" {
			return errors.New("tv-sync: fold tenant is required")
		}
		// Preserve projection.Handle's existing shared-spine policy: foreign
		// tenant facts are ignored, never folded or added to this readiness map.
		if env.GetTenantId() != tenant {
			return nil
		}
		if err := bus.RequireTenantScope(env.GetTenantId(), tenant); err != nil {
			return err
		}
		attempts.Lock()
		defer attempts.Unlock()
		failed := true
		// Also revoke serving if dispatch panics; the bus owns panic recovery.
		defer func() { readiness.FoldResult(env.GetTenantId(), env.GetEventId(), failed) }()
		err := h(ctx, env, payload)
		failed = err != nil
		return err
	}
}

// A disjoint literal root is sufficient to prove non-overlap without a second
// NATS wildcard parser. Broad or order-root price filters fail at startup.
func validateFactPriceSeparation(prices []string) error {
	for _, price := range prices {
		root, _, _ := strings.Cut(price, ".")
		if root == "" || root == "order" || strings.ContainsAny(root, "*>") {
			return errors.New("tv-sync: price filter requires a disjoint literal root")
		}
	}
	return nil
}

func newFactConsumer(client *bus.NATSClient) (*bus.Consumer, error) {
	return bus.NewConsumer(client, bus.WithDLQ(client), bus.WithRetainedRetries(projection.Subjects()...))
}
