package replay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/compute/factor"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/engine"
	"github.com/eighred/kanz/internal/risk/publish"
	"github.com/eighred/kanz/internal/risk/state/persist"
	"google.golang.org/protobuf/proto"
)

type manifest struct {
	Version      int
	CodeRevision string
	Portfolio    persist.PortfolioRecord
	Profile      Profile
	Inputs       map[string]json.RawMessage
}

// Record is a frozen evaluation, not a periodic recovery snapshot. The digest
// covers the entire input manifest and is carried on the published measures.
type Record struct {
	PortfolioID v1.PortfolioID
	AsOf        time.Time
	Digest      string
	Manifest    []byte
	Exposure    []byte
	Measures    []byte
}

type Repository interface {
	Save(context.Context, Record) error
	Load(context.Context, v1.PortfolioID, time.Time) (Record, error)
}

type Evaluator struct {
	registry     *compute.Registry
	repository   Repository
	codeRevision string
	profile      Profile
}

func New(registry *compute.Registry, repository Repository, codeRevision string) (*Evaluator, error) {
	if registry == nil || repository == nil || codeRevision == "" {
		return nil, errors.New("durable risk evaluation requires a registry, repository and code revision")
	}
	profile, err := productionProfile(registry, false)
	if err != nil {
		return nil, err
	}
	return &Evaluator{registry: registry, repository: repository, codeRevision: codeRevision, profile: profile}, nil
}

func (e *Evaluator) Compute(ctx context.Context, p *domain.Portfolio, classifier factor.Classifier) (engine.Evaluation, error) {
	if err := ctx.Err(); err != nil {
		return engine.Evaluation{}, err
	}
	profile := e.profile
	profile.Sector = classifier != nil
	in := &inputs{values: make(map[string]json.RawMessage)}
	defer in.release()
	evalCtx := withInputs(ctx, in)
	result := calculate(evalCtx, p, e.registry, classifier)
	if err := ctx.Err(); err != nil {
		return engine.Evaluation{}, err
	}
	if err := in.failure(); err != nil {
		return engine.Evaluation{}, err
	}
	m := manifest{Version: 1, CodeRevision: e.codeRevision, Portfolio: persist.FromPortfolio(p, nil), Profile: profile, Inputs: in.values}
	body, err := json.Marshal(m)
	if err != nil {
		return engine.Evaluation{}, err
	}
	if len(body) > maxInputBytes {
		return engine.Evaluation{}, errors.New("risk manifest exceeds input budget")
	}
	digest := manifestDigest(body)
	result.Measures = withManifest(result.Measures, digest)
	exposure, measures, err := encodeOutputs(result)
	if err != nil {
		return engine.Evaluation{}, err
	}
	record := Record{PortfolioID: p.ID(), AsOf: p.AsOf(), Digest: digest, Manifest: body, Exposure: exposure, Measures: measures}
	if err := e.repository.Save(ctx, record); err != nil {
		return engine.Evaluation{}, err
	}
	return result, nil
}

func (e *Evaluator) Replay(ctx context.Context, id v1.PortfolioID, asOf time.Time) (engine.Evaluation, error) {
	record, err := e.repository.Load(ctx, id, asOf)
	if err != nil {
		if ctx.Err() != nil {
			return engine.Evaluation{}, ctx.Err()
		}
		return engine.Evaluation{}, errors.Join(v1.ErrHistoryUnavailable, err)
	}
	if record.PortfolioID != id || record.AsOf.After(asOf) {
		return engine.Evaluation{}, errors.New("retained risk evaluation identity mismatch")
	}
	result, err := reconstruct(ctx, record)
	if err != nil {
		if ctx.Err() != nil {
			return engine.Evaluation{}, ctx.Err()
		}
		return engine.Evaluation{}, errors.Join(v1.ErrHistoryUnavailable, err)
	}
	return result, nil
}

func reconstruct(ctx context.Context, record Record) (engine.Evaluation, error) {
	if err := ctx.Err(); err != nil {
		return engine.Evaluation{}, err
	}
	if len(record.Manifest) > maxInputBytes || manifestDigest(record.Manifest) != record.Digest {
		return engine.Evaluation{}, errors.New("retained risk input digest mismatch")
	}
	var m manifest
	if err := json.Unmarshal(record.Manifest, &m); err != nil {
		return engine.Evaluation{}, err
	}
	if m.Version != 1 || m.CodeRevision == "" || m.Portfolio.ID != record.PortfolioID || !m.Portfolio.AsOf.Equal(record.AsOf) {
		return engine.Evaluation{}, errors.New("unsupported or inconsistent risk manifest")
	}
	registry, err := m.Profile.registry()
	if err != nil {
		return engine.Evaluation{}, err
	}
	in := &inputs{replay: true, values: m.Inputs}
	defer in.release()
	var classifier factor.Classifier
	if m.Profile.Sector {
		classifier = Classifier{}
	}
	result := calculate(withInputs(ctx, in), m.Portfolio.ToPortfolio(), registry, classifier)
	if err := ctx.Err(); err != nil {
		return engine.Evaluation{}, err
	}
	if err := in.failure(); err != nil {
		return engine.Evaluation{}, err
	}
	result.Measures = withManifest(result.Measures, record.Digest)
	exposure, measures, err := encodeOutputs(result)
	if err != nil {
		return engine.Evaluation{}, err
	}
	if !bytes.Equal(exposure, record.Exposure) || !bytes.Equal(measures, record.Measures) {
		return engine.Evaluation{}, errors.New("historical risk reconstruction differs from retained result")
	}
	return result, nil
}

func calculate(ctx context.Context, p *domain.Portfolio, registry *compute.Registry, classifier factor.Classifier) engine.Evaluation {
	es := compute.ComputeExposure(p)
	if classifier != nil {
		es = factor.ComputeExposure(ctx, p, Classifier{Source: classifier})
	}
	return engine.Evaluation{Exposure: es, Measures: compute.ComputeMeasuresContext(ctx, p, registry, nil), SourcePosition: p.LogPosition()}
}

func withManifest(ms *domain.MeasureSet, digest string) *domain.MeasureSet {
	values := make(map[v1.MeasureName]v1.Measure)
	for _, name := range ms.Names() {
		m, _ := ms.Lookup(name)
		m.Provenance.InputManifestDigest = digest
		values[name] = m
	}
	return domain.NewMeasureSet(ms.PortfolioID(), ms.AsOf(), values, domain.WithCurrencyExclusions(ms.CurrencyExclusions()))
}

func encodeOutputs(result engine.Evaluation) ([]byte, []byte, error) {
	marshal := proto.MarshalOptions{Deterministic: true}
	exposure, err := marshal.Marshal(publish.ToProtoExposureSet(result.Exposure))
	if err != nil {
		return nil, nil, err
	}
	measures, err := marshal.Marshal(publish.ToProtoMeasureSet(result.Measures, nil))
	return exposure, measures, err
}

func manifestDigest(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

var _ engine.Evaluator = (*Evaluator)(nil)
