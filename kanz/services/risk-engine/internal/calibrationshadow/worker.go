// Package calibrationshadow compares an isolated Julia calculation with a committed
// Go result. It has no store, bus publisher, or admission-control authority.
package calibrationshadow

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/eighred/kanz/internal/risk/pricing/curve"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	refpb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"
	researchpb "github.com/eighred/kanz/kanz-schemas-go/research/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const MaxBytes = 1 << 20

type Config struct {
	Enabled                            bool
	Executable, Project, Script, Depot string
	Timeout                            time.Duration
}

func (c Config) Validate() error {
	if !c.Enabled {
		if c.Executable != "" || c.Project != "" || c.Script != "" || c.Depot != "" || c.Timeout != 0 {
			return errors.New("Julia shadow configuration supplied while disabled")
		}
		return nil
	}
	if c.Timeout < time.Second || c.Timeout > time.Minute {
		return errors.New("Julia shadow timeout must be between 1s and 1m")
	}
	for _, p := range []string{c.Executable, c.Project, c.Script, c.Depot} {
		if !filepath.IsAbs(p) {
			return errors.New("Julia shadow paths must be absolute")
		}
	}
	if filepath.Clean(c.Script) != filepath.Join(filepath.Clean(c.Project), "bin", "curve_worker.jl") {
		return errors.New("Julia shadow script must belong to configured project")
	}
	for _, p := range []string{c.Executable, c.Script, filepath.Join(c.Project, "Project.toml"), filepath.Join(c.Project, "Manifest.toml")} {
		s, e := os.Stat(p)
		if e != nil || !s.Mode().IsRegular() {
			return errors.New("Julia shadow required file unavailable")
		}
	}
	s, e := os.Stat(c.Depot)
	if e != nil || !s.IsDir() {
		return errors.New("Julia shadow depot unavailable")
	}
	return nil
}

type Worker struct {
	cancel  context.CancelFunc
	done    chan struct{}
	queue   chan curve.CalibrationCapture
	observe func(string)
}

func New(ctx context.Context, c Config, observe func(string)) (*Worker, error) {
	if e := c.Validate(); e != nil {
		return nil, e
	}
	if !c.Enabled {
		return nil, nil
	}
	if observe == nil {
		observe = func(string) {}
	}
	ctx, cancel := context.WithCancel(ctx)
	w := &Worker{cancel: cancel, done: make(chan struct{}), queue: make(chan curve.CalibrationCapture, 1), observe: observe}
	go func() {
		defer close(w.done)
		for {
			select {
			case <-ctx.Done():
				return
			case capture := <-w.queue:
				w.observe(evaluate(ctx, c, capture))
			}
		}
	}()
	return w, nil
}

