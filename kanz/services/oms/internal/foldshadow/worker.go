// Package foldshadow compares captured OMS fill folds without publication or admission authority.
package foldshadow

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"google.golang.org/protobuf/types/known/timestamppb"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/shadowprocess"
	omspb "github.com/eighred/kanz/kanz-schemas-go/oms/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type Config struct {
	Enabled                     bool
	Executable, ExpectedVersion string
	Timeout                     time.Duration
}

func (c Config) Validate() error {
	if !c.Enabled {
		if c.Executable != "" || c.ExpectedVersion != "" || c.Timeout != 0 {
			return errors.New("Rust OMS fold shadow configuration supplied while disabled")
		}
		return nil
	}
	if !filepath.IsAbs(c.Executable) || !identifier(c.ExpectedVersion) || c.Timeout < time.Second || c.Timeout > time.Minute {
		return errors.New("Rust OMS fold shadow requires absolute executable, expected version and timeout between 1s and 1m")
	}
	s, e := os.Stat(c.Executable)
	if e != nil || !s.Mode().IsRegular() {
		return errors.New("Rust OMS fold shadow executable unavailable")
	}
	return nil
}
func identifier(s string) bool {
	return s != "" && len(s) <= 256 && strings.IndexFunc(s, func(r rune) bool { return r < 32 || r == 127 }) < 0
}

type Worker struct {
	cancel  context.CancelFunc
	done    chan struct{}
	queue   chan order.FillFoldCapture
	observe func(string)
}

func New(ctx context.Context, c Config, tenant, sourceRevision string, observe func(string)) (*Worker, error) {
	if e := c.Validate(); e != nil {
		return nil, e
	}
	if !c.Enabled {
		return nil, nil
	}
	if !identifier(tenant) || !identifier(sourceRevision) {
		return nil, errors.New("Rust OMS fold shadow requires tenant and source revision")
	}
	if observe == nil {
		observe = func(string) {}
	}
	ctx, cancel := context.WithCancel(ctx)
	w := &Worker{cancel: cancel, done: make(chan struct{}), queue: make(chan order.FillFoldCapture, 1), observe: observe}
	go func() {
		defer close(w.done)
		for {
			select {
			case <-ctx.Done():
				return
			case capture := <-w.queue:
				if ctx.Err() != nil {
					return
				}
				w.observe(evaluate(ctx, c, tenant, sourceRevision, capture))
			}
		}
	}()
	return w, nil
}

// Submit accepts a detached capture; a full queue never delays authoritative fill recording.
func (w *Worker) Submit(c order.FillFoldCapture) {
	if w == nil {
		return
	}
	select {
	case <-w.done:
		return
	default:
	}
	if c.Unsupported || c.State == nil || c.Fill == nil || proto.Size(c.State)+proto.Size(c.Fill)+proto.Size(c.Candidate) > shadowprocess.MaxBytes-1024 {
		w.observe("invalid_input")
		return
	}
	// Internal callers cannot mutate queued inputs after Submit returns.
	c.State = proto.Clone(c.State).(*orderpb.OrderState)
	c.Fill = proto.Clone(c.Fill).(*orderpb.Fill)
	if c.Candidate != nil {
		c.Candidate = proto.Clone(c.Candidate).(*orderpb.OrderState)
	}
	select {
	case w.queue <- c:
	default:
		w.observe("queue_full")
	}
}
func (w *Worker) Close() {
	if w != nil {
		w.cancel()
		<-w.done
	}
}
func job(c order.FillFoldCapture, tenant, revision string) (*omspb.FillFoldRequest, error) {
	bad := errors.New("invalid captured fold")
	if c.Unsupported || c.State == nil || c.Fill == nil || c.Tenant != tenant || !identifier(tenant) || !identifier(revision) || c.ExpectedVersion < 1 || c.EventTime.IsZero() || c.CapturedAt.IsZero() || unknown(c.State.ProtoReflect()) || unknown(c.Fill.ProtoReflect()) {
		return nil, bad
	}
	if c.Path != "live" && c.Path != "reconcile" && c.Path != "recovery" {
		return nil, bad
	}
	var id [16]byte
	if _, e := rand.Read(id[:]); e != nil {
		return nil, e
	}
	j := &omspb.FillFoldRequest{CaptureId: hex.EncodeToString(id[:]), TenantId: tenant, SourceRevision: revision, ExpectedVersion: proto.Int64(c.ExpectedVersion), State: c.State, Fill: c.Fill, EventTime: timestamppb.New(c.EventTime), CapturedAt: timestamppb.New(c.CapturedAt), Path: c.Path}
	if j.EventTime.CheckValid() != nil || j.CapturedAt.CheckValid() != nil || proto.Size(j) > shadowprocess.MaxBytes-64 {
		return nil, bad
	}
	wire, e := proto.MarshalOptions{Deterministic: true}.Marshal(j)
	if e != nil {
		return nil, e
	}
	digest := sha256.Sum256(wire)
	j.CaptureDigest = digest[:]
	return j, nil
}

