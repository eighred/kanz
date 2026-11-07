package recoveryconformance

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

type response struct {
	BuildProfile   string `json:"build_profile"`
	Version        string `json:"version"`
	Status         string `json:"status"`
	Error          string `json:"error"`
	SessionsClosed bool   `json:"sessions_closed"`
}

type worker struct {
	t              *testing.T
	cmd            *exec.Cmd
	input          io.WriteCloser
	lines          chan []byte
	done           chan error
	cancel         context.CancelFunc
	closed         bool
	ctx            context.Context
	successfulACKs int
}

func binary(t *testing.T) string {
	t.Helper()
	path := os.Getenv("KANZ_OMS_RECOVERY_BIN")
	if path == "" {
		t.Skip("KANZ_OMS_RECOVERY_BIN unavailable: real stamped Rust recovery process required")
	}
	if !filepath.IsAbs(path) || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(os.Getenv("KANZ_OMS_RECOVERY_VERSION")) {
		t.Fatal("absolute stamped Rust recovery fixture required")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatal("Rust recovery fixture unavailable")
	}
	return path
}
func startWorkerWithin(t *testing.T, config map[string]any, lifetime time.Duration, postgresURL string) (*worker, response) {
	if lifetime <= 0 || lifetime > 15*time.Minute {
		t.Fatal("worker lifetime outside qualification bound")
	}
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), lifetime)
	cmd := exec.CommandContext(ctx, binary(t))
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "SYSTEMROOT=" + os.Getenv("SYSTEMROOT"), "TEST_POSTGRES_URL=" + postgresURL, "TEST_NATS_URL=" + os.Getenv("TEST_NATS_URL")}
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 5 * time.Second
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	w := &worker{t: t, cmd: cmd, input: in, lines: make(chan []byte, 1), done: make(chan error, 1), cancel: cancel, ctx: ctx}
	if err = cmd.Start(); err != nil {
		cancel()
		t.Fatal("Rust process start failed")
	}
	go func() {
		scan := bufio.NewScanner(out)
		scan.Buffer(make([]byte, 4096), 16384)
	scanLoop:
		for scan.Scan() {
			select {
			case w.lines <- bytes.Clone(scan.Bytes()):
			case <-ctx.Done():
				break scanLoop
			}
		}
		close(w.lines)
		w.done <- cmd.Wait()
	}()
	t.Cleanup(func() {
		if !w.closed {
			cancel()
			_ = in.Close()
			select {
			case <-w.done:
			case <-time.After(6 * time.Second):
				t.Error("Rust process did not terminate")
			}
		}
		cancel()
	})
	return w, w.call(config)
}

