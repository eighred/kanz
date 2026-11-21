package terms

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"
	"github.com/jackc/pgx/v5"
)

// RecordedOption identifies the original stored ContractTerms bytes and the
// database ingestion timestamp. It makes no assertion about premium units.
type RecordedOption struct {
	Terms      *referencepb.ContractTerms
	IngestedAt time.Time
	SHA256     [sha256.Size]byte
}

var ErrNotOption = errors.New("terms: selected revision is not an option")

// OptionAt selects the newest effective revision that meets both inclusive
// cutoffs. ingested_at is the table's transaction timestamp, not a commit
// timestamp. A caller replaying an earlier read can pin the returned digest.
func (p *Postgres) OptionAt(ctx context.Context, instrument string, effectiveAt, ingestedBy time.Time) (RecordedOption, error) {
	if instrument == "" || strings.TrimSpace(instrument) != instrument || effectiveAt.IsZero() || ingestedBy.IsZero() {
		return RecordedOption{}, errors.New("terms: canonical instrument and both cutoffs are required")
	}
	if effectiveAt.Nanosecond()%1000 != 0 || ingestedBy.Nanosecond()%1000 != 0 {
		return RecordedOption{}, errors.New("terms: cutoffs require PostgreSQL microsecond precision")
	}
	var rec Record
	var underlying *string
	var kind string
	var blob []byte
	var result RecordedOption
	// Resolve across all kinds first. A later non-option amendment supersedes
	// the older option at its effective time.
	err := p.pool.QueryRow(ctx, `SELECT instrument_id, as_of, underlying_id, kind, terms, ingested_at
		FROM contract_terms WHERE instrument_id = $1 AND as_of <= $2 AND ingested_at <= $3
		ORDER BY as_of DESC LIMIT 1`, instrument, effectiveAt.UTC(), ingestedBy.UTC()).
		Scan(&rec.InstrumentID, &rec.AsOf, &underlying, &kind, &blob, &result.IngestedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RecordedOption{}, ErrNoTerms
	}
	if err != nil {
		return RecordedOption{}, fmt.Errorf("terms: read recorded option: %w", err)
	}
	rec, err = decodeRecord(rec, underlying, kind, blob)
	if err != nil {
		return RecordedOption{}, err
	}
	if rec.Kind != KindOption {
		return RecordedOption{}, ErrNotOption
	}
	result.Terms = rec.Terms
	result.SHA256 = sha256.Sum256(blob)
	return result, nil
}
