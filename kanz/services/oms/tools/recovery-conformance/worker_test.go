package recoveryconformance

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	t      *testing.T
	cmd    *exec.Cmd
	input  io.WriteCloser
	lines  chan []byte
	done   chan error
	cancel context.CancelFunc
	closed bool
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
	w := &worker{t: t, cmd: cmd, input: in, lines: make(chan []byte, 1), done: make(chan error, 1), cancel: cancel}
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
func (w *worker) call(request map[string]any) response {
	w.t.Helper()
	raw, err := json.Marshal(request)
	if err != nil {
		w.t.Fatal(err)
	}
	if len(raw) > 8<<20 {
		w.t.Fatal("recovery fixture input exceeds bound")
	}
	written := make(chan error, 1)
	go func() { _, err := w.input.Write(append(raw, '\n')); written <- err }()
	select {
	case err := <-written:
		if err != nil {
			w.t.Fatal("Rust fixture request failed")
		}
	case <-time.After(35 * time.Second):
		w.cancel()
		_ = w.input.Close()
		w.t.Fatal("Rust fixture input timed out")
	}
	select {
	case line, ok := <-w.lines:
		if !ok {
			w.t.Fatal("Rust fixture response missing")
		}
		var result response
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF {
			w.t.Fatal("invalid Rust fixture response")
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
		return result
	case <-time.After(35 * time.Second):
		w.t.Fatal("Rust fixture response timed out")
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