// Enqueue accepts the detached capture delivered after Store.Put; it never waits
// for a worker and never returns an error into authoritative calibration.
func (w *Worker) Enqueue(c curve.CalibrationCapture) {
	if w == nil {
		return
	}
	select {
	case <-w.done:
		return
	default:
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

func job(c curve.CalibrationCapture) (*researchpb.CurveCalibrationJob, error) {
	bad := errors.New("invalid captured calculation")
	if c.Strip.Capture == nil || len(c.Strip.Capture.Inputs) == 0 || len(c.Strip.Capture.Inputs) > 256 || len(c.Tenors) == 0 || len(c.Tenors) != len(c.Zeros) || len(c.Tenors) > 256 || len(c.Currency) != 3 || c.Interp != curve.LinearZero {
		return nil, bad
	}
	for _, b := range c.Currency {
		if b < 'A' || b > 'Z' {
			return nil, bad
		}
	}
	if c.RequestedAsOf.IsZero() || c.CapturedAt.IsZero() {
		return nil, bad
	}
	j := &researchpb.CurveCalibrationJob{Currency: c.Currency, RequestedAsOf: timestamppb.New(c.RequestedAsOf), CapturedAt: timestamppb.New(c.CapturedAt), Interpolation: refpb.Interpolation_INTERPOLATION_LINEAR_ZERO, LatestObservations: true}
	if j.RequestedAsOf.CheckValid() != nil || j.CapturedAt.CheckValid() != nil {
		return nil, bad
	}
	cov := c.Strip.Coverage
	if cov.Configured < 0 || cov.Configured > 256 || cov.Quoted < 0 || cov.Quoted > cov.Configured || len(cov.Missing) > 256 {
		return nil, bad
	}
	j.Coverage = &domainpb.CalibrationCoverage{Configured: uint32(cov.Configured), Quoted: uint32(cov.Quoted)}
	for _, m := range cov.Missing {
		if len(m.InstrumentID) > 256 || len(m.Reason) > 128 {
			return nil, bad
		}
		j.Coverage.Missing = append(j.Coverage.Missing, &domainpb.MissingQuote{InstrumentId: m.InstrumentID, Reason: m.Reason})
	}
	for _, i := range c.Strip.Capture.Inputs {
		if i.InstrumentID == "" || len(i.InstrumentID) > 256 || !finite(i.Tenor) || i.Tenor <= 0 || i.Tenor > 100 || !finite(i.Span) || math.Abs(i.Span) > 100 || i.Kind < curve.Deposit || i.Kind > curve.Swap {
			return nil, bad
		}
		if i.Observation != nil && (i.Observation.InstrumentId != i.InstrumentID || i.Observation.EventTime == nil || i.Observation.EventTime.CheckValid() != nil) {
			return nil, bad
		}
		if i.ResolvedRate != nil && !finite(*i.ResolvedRate) {
			return nil, bad
		}
		input := &researchpb.CalibrationInput{InstrumentId: i.InstrumentID, Kind: researchpb.RateQuoteKind(int32(i.Kind) + 1), TenorYears: i.Tenor, SpanYears: i.Span, Observation: i.Observation}
		if i.ResolvedRate != nil {
			input.Rate = &researchpb.CalibrationInput_ResolvedRate{ResolvedRate: *i.ResolvedRate}
		}
		j.Inputs = append(j.Inputs, input)
	}
	for i, t := range c.Tenors {
		if !finite(t) || t <= 0 || t > 100 || !finite(c.Zeros[i]) || (i > 0 && t <= c.Tenors[i-1]) {
			return nil, bad
		}
	}
	var id [16]byte
	if _, e := rand.Read(id[:]); e != nil {
		return nil, bad
	}
	j.CaptureId = hex.EncodeToString(id[:])
	if proto.Size(j) > MaxBytes-64 {
		return nil, bad
	}
	b, e := proto.MarshalOptions{Deterministic: true}.Marshal(j)
	if e != nil {
		return nil, bad
	}
	d := sha256.Sum256(b)
	j.CaptureDigest = d[:]
	return j, nil
}

func compare(j *researchpb.CurveCalibrationJob, c curve.CalibrationCapture, r *researchpb.CurveCalibrationResult) string {
	if r.CaptureId != j.CaptureId || !bytes.Equal(r.CaptureDigest, j.CaptureDigest) || len(r.ProtoReflect().GetUnknown()) != 0 {
		return "invalid_result"
	}
	if r.Failure != researchpb.CurveCalibrationFailure_CURVE_CALIBRATION_FAILURE_NONE {
		if r.Failure < researchpb.CurveCalibrationFailure_CURVE_CALIBRATION_FAILURE_INVALID_INPUT || r.Failure > researchpb.CurveCalibrationFailure_CURVE_CALIBRATION_FAILURE_NUMERICAL_FAILURE || len(r.Pillars) != 0 {
			return "invalid_result"
		}
		return "worker_rejected"
	}
	if len(r.Pillars) != len(c.Tenors) {
		return "invalid_result"
	}
	for i, p := range r.Pillars {
		if p == nil || len(p.ProtoReflect().GetUnknown()) != 0 || !finite(p.TenorYears) || p.TenorYears <= 0 || !finite(p.ZeroRate) || math.Abs(p.TenorYears-c.Tenors[i]) > 1e-12 {
			return "invalid_result"
		}
		a, b := math.Exp(-p.ZeroRate*p.TenorYears), math.Exp(-c.Zeros[i]*c.Tenors[i])
		if !finite(a) || !finite(b) || a <= 0 || b <= 0 {
			return "invalid_result"
		}
		if math.Abs(p.ZeroRate-c.Zeros[i]) > 1e-10 || math.Abs(a-b) > 1e-12 {
			return "disagreement"
		}
	}
	return "matched"
}

type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	cancel   context.CancelFunc
	exceeded bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buffer.Len() {
		b.exceeded = true
		b.cancel()
		return 0, errors.New("worker output limit")
	}
	return b.buffer.Write(p)
}

func execute(ctx context.Context, c Config, input []byte) ([]byte, string) {
	return runProcess(ctx, c, input, []string{"--startup-file=no", "--history-file=no", "--compiled-modules=existing", "--pkgimages=existing", "--project=" + c.Project, c.Script})
}

func runProcess(ctx context.Context, c Config, input []byte, args []string) ([]byte, string) {
	if len(input) > MaxBytes {
		return nil, "invalid_input"
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	scratch, e := os.MkdirTemp("", "kanz-julia-shadow-")
	if e != nil {
		return nil, "worker_failure"
	}
	defer os.RemoveAll(scratch)
	cmd := exec.CommandContext(ctx, c.Executable, args...)
	cmd.Dir = c.Project
	cmd.Env = []string{"JULIA_PKG_OFFLINE=true", "JULIA_LOAD_PATH=@" + string(os.PathListSeparator) + "@stdlib", "JULIA_DEPOT_PATH=" + c.Depot, "JULIA_NUM_THREADS=1", "OPENBLAS_NUM_THREADS=1", "HOME=" + scratch, "TMPDIR=" + scratch, "TEMP=" + scratch, "TMP=" + scratch}
	if v := os.Getenv("SystemRoot"); v != "" {
		cmd.Env = append(cmd.Env, "SystemRoot="+v)
	}
	cmd.Stdin = bytes.NewReader(input)
	out := &boundedBuffer{limit: MaxBytes, cancel: cancel}
	stderr := &boundedBuffer{limit: 32 << 10, cancel: cancel}
	cmd.Stdout = out
	cmd.Stderr = stderr
	cmd.WaitDelay = time.Second
	e = cmd.Run()
	if out.exceeded || stderr.exceeded {
		return nil, "output_limit"
	}
	if ctx.Err() != nil {
		return nil, "timeout"
	}
	if e != nil {
		return nil, "worker_failure"
	}
	return out.buffer.Bytes(), ""
}

func evaluate(ctx context.Context, cfg Config, c curve.CalibrationCapture) string {
	j, e := job(c)
	if e != nil {
		return "invalid_input"
	}
	b, e := proto.Marshal(j)
	if e != nil {
		return "invalid_input"
	}
	out, status := execute(ctx, cfg, b)
	if status != "" {
		return status
	}
	r := new(researchpb.CurveCalibrationResult)
	if proto.Unmarshal(out, r) != nil {
		return "invalid_result"
	}
	return compare(j, c, r)
}
