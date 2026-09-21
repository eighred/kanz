package order

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/eighred/kanz/internal/execution"
	"github.com/jackc/pgx/v5"
)

var ErrRecoveryEvidenceConflict = errors.New("oms: recovery identity has different evidence")

// RecoveryMapping is an immutable, proven collateral attribution. A tenant is
// not a portfolio, and an account label without an exchange proof is not a
// verified destination for an economic correction.
type RecoveryMapping struct {
	Tenant, Portfolio, Venue, Account, ExchangeAccount string
}

func (m RecoveryMapping) Version() (string, error) {
	for _, field := range []string{m.Tenant, m.Portfolio, m.Venue, m.Account, m.ExchangeAccount} {
		if !recoveryText(field, 256) {
			return "", errors.New("oms: incomplete recovery account mapping")
		}
	}
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return recoveryDigest(data), nil
}

func recoveryText(s string, max int) bool {
	return s != "" && len(s) <= max && utf8.ValidString(s) && strings.TrimSpace(s) == s && !strings.ContainsRune(s, '\x00')
}

func recoveryDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// RecordRecoveryMapping is called only after the configured binding and the
// adapter's exchange proof agree. The stored version, not a later environment
// value, is the basis of every case that references it.
func (p *Postgres) RecordRecoveryMapping(ctx context.Context, m RecoveryMapping, proof execution.AccountProof) (string, error) {
	if !proof.Verified || proof.ExchangeAccountID != m.ExchangeAccount {
		return "", errors.New("oms: recovery account is not proven by the venue")
	}
	version, err := m.Version()
	if err != nil {
		return "", err
	}
	tag, err := p.pool.Exec(ctx, `INSERT INTO execution_account_mappings
		(tenant_id, version, venue, venue_account_id, exchange_account_id, portfolio_id)
		VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (tenant_id, version) DO NOTHING`,
		m.Tenant, version, m.Venue, m.Account, m.ExchangeAccount, m.Portfolio)
	if err != nil {
		return "", fmt.Errorf("record recovery mapping: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		err = p.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM execution_account_mappings
			WHERE tenant_id=$1 AND version=$2 AND venue=$3 AND venue_account_id=$4
			AND exchange_account_id=$5 AND portfolio_id=$6)`,
			m.Tenant, version, m.Venue, m.Account, m.ExchangeAccount, m.Portfolio).Scan(&exists)
		if err != nil {
			return "", err
		}
		if !exists {
			return "", ErrRecoveryEvidenceConflict
		}
	}
	return version, nil
}

type RecoveryCase struct {
	ID, OrderID, MappingVersion, SourceCursor, Digest, Status, Reason string
	Evidence                                                          []byte
	Checkpoint                                                        int
	Version                                                           int64
}

// ObserveRecovery durably records the source before any query or financial
// write. Reusing a case identity with changed bytes is refused, never silently
// treated as redelivery. The bound limits both storage and decode work.
func (p *Postgres) ObserveRecovery(ctx context.Context, id, orderID, cursor string, evidence []byte) (RecoveryCase, error) {
	if !recoveryText(id, 256) || !recoveryText(orderID, 256) || !recoveryText(cursor, 1024) || len(evidence) == 0 || len(evidence) > 4<<20 {
		return RecoveryCase{}, errors.New("oms: invalid recovery evidence")
	}
	digest := recoveryDigest(evidence)
	_, err := p.pool.Exec(ctx, `INSERT INTO execution_recovery_cases
		(case_id, order_id, source_cursor, payload_digest, evidence, status)
		VALUES ($1,$2,$3,$4,$5,'observed') ON CONFLICT (tenant_id, case_id) DO NOTHING`,
		id, orderID, cursor, digest, evidence)
	if err != nil {
		return RecoveryCase{}, err
	}
	c, err := p.RecoveryCase(ctx, id)
	if err != nil {
		return RecoveryCase{}, err
	}
	if c.OrderID != orderID || c.SourceCursor != cursor || c.Digest != digest {
		return RecoveryCase{}, ErrRecoveryEvidenceConflict
	}
	return c, nil
}

func (p *Postgres) RecoveryCase(ctx context.Context, id string) (RecoveryCase, error) {
	return scanRecoveryCase(p.pool.QueryRow(ctx, recoveryCaseSQL, id))
}

const recoveryCaseSQL = `SELECT case_id, order_id, COALESCE(mapping_version,''),
		source_cursor, payload_digest, evidence, status, reason, checkpoint, version
		FROM execution_recovery_cases WHERE case_id=$1`

func scanRecoveryCase(row pgx.Row) (RecoveryCase, error) {
	var c RecoveryCase
	err := row.Scan(&c.ID, &c.OrderID,
		&c.MappingVersion, &c.SourceCursor, &c.Digest, &c.Evidence, &c.Status, &c.Reason, &c.Checkpoint, &c.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return RecoveryCase{}, ErrNotFound
	}
	return c, err
}
