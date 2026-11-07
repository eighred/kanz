//go:build linux && amd64

package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"
	workerpb "github.com/eighred/kanz/kanz-schemas-go/lake/worker/v1"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func actualWorker(t *testing.T) (string, [32]byte) {
	t.Helper()
	path, digestText := os.Getenv("TEST_LAKE_WORKER_PATH"), os.Getenv("TEST_LAKE_WORKER_SHA256")
	if path == "" && digestText == "" {
		t.Skip("actual immutable Linux worker unavailable")
	}
	digestBytes, err := hex.DecodeString(digestText)
	if err != nil || len(digestBytes) != 32 || path == "" {
		t.Fatal("incomplete actual worker identity")
	}
	var digest [32]byte
	copy(digest[:], digestBytes)
	verified, err := openWorkerExecutable(path, digest)
	if err != nil {
		t.Fatal("actual worker ownership/hash verification refused")
	}
	if verified.Close() != nil {
		t.Fatal("verified executable close failed")
	}
	return path, digest
}

func actualConvertRequest(t *testing.T) *workerpb.WorkerRequest {
	t.Helper()
	row := custodyRow(t, "fixture-tenant")
	selection := &lakepb.SnapshotSelection{EventTypes: []string{row.EventType}, EventTimeFrom: timestamppb.New(row.EventTime), EventTimeUntil: timestamppb.New(row.EventTime.Add(time.Second))}
	request, err := ConvertLanding(context.Background(), row.TenantID, selection, [][]byte{landingBytes(t, row, row)})
	if err != nil {
		t.Fatal("canonical independent raw fixture refused")
	}
	return request
}

