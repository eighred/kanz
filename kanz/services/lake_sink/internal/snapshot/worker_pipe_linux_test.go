//go:build linux && amd64

package snapshot

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestWorkerPipeJoinPrecedesWait(t *testing.T) {
	for _, oldOrder := range []bool{true, false} {
		name := "joined-before-wait"
		if oldOrder {
			name = "old-wait-closes-unread-stderr"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "printf done")
			cmd.Env = []string{}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal("stdout unavailable")
			}
			stderr, err := cmd.StderrPipe()
			if err != nil {
				t.Fatal("stderr unavailable")
			}
			if cmd.Start() != nil {
				t.Fatal("disposable child failed")
			}
			defer func() {
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			}()
			body, err := io.ReadAll(stdout)
			if err != nil || string(body) != "done" {
				t.Fatal("positive stdout control failed")
			}
			// The child has closed stdout. Deliberately hold the stderr reader
			// until the ordering check; no scheduler timing is the oracle.
			writeDone, stderrDone := make(chan error, 1), make(chan error, 1)
			writeDone <- nil
			if oldOrder {
				if cmd.Wait() != nil {
					t.Fatal("positive child exit failed")
				}
				_, err = io.Copy(io.Discard, stderr)
				if !errors.Is(err, os.ErrClosed) {
					t.Fatal("old ordering did not expose closed unread pipe")
				}
				return
			}
			result := make(chan workerPipeJoins, 1)
			go func() { result <- joinWorkerPipes(ctx, writeDone, stderrDone) }()
			select {
			case <-result:
				t.Fatal("join completed before stderr reader")
			default:
			}
			_, err = io.Copy(io.Discard, stderr)
			stderrDone <- err
			select {
			case joined := <-result:
				if !joined.write || !joined.stderr || joined.writeErr != nil || joined.stderrErr != nil {
					t.Fatal("pipe join failed")
				}
			case <-ctx.Done():
				t.Fatal("pipe join did not complete")
			}
			if cmd.Wait() != nil {
				t.Fatal("joined child exit failed")
			}
		})
	}
}

func TestWorkerPipeJoinCancellationRetainsUnjoinedTasks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	joined := joinWorkerPipes(ctx, make(chan error), make(chan error))
	if joined.write || joined.stderr {
		t.Fatal("cancellation fabricated pipe joins")
	}
}
