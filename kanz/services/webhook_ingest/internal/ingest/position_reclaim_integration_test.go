package ingest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest/positionfixture"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func reclaimLiveOMSFixture(t *testing.T, ctx context.Context, url, portfolio string, venues ...string) (*positionfixture.PositionReceipts, jetstream.JetStream, uint64) {
	t.Helper()
	admin, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(ctx, "EXECUTION")
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return positionfixture.ReclaimOMSPositions(t, js, portfolio, "BTC-USD", venues...), js, info.State.LastSeq
}

func joinLiveOMSObserver(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("live OMS input observer did not stop/join before POSITION reclaim")
		}
	})
}
