// Package bondshadow compares captured bond risk without publication or admission authority.
package bondshadow

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/pricing"
	"github.com/eighred/kanz/internal/shadowprocess"
	quantpb "github.com/eighred/kanz/kanz-schemas-go/quant/v1"
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
			return errors.New("Rust bond shadow configuration supplied while disabled")
		}
		return nil
	}
	if !filepath.IsAbs(c.Executable) || !identifier(c.ExpectedVersion) || c.Timeout < time.Second || c.Timeout > time.Minute {
		return errors.New("Rust bond shadow requires absolute executable, expected version and timeout between 1s and 1m")
	}
	s, e := os.Stat(c.Executable)
	if e != nil || !s.Mode().IsRegular() {
		return errors.New("Rust bond shadow executable unavailable")
	}
	return nil
}
func identifier(s string) bool {
	return s != "" && len(s) <= 256 && strings.IndexFunc(s, func(r rune) bool { return r < 32 || r == 127 }) < 0
}

type Worker struct {
	cancel  context.CancelFunc
	done    chan struct{}
	queue   chan compute.BondRiskCapture
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
		return nil, errors.New("Rust bond shadow requires tenant and source revision")
	}
	if observe == nil {
		observe = func(string) {}
	}
	ctx, cancel := context.WithCancel(ctx)
	w := &Worker{cancel: cancel, done: make(chan struct{}), queue: make(chan compute.BondRiskCapture, 1), observe: observe}
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

// Submit accepts a detached capture; a full queue never delays authoritative pricing.
func (w *Worker) Submit(c compute.BondRiskCapture) {
	if w == nil {
		return
	}
	select {
	case <-w.done:
		return
	default:
	}
	// Capture terms remain private even if an internal caller retains its pointer.
	if c.Terms != nil {
		c.Terms = pricing.CloneBoundedBondTerms(c.Terms)
		if c.Terms == nil {
			w.observe("invalid_input")
			return
		}
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
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func job(c compute.BondRiskCapture, tenant, revision string) (*quantpb.BondRiskRequest, error) {
	var id [16]byte
	if _, e := rand.Read(id[:]); e != nil {
		return nil, e
	}
	return pricing.ProjectBondRisk(c.Terms, c.Bond, c.Curve, pricing.BondRiskCaptureMeta{CaptureID: hex.EncodeToString(id[:]), InstrumentID: c.InstrumentID, TenantID: tenant, SourceRevision: revision, AsOf: c.AsOf, CapturedAt: c.CapturedAt})
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
func near(a, b, abs, rel float64) bool {
	return finite(a) && finite(b) && math.Abs(a-b) <= abs+rel*math.Abs(b)
}
func compare(j *quantpb.BondRiskRequest, c compute.BondRiskCapture, r *quantpb.BondRiskResult, version string) string {
	if unknown(r.ProtoReflect()) || r.CaptureId != j.CaptureId || !bytes.Equal(r.CaptureDigest, j.CaptureDigest) || r.TenantId != j.TenantId || r.ProducerVersion != version {
		return "invalid_result"
	}
	if r.Status != quantpb.BondRiskStatus_BOND_RISK_STATUS_OK {
		if r.Status < quantpb.BondRiskStatus_BOND_RISK_STATUS_INVALID_INPUT || r.Status > quantpb.BondRiskStatus_BOND_RISK_STATUS_NUMERICAL_FAILURE || r.Values != nil {
			return "invalid_result"
		}
		return "worker_rejected"
	}
	v := r.Values
	if v == nil || v.EffectiveDuration == nil || v.Convexity == nil {
		return "invalid_result"
	}
	price, ok := dec.Float64(v.DirtyPrice)
	if !ok {
		return "invalid_result"
	}
	dv01, ok := dec.Float64(v.Dv01)
	if !ok || price < 0 || dv01 < 0 || !finite(*v.EffectiveDuration) || !finite(*v.Convexity) {
		return "invalid_result"
	}
	if !near(price, c.Result.DirtyPrice, 1e-9, 1e-11) || !near(dv01, c.Result.DV01, 1e-9, 1e-9) || !near(*v.EffectiveDuration, c.Result.EffectiveDuration, 1e-7, 1e-9) || !near(*v.Convexity, c.Result.Convexity, 2e-5, 1e-8) {
		return "disagreement"
	}
	return "matched"
}
func evaluate(ctx context.Context, c Config, tenant, revision string, capture compute.BondRiskCapture) string {
	j, e := job(capture, tenant, revision)
	if e != nil {
		return "invalid_input"
	}
	b, e := proto.Marshal(j)
	if e != nil {
		return "invalid_input"
	}
	out, status := shadowprocess.Run(ctx, shadowprocess.Config{Executable: c.Executable, Timeout: c.Timeout}, b)
	if status != "" {
		return status
	}
	r := new(quantpb.BondRiskResult)
	if proto.Unmarshal(out, r) != nil {
		return "invalid_result"
	}
	return compare(j, capture, r, c.ExpectedVersion)
}
