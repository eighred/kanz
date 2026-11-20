package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
)

// Both Go processes complete their startup sweeps before this barrier is armed.
// While the single winning CancelOrder is waiting here, the other consumer can
// observe only the original XSIM working fill with its full reservation.
type fundedWithdrawalRace struct {
	armed                                             atomic.Bool
	cancelled                                         atomic.Bool
	openViews, terminalViews                          atomic.Int32
	cancelArrived, openChecked, ackLost, bothTerminal chan struct{}
}

func newFundedWithdrawalRace() *fundedWithdrawalRace {
	return &fundedWithdrawalRace{cancelArrived: make(chan struct{}), openChecked: make(chan struct{}), ackLost: make(chan struct{}), bothTerminal: make(chan struct{})}
}

func awaitWithdrawalRPC(ctx context.Context, ready <-chan struct{}, what string) error {
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(8 * time.Second):
		return fmt.Errorf("timed out waiting for %s", what)
	}
}

func (r *fundedWithdrawalRace) beforeRPC(ctx context.Context, method string) error {
	switch method {
	case venuepb.VenueAdapterService_CancelOrder_FullMethodName:
		if !r.cancelled.CompareAndSwap(false, true) {
			return errors.New("second withdrawal reached XSIM")
		}
		close(r.cancelArrived)
		return awaitWithdrawalRPC(ctx, r.openChecked, "the competing working-order query")
	case venuepb.VenueAdapterService_QueryOrder_FullMethodName:
		return awaitWithdrawalRPC(ctx, r.cancelArrived, "the winning withdrawal RPC")
	}
	return nil
}

func (r *fundedWithdrawalRace) observeQuery(ctx context.Context, a *fundedWithdrawalApproval, view *venuepb.QueryOrderResponse) error {
	switch view.GetState() {
	case venuepb.OrderViewState_ORDER_VIEW_STATE_PARTIALLY_FILLED:
		if r.openViews.Add(1) != 1 {
			return errors.New("more than one racing query returned the working order")
		}
		prior, err := a.readPartial(ctx)
		if err != nil {
			return err
		}
		if _, err = a.confirmExecution(ctx, prior, view, venuepb.OrderViewState_ORDER_VIEW_STATE_PARTIALLY_FILLED); err != nil {
			return err
		}
		if err = a.checkClaim(ctx); err != nil {
			return err
		}
		close(r.openChecked)
		// Return this working response unchanged. Holding its delivery until the
		// first cancel commits prevents a fast NAK from racing the barrier itself.
		return awaitWithdrawalRPC(ctx, r.ackLost, "the winning lost acknowledgement")
	case venuepb.OrderViewState_ORDER_VIEW_STATE_CANCELLED:
		if err := awaitWithdrawalRPC(ctx, r.ackLost, "the winning lost acknowledgement"); err != nil {
			return err
		}
		switch r.terminalViews.Add(1) {
		case 1:
		case 2:
			close(r.bothTerminal)
		default:
			return errors.New("unexpected third terminal query")
		}
		// Neither process receives this evidence until both asked XSIM about
		// their unchanged partial snapshot. Both must run the settlement path.
		return awaitWithdrawalRPC(ctx, r.bothTerminal, "both terminal queries")
	default:
		return errors.New("XSIM query returned an unexpected withdrawal race state")
	}
}

type goWithdrawalChild struct {
	pid    int
	done   <-chan struct{}
	err    error
	output []byte
}

func awaitWithdrawalChild(t *testing.T, ctx context.Context, child *goWithdrawalChild, path string) {
	t.Helper()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		select {
		case <-child.done:
			t.Fatalf("Go process %d exited before %s: %v %s", child.pid, filepath.Base(path), child.err, child.output)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-timeout.C:
			t.Fatalf("Go process %d never reported %s", child.pid, filepath.Base(path))
		case <-tick.C:
		}
	}
}

func verifyRaceSettlementResults(t *testing.T, a, b []byte) {
	t.Helper()
	results := map[string]int{}
	for _, log := range [][]byte{a, b} {
		for _, line := range strings.Split(string(log), "\n") {
			if !strings.Contains(line, "oms synthetic withdrawal settlement") {
				continue
			}
			for _, code := range []string{"APPLIED", "DUPLICATE"} {
				if strings.Contains(line, "result="+code) {
					results[code]++
				}
			}
		}
	}
	if results["APPLIED"] != 1 || results["DUPLICATE"] != 1 {
		t.Fatalf("two Go settlement results: %v", results)
	}
}