// Unknown fields anywhere in the typed result are refused, including nested Decimals.
func unknown(m protoreflect.Message) bool {
	if len(m.GetUnknown()) != 0 {
		return true
	}
	found := false
	m.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if f.Kind() != protoreflect.MessageKind && f.Kind() != protoreflect.GroupKind {
			return true
		}
		if f.IsList() {
			l := v.List()
			for i := 0; i < l.Len(); i++ {
				if unknown(l.Get(i).Message()) {
					found = true
					return false
				}
			}
		} else if unknown(v.Message()) {
			found = true
		}
		return !found
	})
	return found
}
func compare(j *omspb.FillFoldRequest, c order.FillFoldCapture, r *omspb.FillFoldResult, version string) string {
	if unknown(r.ProtoReflect()) || r.CaptureId != j.CaptureId || !bytes.Equal(r.CaptureDigest, j.CaptureDigest) || r.TenantId != j.TenantId || r.ProducerVersion != version || r.ExpectedVersion == nil || r.GetExpectedVersion() != j.GetExpectedVersion() {
		return "invalid_result"
	}
	switch r.Status {
	case omspb.FillFoldStatus_FILL_FOLD_STATUS_OK:
		if r.Candidate == nil || r.RejectionCode != "" {
			return "invalid_result"
		}
		if c.RejectionCode != "" || c.NumericalFailure || !proto.Equal(c.Candidate, r.Candidate) {
			return "disagreement"
		}
		return "matched"
	case omspb.FillFoldStatus_FILL_FOLD_STATUS_REJECTED:
		if r.Candidate != nil || r.RejectionCode == "" {
			return "invalid_result"
		}
		if r.RejectionCode != c.RejectionCode || c.Candidate != nil || c.NumericalFailure {
			return "disagreement"
		}
		return "matched"
	case omspb.FillFoldStatus_FILL_FOLD_STATUS_NUMERICAL_FAILURE:
		if r.Candidate != nil || r.RejectionCode != "" {
			return "invalid_result"
		}
		if !c.NumericalFailure {
			return "disagreement"
		}
		return "matched"
	case omspb.FillFoldStatus_FILL_FOLD_STATUS_UNSUPPORTED_INPUT:
		if r.Candidate != nil || r.RejectionCode != "" {
			return "invalid_result"
		}
		return "worker_rejected"
	default:
		return "invalid_result"
	}
}
func evaluate(ctx context.Context, c Config, tenant, revision string, capture order.FillFoldCapture) string {
	j, e := job(capture, tenant, revision)
	if e != nil {
		return "invalid_input"
	}
	wire, e := proto.MarshalOptions{Deterministic: true}.Marshal(j)
	if e != nil {
		return "invalid_input"
	}
	output, status := shadowprocess.Run(ctx, shadowprocess.Config{Executable: c.Executable, Timeout: c.Timeout}, wire)
	if status != "" {
		return status
	}
	result := new(omspb.FillFoldResult)
	if proto.Unmarshal(output, result) != nil {
		return "invalid_result"
	}
	return compare(j, capture, result, c.ExpectedVersion)
}
