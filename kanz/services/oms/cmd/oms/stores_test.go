package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/eighred/kanz/services/oms/internal/config"
)

// BOTH BOOKS MUST ANNOUNCE INTO ONE QUEUE, OR THE POSITION FACT HAS NO DRAINER (#795).
//
// # What this is protecting
//
// The position projector no longer publishes. It hands the store records to
// commit with the fold, and a RELAY publishes them — which is what makes the
// announcement inherit the fold's ordering instead of racing it.
//
// The relay this composition root runs is built from the ORDER store's queue
// (order.NewService → outbox.NewRelay(store.Outbox(), …), started in
// runConsumers). In the durable deployment that costs nothing to get right:
// openStores hands both stores the SAME pool, so both write the same `outbox`
// TABLE and PendingKeys — which filters on nothing but published_at — finds every
// record whoever wrote it.
//
// The in-memory deployment has no such shared table. Two constructors would make
// two maps, and the position book's records would sit in a queue NOTHING drains:
// the inline Flush would still publish them, so every test and every dev session
// would look correct, and the background guarantee the outbox exists to provide
// would simply be absent. That is the exact shape of failure this composition
// root has shipped twice — wiring that no unit test reaches, and a nil-or-
// disconnected seam that presents as "it works".
//
// # Why the assertion is identity
//
// "They are both non-nil" would pass on two separate queues, which is the bug.
// The property is that they are the SAME queue, and interface comparison says so
// exactly.
func TestBothBooksShareOneOutboxInTheInMemoryDeployment(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	orders, positions, closeStores, err := openStores(context.Background(),
		config.Config{BaseCurrency: "USD"}, logger)
	if err != nil {
		t.Fatalf("openStores: %v", err)
	}
	t.Cleanup(closeStores)

	if orders.Outbox() == nil || positions.Outbox() == nil {
		t.Fatal("a store has no outbox queue — nothing would carry its FACTs at all")
	}
	if orders.Outbox() != positions.Outbox() {
		t.Fatal("the order store and the position book announce into DIFFERENT queues.\n\n" +
			"The relay this root runs is built from the ORDER store's queue, so the position " +
			"book's records would have no background drainer: they would publish only while the " +
			"inline flush succeeded, and a record it could not send would sit in a map nothing " +
			"ever reads again. Pass one outbox.Memory to both (order.WithSharedOutbox / " +
			"position.WithSharedOutbox).")
	}
}

// NON-VACUITY, AND IT IS NOT A FORMALITY HERE. The assertion above is an
// inequality between two interface values; it would hold just as well if
// openStores had started returning the same store twice, or nil twice. This
// pins that the two ARE the two stores the root needs.
func TestOpenStoresReturnsBothBooks(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	orders, positions, closeStores, err := openStores(context.Background(),
		config.Config{BaseCurrency: "USD"}, logger)
	if err != nil {
		t.Fatalf("openStores: %v", err)
	}
	t.Cleanup(closeStores)

	if orders == nil || positions == nil {
		t.Fatalf("openStores returned orders=%v positions=%v — a nil store is an OMS that cannot "+
			"admit an order or fold a fill", orders, positions)
	}
	// The order store must answer for orders and the position book for positions:
	// two handles onto one object would satisfy the identity check above.
	if _, _, err := orders.Load(context.Background(), "no-such-order"); err == nil {
		t.Error("the order store answered for an order that does not exist")
	}
	if _, err := positions.Snapshot(context.Background(), "no-such-portfolio", time.Now().UTC()); err != nil {
		t.Errorf("the position book cannot answer a snapshot: %v", err)
	}
}
