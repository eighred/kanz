package ledger

import (
	"context"
	"errors"
	"strings"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

var (
	ErrCashReplayBounds = errors.New("ledger: invalid cash replay bounds")
	ErrNoCashHistory    = errors.New("ledger: cash history unavailable")
)

const maxCashReplayBytes = 4 << 20

// CashCommitRecord carries the original protobuf bytes, not a reconstructed
// balance. JSON encodes Payload as base64 and revisions as strings so an
// operator client cannot round an int64 revision or rewrite Decimal evidence.
type CashCommitRecord struct {
	Revision int64  `json:"revision,string"`
	Payload  []byte `json:"payload"`
}

type CashCommitPage struct {
	PortfolioID string             `json:"portfolio_id"`
	Currency    string             `json:"currency"`
	Through     int64              `json:"through_revision,string"`
	Next        int64              `json:"next_revision,string"`
	HasMore     bool               `json:"has_more"`
	Records     []CashCommitRecord `json:"records"`
}

// ReadCashCommits returns a contiguous, bounded slice of retained source
// evidence. through=0 freezes the current committed head; every subsequent page
// must use the returned Through to avoid chasing a continuously advancing book.
// It neither publishes facts nor authorizes orders or clears quarantine.
func (p *Postgres) ReadCashCommits(ctx context.Context, portfolio, currency string, after, through int64, limit int) (CashCommitPage, error) {
	if strings.TrimSpace(portfolio) == "" || len(portfolio) > 256 || strings.TrimSpace(currency) == "" || len(currency) > 256 ||
		after < 0 || through < 0 || (through != 0 && after > through) || limit < 1 || limit > 64 {
		return CashCommitPage{}, ErrCashReplayBounds
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return CashCommitPage{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var head int64
	if err := tx.QueryRow(ctx, `SELECT revision FROM cash_commit_heads WHERE portfolio_id=$1 AND currency=$2`, portfolio, currency).Scan(&head); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CashCommitPage{}, ErrNoCashHistory
		}
		return CashCommitPage{}, err
	}
	if head == 0 {
		return CashCommitPage{}, ErrNoCashHistory
	}
	if through == 0 {
		through = head
	}
	if through > head || after > through {
		return CashCommitPage{}, ErrCashReplayBounds
	}
	page := CashCommitPage{PortfolioID: portfolio, Currency: currency, Through: through, Next: after, Records: []CashCommitRecord{}}
	rows, err := tx.Query(ctx, `SELECT revision,payload FROM cash_commit_history
		WHERE portfolio_id=$1 AND currency=$2 AND revision>$3 AND revision<=$4
		ORDER BY revision LIMIT $5`, portfolio, currency, after, through, limit)
	if err != nil {
		return CashCommitPage{}, err
	}
	defer rows.Close()
	totalBytes, byteLimited := 0, false
	for rows.Next() {
		var record CashCommitRecord
		if err := rows.Scan(&record.Revision, &record.Payload); err != nil {
			return CashCommitPage{}, err
		}
		if record.Revision != page.Next+1 || len(record.Payload) == 0 || len(record.Payload) > maxCashCommitBytes {
			return CashCommitPage{}, ErrCashCommit
		}
		if totalBytes+len(record.Payload) > maxCashReplayBytes {
			byteLimited = true
			break
		}
		var msg accountingpb.PortfolioCashBalance
		if err := proto.Unmarshal(record.Payload, &msg); err != nil || msg.GetPortfolioId() != portfolio || msg.GetBaseCurrency() != currency || msg.GetCashCommit().GetRevision() != record.Revision {
			return CashCommitPage{}, ErrCashCommit
		}
		page.Records = append(page.Records, record)
		page.Next = record.Revision
		totalBytes += len(record.Payload)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return CashCommitPage{}, err
	}
	page.HasMore = page.Next < through
	if !byteLimited && len(page.Records) < limit && page.HasMore {
		return CashCommitPage{}, ErrCashCommit // retained prefix has a hole
	}
	if err := tx.Commit(ctx); err != nil {
		return CashCommitPage{}, err
	}
	return page, nil
}
