package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	workerpb "github.com/eighred/kanz/kanz-schemas-go/lake/worker/v1"
	"google.golang.org/protobuf/proto"
)

var ErrWorker = errors.New("snapshot worker refused or unavailable")
var ErrBusy = errors.New("snapshot operation busy")

// Worker invokes only the configured, digest-verified executable. The binary
// installs its own reviewed Linux sandbox before reading this private protocol.
// No caller may choose a program, arguments, environment, SQL or object path.
type Worker struct {
	path      string
	digest    [32]byte
	slot      chan struct{}
	failed    atomic.Bool
	onFailure func()
}

// Operation reserves the whole owner data path before large reads or encoding.
// Busy callers refuse immediately rather than retaining a queue of large inputs.
type Operation struct {
	worker          *Worker
	references      atomic.Int32
	mu              sync.Mutex
	closed          bool
	used            bool
	childPending    bool
	joinedCallbacks []func()
}

func (w *Worker) Reserve() (*Operation, error) {
	if w.failed.Load() {
		return nil, ErrWorker
	}
	select {
	case w.slot <- struct{}{}:
		if w.failed.Load() {
			<-w.slot
			return nil, ErrWorker
		}
		op := &Operation{worker: w}
		op.references.Store(1)
		return op, nil
	default:
		return nil, ErrBusy
	}
}

func (op *Operation) release() {
	if op.references.Add(-1) == 0 {
		<-op.worker.slot
	}
}

func (op *Operation) Close() {
	op.mu.Lock()
	defer op.mu.Unlock()
	if !op.closed {
		op.closed = true
		op.release()
	}
}

func (op *Operation) afterChildJoined(callback func()) {
	op.mu.Lock()
	if op.childPending {
		op.joinedCallbacks = append(op.joinedCallbacks, callback)
		op.mu.Unlock()
		return
	}
	op.mu.Unlock()
	callback()
}

func (op *Operation) childJoined() {
	op.mu.Lock()
	op.childPending = false
	callbacks := op.joinedCallbacks
	op.joinedCallbacks = nil
	op.mu.Unlock()
	for _, callback := range callbacks {
		callback()
	}
}

func NewWorker(path string, digest [32]byte, onFailure func()) (*Worker, error) {
	if path == "" || digest == ([32]byte{}) || onFailure == nil {
		return nil, ErrInvalid
	}
	f, err := openWorkerExecutable(path, digest)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, ErrWorker
	}
	return &Worker{path: path, digest: digest, slot: make(chan struct{}, 1), onFailure: onFailure}, nil
}

// Invoke is bounded by the same outer work deadline, with a maximum thirty
// seconds and separate five-second kill/join observation. The concurrency slot
// is released only when Wait actually completes, including on cleanup failure.
func (w *Worker) Invoke(ctx context.Context, request *workerpb.WorkerRequest, parquet []byte) (*workerpb.WorkerResponse, error) {
	op, err := w.Reserve()
	if err != nil {
		return nil, err
	}
	defer op.Close()
	return op.Invoke(ctx, request, parquet)
}

