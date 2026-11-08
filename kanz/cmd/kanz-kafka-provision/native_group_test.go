package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eighred/kanz/cmd/kanz-kafka-provision/internal/provision"
	"github.com/segmentio/kafka-go"
)

// Deliberately implements only reads; this does not prove real broker behavior.
type nativeGroupFake struct {
	findErrors, fetchErrors []error
	findCalls, fetchCalls   int
	foreign                 bool
}

func (f *nativeGroupFake) FindCoordinator(context.Context, *kafka.FindCoordinatorRequest) (*kafka.FindCoordinatorResponse, error) {
	f.findCalls++
	var err error
	if f.findCalls <= len(f.findErrors) {
		err = f.findErrors[f.findCalls-1]
	}
	host := "native-redpanda-0.kanz-messaging.svc"
	if f.foreign {
		host = "foreign"
	}
	return &kafka.FindCoordinatorResponse{Error: err, Coordinator: &kafka.FindCoordinatorResponseCoordinator{NodeID: 0, Host: host, Port: 9094}}, nil
}
func (f *nativeGroupFake) OffsetFetch(_ context.Context, req *kafka.OffsetFetchRequest) (*kafka.OffsetFetchResponse, error) {
	f.fetchCalls++
	var err error
	if f.fetchCalls <= len(f.fetchErrors) {
		err = f.fetchErrors[f.fetchCalls-1]
	}
	result := &kafka.OffsetFetchResponse{Error: err, Topics: map[string][]kafka.OffsetFetchPartition{}}
	for topic := range req.Topics {
		result.Topics[topic] = []kafka.OffsetFetchPartition{{Partition: 0, CommittedOffset: -1}}
	}
	return result, nil
}

func TestNativeGroupReadinessDistinguishesCoordinatorAndAuthorization(t *testing.T) {
	f := &nativeGroupFake{findErrors: []error{kafka.GroupCoordinatorNotAvailable}, fetchErrors: []error{kafka.NotCoordinatorForGroup}}
	if err := nativeGroupReady(context.Background(), f, "acme.fixture", "acme.order.order"); err != nil || f.findCalls != 3 || f.fetchCalls != 2 {
		t.Fatal("readiness failed", err)
	}
	for _, tc := range []struct {
		fake nativeGroupFake
		want error
	}{
		{nativeGroupFake{findErrors: []error{kafka.GroupAuthorizationFailed}}, kafka.GroupAuthorizationFailed},
		{nativeGroupFake{foreign: true}, provision.ErrConflict},
	} {
		if err := nativeGroupReady(context.Background(), &tc.fake, "acme.fixture", "acme.order.order"); !errors.Is(err, tc.want) || tc.fake.findCalls != 1 || tc.fake.fetchCalls != 0 {
			t.Fatal("refusal retried", err)
		}
	}
}

func TestNativeGroupReadinessHonorsCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	f := &nativeGroupFake{findErrors: []error{kafka.GroupCoordinatorNotAvailable}}
	if err := nativeGroupReady(ctx, f, "acme.fixture", "acme.order.order"); !errors.Is(err, context.DeadlineExceeded) || f.findCalls != 1 || f.fetchCalls != 0 {
		t.Fatal("deadline not preserved", err)
	}
}
