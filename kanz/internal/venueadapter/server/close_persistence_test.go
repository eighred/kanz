package server

import (
	"context"
	"errors"
	"testing"

	"github.com/eighred/kanz/internal/execution"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type unavailableCloseStore struct{ *execution.CloseRegistry }

func (*unavailableCloseStore) Track(context.Context, execution.CloseIntent) error {
	return errors.New("database unavailable")
}

func TestCancelCannotDispatchWithoutPersistedOwnership(t *testing.T) {
	v := &fakeVenue{}
	s, registry, _ := newServer(t, v)
	s.closes = &unavailableCloseStore{registry}
	_, err := s.CancelOrder(context.Background(), &venuepb.CancelOrderRequest{State: order()})
	if status.Code(err) != codes.Unavailable || v.cancelCalls != 0 || registry.Len() != 0 {
		t.Fatalf("unowned cancel dispatched: err=%v calls=%d pending=%d", err, v.cancelCalls, registry.Len())
	}
}
