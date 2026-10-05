// Package artifacts materializes immutable model FACTs for risk reconstruction.
package artifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/risk/factormodel"
	"github.com/eighred/kanz/internal/risk/pricing/curve"
	"github.com/eighred/kanz/internal/risk/publish"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	factorpb "github.com/eighred/kanz/kanz-schemas-go/factor/v1"
)

var (
	ErrMissing  = errors.New("risk artifact not retained")
	ErrConflict = errors.New("risk artifact identity has conflicting content")
)

// Store borrows an authenticated tenant pool; it never changes its tenant GUC.
type Store struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Check refuses startup without the migration and its tenant isolation.
func (s *Store) Check(ctx context.Context) error {
	var protected bool
	err := s.pool.QueryRow(ctx, `SELECT c.relrowsecurity AND c.relforcerowsecurity AND NOT (r.rolsuper OR r.rolbypassrls)
		FROM pg_class c CROSS JOIN pg_roles r WHERE c.oid='risk_model_artifacts'::regclass AND r.rolname=current_user`).Scan(&protected)
	if err != nil {
		return err
	}
	if !protected {
		return errors.New("risk artifact store requires FORCE RLS and a non-bypass role")
	}
	return nil
}

func (s *Store) SaveCurve(ctx context.Context, a *domainpb.CalibratedCurve) error {
	if _, err := curve.FromArtifact(a); err != nil {
		return err
	}
	return s.save(ctx, "curve", a.Curve.CurrencyCode, a.Curve.AsOf.AsTime(), a)
}

func (s *Store) SaveModel(ctx context.Context, a *factorpb.FactorModelSnapshot) error {
	if _, err := factormodel.FromSnapshot(a); err != nil {
		return err
	}
	return s.save(ctx, "factor", a.Model.ModelId, a.Model.AsOf.AsTime(), a)
}

func (s *Store) save(ctx context.Context, kind, key string, asOf time.Time, msg proto.Message) error {
	if !time.Unix(0, asOf.UnixNano()).Equal(asOf) {
		return errors.New("risk artifact timestamp outside nanosecond domain")
	}
	body, err := (proto.MarshalOptions{Deterministic: true}).Marshal(msg)
	if err != nil {
		return err
	}
	if len(body) > 8<<20 {
		return errors.New("risk artifact exceeds size limit")
	}
	digest := sha256.Sum256(body)
	_, err = s.pool.Exec(ctx, `INSERT INTO risk_model_artifacts (kind,artifact_key,as_of_ns,payload,digest)
		VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, kind, key, asOf.UnixNano(), body, digest[:])
	if err != nil {
		return err
	}
	// Separate statement: a concurrent insertion may have committed while our
	// INSERT waited, after that statement's read snapshot was established.
	var retained []byte
	err = s.pool.QueryRow(ctx, `SELECT digest FROM risk_model_artifacts WHERE kind=$1 AND artifact_key=$2 AND as_of_ns=$3`, kind, key, asOf.UnixNano()).Scan(&retained)
	if err != nil {
		return err
	}
	if !bytes.Equal(retained, digest[:]) {
		return ErrConflict
	}
	return nil
}

// CurveAt uses both effective and knowledge cutoffs. No cache horizon limits
// this read and a missing row never falls back to a live calibration.
func (s *Store) CurveAt(ctx context.Context, currency string, asOf, knownAt time.Time) (*curve.Curve, error) {
	a := new(domainpb.CalibratedCurve)
	if err := s.load(ctx, "curve", currency, asOf, knownAt, a); err != nil {
		return nil, err
	}
	return curve.FromArtifact(a)
}

func (s *Store) ModelAt(ctx context.Context, modelID string, asOf, knownAt time.Time) (*factormodel.Model, error) {
	a := new(factorpb.FactorModelSnapshot)
	if err := s.load(ctx, "factor", modelID, asOf, knownAt, a); err != nil {
		return nil, err
	}
	return factormodel.FromSnapshot(a)
}

func (s *Store) load(ctx context.Context, kind, key string, asOf, knownAt time.Time, out proto.Message) error {
	if key == "" || asOf.IsZero() || knownAt.IsZero() || !time.Unix(0, asOf.UnixNano()).Equal(asOf) {
		return ErrMissing
	}
	var body, retained []byte
	err := s.pool.QueryRow(ctx, `SELECT payload,digest FROM risk_model_artifacts
		WHERE kind=$1 AND artifact_key=$2 AND as_of_ns <= $3 AND recorded_at <= $4
		ORDER BY as_of_ns DESC LIMIT 1`, kind, key, asOf.UnixNano(), knownAt).Scan(&body, &retained)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrMissing
	}
	if err != nil {
		return err
	}
	digest := sha256.Sum256(body)
	if !bytes.Equal(retained, digest[:]) {
		return fmt.Errorf("%w: retained digest mismatch", ErrConflict)
	}
	return proto.Unmarshal(body, out)
}

// RecordCurve and RecordModel adapt live producers to the same canonical
// validation and immutable insertion used by FACT materialization.
func (s *Store) RecordCurve(ctx context.Context, currency string, asOf time.Time, c *curve.Curve) error {
	if c == nil {
		return publish.ErrNoCurve
	}
	return s.SaveCurve(ctx, publish.ToProtoCalibratedCurve(currency, asOf, c))
}

func (s *Store) RecordModel(ctx context.Context, m *factormodel.Model) error {
	if m == nil {
		return publish.ErrModelNamesNoInstance
	}
	return s.SaveModel(ctx, publish.ToProtoFactorModelSnapshot(m))
}
