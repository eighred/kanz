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

type nativeQuorumLeaderFake struct {
	calls int
}

func (f *nativeQuorumLeaderFake) Metadata(_ context.Context, req *kafka.MetadataRequest) (*kafka.MetadataResponse, error) {
	f.calls++
	brokers := fixtureBrokers(0, 1)
	partition := kafka.Partition{ID: 0}
	if f.calls > 1 {
		partition.Leader = brokers[0]
	}
	return &kafka.MetadataResponse{
		ClusterID: "cluster-a", Brokers: brokers,
		Topics: []kafka.Topic{{Name: req.Topics[0], Partitions: []kafka.Partition{{ID: 1}, partition}}},
	}, nil
}

func TestNativeQuorumLeaderRequiresPresentBrokerAndPartitionZero(t *testing.T) {
	f := &nativeQuorumLeaderFake{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := nativeWaitForQuorumLeader(ctx, f, "quorum-majority", "acme.order.order", "cluster-a"); err != nil || f.calls != 2 {
		t.Fatalf("expected actual broker zero on second observation, got %d calls: %v", f.calls, err)
	}
}

func TestNativeQuorumObservationRetriesReadTransitionsOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	results := []error{kafka.NotLeaderForPartition, provision.ErrMissing, nil}
	calls := 0
	err := nativeWaitForObservation(ctx, func() error {
		result := results[calls]
		calls++
		return result
	})
	if err != nil || calls != len(results) {
		t.Fatalf("expected marker after a leader transition, got %d calls: %v", calls, err)
	}
	calls = 0
	err = nativeWaitForObservation(ctx, func() error {
		calls++
		return kafka.TopicAuthorizationFailed
	})
	if !errors.Is(err, kafka.TopicAuthorizationFailed) || calls != 1 {
		t.Fatalf("authorization failure retried, got %d calls: %v", calls, err)
	}
}