func (op *Operation) Invoke(ctx context.Context, request *workerpb.WorkerRequest, parquet []byte) (*workerpb.WorkerResponse, error) {
	op.mu.Lock()
	if op.closed || op.used {
		op.mu.Unlock()
		return nil, ErrInvalid
	}
	op.used = true
	op.references.Add(1)
	op.mu.Unlock()
	defer op.release()
	w := op.worker
	if request == nil || request.ProtocolVersion != ProtocolVersion {
		return nil, ErrInvalid
	}
	limit := MaxCaptureBytes
	outputLimit := MaxParquetBytes + MaxControlBytes
	switch {
	case request.GetConvert() != nil:
		if len(parquet) != 0 || request.GetConvert().SchemaVersion != SchemaVersion || len(request.GetConvert().Rows) > MaxRows {
			return nil, ErrInvalid
		}
	case request.GetQuery() != nil:
		limit = MaxControlBytes
		outputLimit = MaxResponseBytes
		q := request.GetQuery()
		digest := sha256.Sum256(parquet)
		if q.SchemaVersion != SchemaVersion || !boundedText(q.ExpectedTenant, 256) || q.ExpectedRowCount > MaxRows || ValidateQuery(q.Query) != nil || len(parquet) == 0 || len(parquet) > MaxParquetBytes || uint64(len(parquet)) != q.ParquetBytes || !bytes.Equal(digest[:], q.ParquetSha256) {
			return nil, ErrInvalid
		}
	default:
		return nil, ErrInvalid
	}
	if proto.Size(request) > limit {
		return nil, ErrCapacity
	}
	wire, err := proto.Marshal(request)
	if err != nil {
		return nil, ErrInvalid
	}
	workCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	executable, err := openWorkerExecutable(w.path, w.digest)
	if err != nil {
		return nil, err
	}
	defer executable.Close()
	cmd := verifiedWorkerCommand(workCtx, executable)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, ErrWorker
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, ErrWorker
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, ErrWorker
	}
	if err = cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, ErrWorker
	}
	op.references.Add(1)
	op.mu.Lock()
	op.childPending = true
	op.mu.Unlock()
	joined := true
	defer func() {
		if joined {
			op.childJoined()
			op.release()
		}
	}()
	writeDone := make(chan error, 1)
	go func() {
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], uint32(len(wire)))
		_, err := io.Copy(stdin, io.MultiReader(bytes.NewReader(header[:]), bytes.NewReader(wire), bytes.NewReader(parquet)))
		closeErr := stdin.Close()
		if err == nil {
			err = closeErr
		}
		writeDone <- err
	}()
	type readResult struct {
		body []byte
		err  error
	}
	readDone := make(chan readResult, 1)
	go func() {
		var header [4]byte
		if _, err := io.ReadFull(stdout, header[:]); err != nil {
			readDone <- readResult{err: err}
			return
		}
		size := binary.BigEndian.Uint32(header[:])
		if size == 0 || uint64(size) > uint64(outputLimit) {
			cancel()
			readDone <- readResult{err: ErrCapacity}
			return
		}
		body := make([]byte, int(size))
		if _, err := io.ReadFull(stdout, body); err != nil {
			readDone <- readResult{err: err}
			return
		}
		var extra [1]byte
		n, err := stdout.Read(extra[:])
		if n != 0 || err != io.EOF {
			readDone <- readResult{err: ErrWorker}
			return
		}
		readDone <- readResult{body: body}
	}()
	errorDone := make(chan error, 1)
	go func() {
		n, err := io.Copy(io.Discard, io.LimitReader(stderr, 8193))
		if n > 8192 {
			cancel()
			errorDone <- ErrCapacity
			return
		}
		errorDone <- err
	}()
	// Wait follows pipe readers: os/exec may close pipes when Wait returns.
	var output readResult
	readJoined := false
	select {
	case output = <-readDone:
		readJoined = true
	case <-workCtx.Done():
		output.err = workCtx.Err()
		cancel()
	}
	if output.err != nil {
		cancel()
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	join := time.NewTimer(5 * time.Second)
	defer join.Stop()
	var waitErr error
	select {
	case waitErr = <-waitDone:
	case <-join.C:
		_ = cmd.Process.Kill()
		if !w.failed.Swap(true) && w.onFailure != nil {
			w.onFailure()
		}
		joined = false
		go func() {
			<-waitDone
			if !readJoined {
				<-readDone
			}
			<-writeDone
			<-errorDone
			op.childJoined()
			op.release()
		}()
		return nil, ErrWorker
	}
	if !readJoined {
		<-readDone
	}
	writeErr := <-writeDone
	stderrErr := <-errorDone
	if output.err != nil || waitErr != nil || writeErr != nil || stderrErr != nil || workCtx.Err() != nil {
		return nil, ErrWorker
	}
	var response workerpb.WorkerResponse
	if proto.Unmarshal(output.body, &response) != nil || len(response.ProtoReflect().GetUnknown()) != 0 || response.ProtocolVersion != ProtocolVersion || response.GetFailure() != nil {
		return nil, ErrWorker
	}
	if request.GetConvert() != nil {
		c := response.GetConverted()
		if c == nil || len(c.ProtoReflect().GetUnknown()) != 0 || c.RowCount != uint64(len(request.GetConvert().Rows)) || len(c.Parquet) == 0 || len(c.Parquet) > MaxParquetBytes {
			return nil, ErrWorker
		}
		digest := sha256.Sum256(c.Parquet)
		if !bytes.Equal(digest[:], c.Sha256) {
			return nil, ErrWorker
		}
	} else if ValidateResult(request.GetQuery().Query, response.GetQuery(), request.GetQuery().ExpectedRowCount) != nil {
		return nil, ErrWorker
	}
	return &response, nil
}

// Keep the verified descriptor, rather than re-opening its original pathname.
// The trusted executable closes inherited descriptors before reading input.
func verifiedWorkerCommand(ctx context.Context, executable *os.File) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/proc/self/fd/3")
	cmd.Env = []string{}
	cmd.ExtraFiles = []*os.File{executable}
	cmd.WaitDelay = 5 * time.Second
	return cmd
}
