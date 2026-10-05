package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrMissing = errors.New("no retained risk evaluation at the requested cutoff")

// Postgres uses the authenticated tenant pool, including its immutable tenant
// session setting. No request parameter can select another tenant.
type Postgres struct{ pool *pgxpool.Pool }

func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

func (s *Postgres) Check(ctx context.Context) error {
	for _, table := range []string{"risk_input_objects", "risk_evaluations", "risk_evaluation_inputs"} {
		var protected bool
		err := s.pool.QueryRow(ctx, `SELECT c.relrowsecurity AND c.relforcerowsecurity AND NOT (r.rolsuper OR r.rolbypassrls)
		 FROM pg_class c CROSS JOIN pg_roles r WHERE c.oid=$1::regclass AND r.rolname=current_user`, table).Scan(&protected)
		if err != nil {
			return err
		}
		if !protected {
			return errors.New("risk replay requires FORCE RLS and a non-bypass role")
		}
	}
	return nil
}

func (s *Postgres) Save(ctx context.Context, r Record) error {
	if r.PortfolioID == "" || r.AsOf.IsZero() || !time.Unix(0, r.AsOf.UnixNano()).Equal(r.AsOf) || manifestDigest(r.Manifest) != r.Digest {
		return errors.New("invalid risk evaluation record")
	}
	packed, objects, err := pack(r.Manifest)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Sorted insertion prevents concurrent overlapping portfolios from locking
	// their shared input objects in opposite orders.
	digests := make([]string, 0, len(objects))
	for digest := range objects {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	batch := new(pgx.Batch)
	for _, digest := range digests {
		batch.Queue(`INSERT INTO risk_input_objects(digest,payload) VALUES($1,$2) ON CONFLICT DO NOTHING`, digest, objects[digest])
	}
	if len(digests) > 0 {
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO risk_evaluations(digest,portfolio_id,as_of_ns,manifest,exposure,measures) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, r.Digest, r.PortfolioID, r.AsOf.UnixNano(), packed, r.Exposure, r.Measures)
	if err != nil {
		return err
	}
	var retainedManifest, retainedExposure, retainedMeasures []byte
	err = tx.QueryRow(ctx, `SELECT manifest,exposure,measures FROM risk_evaluations WHERE digest=$1`, r.Digest).Scan(&retainedManifest, &retainedExposure, &retainedMeasures)
	if err != nil {
		return err
	}
	if !bytes.Equal(retainedManifest, packed) || !bytes.Equal(retainedExposure, r.Exposure) || !bytes.Equal(retainedMeasures, r.Measures) {
		return errors.New("risk input identity produced conflicting retained results")
	}
	_, err = tx.Exec(ctx, `INSERT INTO risk_evaluation_inputs(evaluation_digest,input_digest) SELECT $1,unnest($2::text[]) ON CONFLICT DO NOTHING`, r.Digest, digests)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Postgres) Load(ctx context.Context, id v1.PortfolioID, cutoff time.Time) (Record, error) {
	if id == "" || cutoff.IsZero() || !time.Unix(0, cutoff.UnixNano()).Equal(cutoff) {
		return Record{}, ErrMissing
	}
	// Both effective state and recorded knowledge must predate the requested
	// instant. A correction received later cannot rewrite what was known then.
	var r Record
	var packed []byte
	var ns int64
	err := s.pool.QueryRow(ctx, `SELECT portfolio_id,as_of_ns,digest,manifest,exposure,measures FROM risk_evaluations
	 WHERE portfolio_id=$1 AND as_of_ns<=$2 AND recorded_at<=$3 ORDER BY as_of_ns DESC,recorded_at DESC,digest LIMIT 1`, id, cutoff.UnixNano(), cutoff).Scan(&r.PortfolioID, &ns, &r.Digest, &packed, &r.Exposure, &r.Measures)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrMissing
	}
	if err != nil {
		return Record{}, err
	}
	r.AsOf = time.Unix(0, ns).UTC()
	var p packedManifest
	if err := json.Unmarshal(packed, &p); err != nil {
		return Record{}, err
	}
	rows, err := s.pool.Query(ctx, `SELECT o.digest,o.payload FROM risk_input_objects o JOIN risk_evaluation_inputs i ON i.tenant_id=o.tenant_id AND i.input_digest=o.digest WHERE i.evaluation_digest=$1`, r.Digest)
	if err != nil {
		return Record{}, err
	}
	defer rows.Close()
	objects := make(map[string][]byte)
	total := 0
	for rows.Next() {
		var digest string
		var body []byte
		if err := rows.Scan(&digest, &body); err != nil {
			return Record{}, err
		}
		total += len(body)
		if total > maxInputBytes {
			return Record{}, errors.New("retained risk inputs exceed budget")
		}
		objects[digest] = body
	}
	if err := rows.Err(); err != nil {
		return Record{}, err
	}
	r.Manifest, err = unpack(p, objects)
	if err != nil {
		return Record{}, err
	}
	if manifestDigest(r.Manifest) != r.Digest {
		return Record{}, errors.New("retained risk manifest digest mismatch")
	}
	return r, nil
}