// Diagnostics contain only closed protocol categories and completed outcomes.
// Peeking at exit must preserve the result for the existing cleanup/join owner.
func (w *worker) diagnostic(request map[string]any, phase string) string {
	method := "Init"
	if value, ok := request["method"].(string); ok {
		switch value {
		case "Observation", "Ack", "Parked", "Close":
			method = value
		default:
			method = "unknown"
		}
	}
	state := "active"
	if errors.Is(w.ctx.Err(), context.DeadlineExceeded) {
		state = "lifetime-expired"
	} else if w.ctx.Err() != nil {
		state = "cancelled"
	}
	exit := "pending"
	select {
	case err := <-w.done:
		exit = "success"
		if err != nil {
			exit = "failure"
		}
		w.done <- err
	default:
	}
	return fmt.Sprintf("method=%s phase=%s successful_acks=%d context=%s exit=%s", method, phase, w.successfulACKs, state, exit)
}
func TestWorkerDiagnosticPreservesJoinAndClosedCategories(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &worker{ctx: ctx, done: make(chan error, 1), successfulACKs: 7}
	exitErr := errors.New("private exit detail")
	w.done <- exitErr
	got := w.diagnostic(map[string]any{"method": "private method detail"}, "read")
	if got != "method=unknown phase=read successful_acks=7 context=active exit=failure" {
		t.Fatal("diagnostic category or completed count differs")
	}
	if <-w.done != exitErr {
		t.Fatal("diagnostic consumed or replaced join result")
	}
	cancel()
	if !strings.Contains(w.diagnostic(map[string]any{"method": "Ack"}, "read"), "context=cancelled exit=pending") {
		t.Fatal("cancelled diagnostic differs")
	}
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	w.ctx = expired
	if !strings.Contains(w.diagnostic(nil, "read"), "method=Init phase=read successful_acks=7 context=lifetime-expired") {
		t.Fatal("lifetime diagnostic differs")
	}
}
func (w *worker) call(request map[string]any) response {
	w.t.Helper()
	raw, err := json.Marshal(request)
	if err != nil {
		w.t.Fatal(err)
	}
	if len(raw) > 8<<20 {
		w.t.Fatal("recovery fixture input exceeds bound")
	}
	// One observation allowance covers input delivery, owner work and bounded cleanup.
	observation := time.NewTimer(45 * time.Second)
	defer observation.Stop()
	written := make(chan error, 1)
	go func() { _, err := w.input.Write(append(raw, '\n')); written <- err }()
	select {
	case err := <-written:
		if err != nil {
			w.t.Fatalf("Rust fixture request failed: %s", w.diagnostic(request, "write"))
		}
	case <-observation.C:
		diagnostic := w.diagnostic(request, "write-timeout")
		w.cancel()
		_ = w.input.Close()
		w.t.Fatalf("Rust fixture input timed out: %s", diagnostic)
	}
	select {
	case line, ok := <-w.lines:
		if !ok {
			w.t.Fatalf("Rust fixture response missing: %s", w.diagnostic(request, "read"))
		}
		var result response
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF {
			w.t.Fatalf("invalid Rust fixture response: %s", w.diagnostic(request, "decode"))
		}
		if result.BuildProfile != "release" {
			w.t.Fatal("Rust recovery fixture must use measured release build profile")
		}
		if result.Version != os.Getenv("KANZ_OMS_RECOVERY_VERSION") {
			w.t.Fatal("Rust fixture source stamp differs")
		}
		switch result.Status {
		case "connected":
			if request["method"] != nil || result.Error != "" {
				w.t.Fatal("unexpected connection outcome")
			}
		case "ok", "closed":
			if result.Error != "" {
				w.t.Fatal("success carries failure")
			}
		case "error":
			switch result.Error {
			case "DatabaseMemory", "ApprovalRefused", "RecoveryEvidenceConflict", "InvalidInput", "Capacity", "Conflict", "EvidenceConflict", "HistoryIncomplete", "PostedHistoryUnknown", "FeeRevision", "Database", "CommitUnconfirmed", "PublicationUnconfirmed", "Venue", "UnsupportedAction", "Deadline":
			default:
				w.t.Fatal("unknown recovery failure classification")
			}
		default:
			w.t.Fatal("unknown recovery outcome")
		}
		if request["method"] == "Ack" && result.Status == "ok" && result.SessionsClosed {
			w.successfulACKs++
		}
		return result
	case <-observation.C:
		w.t.Fatalf("Rust fixture response timed out: %s", w.diagnostic(request, "read-timeout"))
	}
	return response{}
}
func (w *worker) close(tr *transport) {
	w.t.Helper()
	r := w.call(map[string]any{"method": "Close", "timeout_ms": 2000})
	if r.Status != "closed" || !r.SessionsClosed {
		w.t.Fatal("owner cleanup not confirmed")
	}
	deadline := time.Now().Add(3 * time.Second)
	for (tr.source.Active() != 0 || tr.venue.active.Load() != 0) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if tr.source.Active() != 0 || tr.venue.active.Load() != 0 {
		w.t.Fatal("workload or venue connection survived Close before process exit")
	}
	if err := w.input.Close(); err != nil {
		w.t.Fatal(err)
	}
	select {
	case err := <-w.done:
		if err != nil {
			w.t.Fatal("Rust process failed after Close")
		}
	case <-time.After(5 * time.Second):
		w.t.Fatal("Rust process did not exit")
	}
	w.closed = true
	w.cancel()
}