func TestSnapshotActualWorkerConvertAndQuery(t *testing.T) {
	path, digest := actualWorker(t)
	w, err := NewWorker(path, digest, func() { t.Error("worker cleanup failed") })
	if err != nil {
		t.Fatal("worker initialization refused")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	request := actualConvertRequest(t)
	response, err := w.Invoke(ctx, request, nil)
	if err != nil || response.GetConverted() == nil || response.GetConverted().RowCount != 2 {
		t.Fatal("actual raw-custody conversion failed")
	}
	c := response.GetConverted()
	q := &lakepb.SnapshotQuery{Operation: lakepb.SnapshotQueryOperation_SNAPSHOT_QUERY_OPERATION_LIST_EVENTS, PageSize: 1}
	query := &workerpb.WorkerRequest{ProtocolVersion: ProtocolVersion, Operation: &workerpb.WorkerRequest_Query{Query: &workerpb.QueryControl{ParquetBytes: uint64(len(c.Parquet)), ParquetSha256: c.Sha256, Query: q, ExpectedTenant: request.GetConvert().Tenant, ExpectedRowCount: 2, SchemaVersion: SchemaVersion}}}
	first, err := w.Invoke(ctx, query, c.Parquet)
	if err != nil || len(first.GetQuery().GetEvents()) != 1 || first.GetQuery().Exhausted || first.GetQuery().NextOrdinal == nil {
		t.Fatal("actual bounded first page failed")
	}
	e := first.GetQuery().Events[0]
	want := request.GetConvert().Rows[0]
	if e.Ordinal != want.Ordinal || e.EventId != want.EventId || e.EventType != want.EventType || !proto.Equal(e.EventTime, want.EventTime) {
		t.Fatal("exact event identity/time changed")
	}
	q.AfterOrdinal = first.GetQuery().NextOrdinal
	last, err := w.Invoke(ctx, query, c.Parquet)
	if err != nil || len(last.GetQuery().GetEvents()) != 1 || !last.GetQuery().Exhausted || last.GetQuery().Events[0].Ordinal != request.GetConvert().Rows[1].Ordinal {
		t.Fatal("duplicate order or finite page exhaustion changed")
	}
}

// The dedicated CI job installs a second root-owned immutable binary beneath a
// runner-owned disposable directory. No test escalates privileges or copies a
// mutable executable and calls that production ownership evidence.
func TestSnapshotActualVerifiedDescriptorSurvivesPathReplacementAndClosesInheritedFD(t *testing.T) {
	_, digest := actualWorker(t)
	path := os.Getenv("TEST_LAKE_WORKER_SWAP_PATH")
	if path == "" {
		t.Fatal("configured actual proof requires separately owned swap fixture")
	}
	executable, err := openWorkerExecutable(path, digest)
	if err != nil {
		t.Fatal("swap fixture verification refused")
	}
	defer executable.Close()
	// Only the explicitly supplied fixture's directory entry is changed. The
	// verified inode remains root-owned, nonwritable and open throughout Start.
	moved := path + ".verified"
	if _, err := os.Lstat(moved); !os.IsNotExist(err) {
		t.Fatal("swap fixture destination is not fresh")
	}
	if os.Rename(path, moved) != nil {
		t.Fatal("owned fixture path replacement unavailable")
	}
	t.Cleanup(func() {
		if err := os.Remove(path); err != nil {
			t.Error("sentinel removal failed")
		}
		if err := os.Rename(moved, path); err != nil {
			t.Error("fixture path restoration failed")
		}
	})
	if os.WriteFile(path, []byte("not an executable"), 0600) != nil {
		t.Fatal("sentinel creation failed")
	}
	sentinel, err := os.CreateTemp(t.TempDir(), "inherited-")
	if err != nil {
		t.Fatal("sentinel open failed")
	}
	defer sentinel.Close()
	var sentinelStat unix.Stat_t
	if unix.Fstat(int(sentinel.Fd()), &sentinelStat) != nil {
		t.Fatal("sentinel stat failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := verifiedWorkerCommand(ctx, executable)
	cmd.ExtraFiles = append(cmd.ExtraFiles, sentinel)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal("stdin unavailable")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal("stdout unavailable")
	}
	// No stderr payload enters logs or test artifacts.
	cmd.Stderr = io.Discard
	if cmd.Start() != nil {
		t.Fatal("verified descriptor execution failed")
	}
	joined := false
	defer func() {
		if !joined {
			_ = stdin.Close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	probeCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	for {
		entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", cmd.Process.Pid))
		if err != nil {
			t.Fatal("live child descriptors unavailable")
		}
		found := false
		for _, entry := range entries {
			var st unix.Stat_t
			if unix.Stat(filepath.Join(fmt.Sprintf("/proc/%d/fd", cmd.Process.Pid), entry.Name()), &st) == nil && st.Dev == sentinelStat.Dev && st.Ino == sentinelStat.Ino {
				found = true
			}
		}
		if !found {
			break
		}
		select {
		case <-probeCtx.Done():
			t.Fatal("inherited sentinel remained open")
		case <-time.After(10 * time.Millisecond):
		}
	}
	request := actualConvertRequest(t)
	wire, err := proto.Marshal(request)
	if err != nil {
		t.Fatal("request encoding failed")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(wire)))
	writeDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(stdin, io.MultiReader(bytes.NewReader(header[:]), bytes.NewReader(wire)))
		closeErr := stdin.Close()
		if err == nil {
			err = closeErr
		}
		writeDone <- err
	}()
	if _, err := io.ReadFull(stdout, header[:]); err != nil {
		t.Fatal("original worker did not respond")
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > MaxParquetBytes+MaxControlBytes {
		t.Fatal("worker output exceeds bound")
	}
	body := make([]byte, int(size))
	if _, err := io.ReadFull(stdout, body); err != nil {
		t.Fatal("worker response incomplete")
	}
	var extra [1]byte
	if n, err := stdout.Read(extra[:]); n != 0 || err != io.EOF {
		t.Fatal("trailing worker output")
	}
	if cmd.Wait() != nil {
		joined = true
		t.Fatal("original worker failed")
	}
	joined = true
	if <-writeDone != nil {
		t.Fatal("request write failed")
	}
	var response workerpb.WorkerResponse
	if proto.Unmarshal(body, &response) != nil || response.ProtocolVersion != 1 || response.GetConverted() == nil || response.GetConverted().RowCount != 2 {
		t.Fatal("path replacement bypassed original positive conversion")
	}
	converted := response.GetConverted()
	sum := sha256.Sum256(converted.Parquet)
	if !bytes.Equal(sum[:], converted.Sha256) {
		t.Fatal("converted digest mismatch")
	}
}
