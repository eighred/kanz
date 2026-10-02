package ledger

import (
	"context"
	"errors"
	"math"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/eighred/kanz/internal/dec"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

var ErrCashCommit = errors.New("ledger: cash commit requires an exact, ordered, transaction-bound projection")

// Bound a retained fact before committing it to the outbox. A large catch-up
// batch must be reconciled explicitly, never truncated or left unpublishable.
const maxCashCommitBytes = 512 << 10

// SealCashBalance attaches durable debit-inclusion evidence to the balance.
// Only an Append transaction may seal: its portfolio lock orders commits, and
// its rollback includes the source sequence, covered debits, history and outbox.
// Non-durable stores have no sealer and must leave cash_commit absent.
func (p *Postgres) SealCashBalance(ctx context.Context, msg *accountingpb.PortfolioCashBalance, projection CashProjection) error {
	tx, ok := p.q.(pgx.Tx)
	if !ok || msg == nil || projection.Book == nil || projection.asOf.IsZero() ||
		msg.GetPortfolioId() != projection.Book.PortfolioID || strings.TrimSpace(msg.GetPortfolioId()) == "" ||
		len(msg.GetPortfolioId()) > 256 || strings.TrimSpace(msg.GetBaseCurrency()) == "" || len(msg.GetBaseCurrency()) > 256 ||
		msg.GetAsOf() == nil || msg.GetAsOf().CheckValid() != nil || !msg.GetAsOf().AsTime().Equal(projection.asOf) ||
		msg.GetKnowledgeTime() == nil || msg.GetKnowledgeTime().CheckValid() != nil || !msg.GetKnowledgeTime().AsTime().Equal(projection.asOf) || msg.CashCommit != nil ||
		!time.Unix(0, projection.asOf.UnixNano()).Equal(projection.asOf) || msg.GetTotal() == nil {
		return ErrCashCommit
	}
	if proto.Size(msg) > maxCashCommitBytes {
		return ErrCashCommit
	}
	if _, ok := dec.InDomainDeep(msg); !ok || dec.FromProto(msg.Total).Cmp(projection.Book.CashBalance(msg.BaseCurrency)) != 0 {
		return ErrCashCommit
	}
	portfolio, currency := msg.PortfolioId, msg.BaseCurrency
	if _, err := tx.Exec(ctx, `INSERT INTO cash_commit_heads(portfolio_id,currency) VALUES($1,$2) ON CONFLICT DO NOTHING`, portfolio, currency); err != nil {
		return err
	}
	var revision int64
	var observed *int64
	if err := tx.QueryRow(ctx, `SELECT revision,observed_at_ns FROM cash_commit_heads WHERE portfolio_id=$1 AND currency=$2 FOR UPDATE`, portfolio, currency).Scan(&revision, &observed); err != nil {
		return err
	}
	if revision == math.MaxInt64 || (observed != nil && projection.asOf.UnixNano() < *observed) {
		return ErrCashCommit
	}
	debits, unknown := executionDebits(projection.events, currency)
	coverage := &accountingpb.CashCommitCoverage{Revision: revision + 1, Complete: unknown == 0, UnattributedEntries: unknown}
	if coverage.Complete {
		changes, err := changedCashDebits(ctx, tx, portfolio, currency, debits)
		if err != nil {
			return err
		}
		coverage.Applied = changes
		for _, change := range changes {
			if _, err := tx.Exec(ctx, `INSERT INTO cash_commit_debits(portfolio_id,currency,order_id,debit) VALUES($1,$2,$3,$4)
				ON CONFLICT(tenant_id,portfolio_id,currency,order_id) DO UPDATE SET debit=EXCLUDED.debit`, portfolio, currency, change.OrderId, dec.FromProto(change.Debit).RatString()); err != nil {
				return err
			}
		}
	}
	// The retained message is exactly the one published, including completeness
	// and currency coverage. Replay cannot upgrade a previously incomplete fact.
	msg.CashCommit = coverage
	if proto.Size(msg) > maxCashCommitBytes {
		return ErrCashCommit
	}
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(msg)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO cash_commit_history(portfolio_id,currency,revision,payload) VALUES($1,$2,$3,$4)`, portfolio, currency, coverage.Revision, body); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE cash_commit_heads SET revision=$3,observed_at_ns=$4 WHERE portfolio_id=$1 AND currency=$2`, portfolio, currency, coverage.Revision, projection.asOf.UnixNano())
	return err
}

func changedCashDebits(ctx context.Context, tx pgx.Tx, portfolio, currency string, current map[string]*big.Rat) ([]*accountingpb.OrderCashDebit, error) {
	previous := map[string]*big.Rat{}
	rows, err := tx.Query(ctx, `SELECT order_id,debit FROM cash_commit_debits WHERE portfolio_id=$1 AND currency=$2`, portfolio, currency)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, text string
		if err := rows.Scan(&id, &text); err != nil {
			return nil, err
		}
		amount, err := dec.Exact(text).Rat()
		if err != nil || amount.Sign() < 0 {
			return nil, ErrCashCommit
		}
		previous[id] = amount
		if current[id] == nil {
			current[id] = new(big.Rat) // explicit coverage reversal
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(current))
	for id := range current {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var changes []*accountingpb.OrderCashDebit
	for _, id := range ids {
		amount := current[id]
		if strings.TrimSpace(id) == "" || len(id) > 256 || amount.Sign() < 0 {
			return nil, ErrCashCommit
		}
		if old := previous[id]; old != nil && old.Cmp(amount) == 0 {
			continue
		}
		exact, ok := dec.ToProtoExact(amount)
		if !ok || len(changes) >= 4096 {
			return nil, ErrCashCommit // never truncate or round a proof batch
		}
		changes = append(changes, &accountingpb.OrderCashDebit{OrderId: id, Debit: exact})
	}
	return changes, nil
}
