package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/eighred/kanz/cmd/kanz-kafka-provision/internal/provision"
	"github.com/segmentio/kafka-go"
)

func fixtureBrokers(ids ...int) []kafka.Broker {
	var result []kafka.Broker
	for _, id := range ids {
		result = append(result, kafka.Broker{ID: id, Host: fmt.Sprintf("native-redpanda-%d.kanz-messaging.svc", id), Port: 9094})
	}
	return result
}

func TestNativeQuorumViewsRequireActualFaultAndRestoration(t *testing.T) {
	for _, tc := range []struct {
		mode  string
		ids   []int
		ready bool
	}{
		{"quorum-healthy", []int{0, 1, 2}, true}, {"quorum-healthy", []int{0, 1}, false},
		{"quorum-majority", []int{0, 1}, true}, {"quorum-majority", []int{0, 1, 2}, false}, {"quorum-majority", []int{1, 2}, false},
		{"quorum-minority", []int{1, 2}, true}, {"quorum-minority", []int{0, 1, 2}, false},
		{"quorum-restored", []int{0, 1, 2}, true}, {"quorum-restored", []int{0, 1}, false},
	} {
		ready, err := nativeBrokerView(fixtureBrokers(tc.ids...), tc.mode)
		if err != nil || ready != tc.ready {
			t.Fatal("unexpected broker view", tc.mode, err)
		}
	}
	for _, brokers := range [][]kafka.Broker{fixtureBrokers(0, 0), fixtureBrokers(3), {{ID: 0, Host: "foreign", Port: 9094}}} {
		if _, err := nativeBrokerView(brokers, "quorum-majority"); !errors.Is(err, provision.ErrConflict) {
			t.Fatal("unowned broker accepted")
		}
	}
}

type nativeMetadataFake struct {
	calls   int
	cluster string
}

func (f *nativeMetadataFake) Metadata(context.Context, *kafka.MetadataRequest) (*kafka.MetadataResponse, error) {
	f.calls++
	return &kafka.MetadataResponse{ClusterID: f.cluster, Brokers: fixtureBrokers(0, 1, 2)}, nil
}

func TestNativeQuorumObservationRefusesChangedEstateAndHonorsDeadline(t *testing.T) {
	f := &nativeMetadataFake{cluster: "wrong"}
	if _, err := nativeQuorumMetadata(context.Background(), f, "quorum-majority", "cluster-a"); !errors.Is(err, provision.ErrConflict) || f.calls != 1 {
		t.Fatal("changed estate retried")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	f = &nativeMetadataFake{cluster: "cluster-a"}
	if _, err := nativeQuorumMetadata(ctx, f, "quorum-majority", "cluster-a"); !errors.Is(err, context.DeadlineExceeded) || f.calls != 1 {
		t.Fatal("unchanged healthy view conferred fault proof")
	}
}
