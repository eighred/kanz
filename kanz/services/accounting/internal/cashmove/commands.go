package cashmove

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/outbox"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

var (
	ErrCommandInput    = errors.New("cashmove: explicit exact command terms required")
	ErrCommandConflict = errors.New("cashmove: command identity or reviewed terms conflict")
	ErrCommandNotFound = errors.New("cashmove: command not found")
)

// Command records a human instruction, not proof that money has settled.
// Actor and Tenant come exclusively from the authenticated server context.
type Command struct {
	Tenant, Actor, Reason string
	Movement              CashMovement
}

type canonicalCommand struct {
	Version, Tenant, Actor, Reason          string
	MovementID, PortfolioID, VenueAccountID string
	Kind                                    Kind
	Amount                                  dec.Exact
	Currency                                string
	Effective                               time.Time
	SourceRef                               string
}

type Receipt struct {
	MovementID string    `json:"movement_id"`
	Digest     string    `json:"command_digest"`
	Actor      string    `json:"actor"`
	AcceptedAt time.Time `json:"accepted_at"`
	Status     string    `json:"status"`
}

func commandText(s string, limit int) bool {
	return s != "" && len(s) <= limit && utf8.ValidString(s) && strings.TrimSpace(s) == s && !strings.ContainsRune(s, '\x00')
}

func canonicalize(c Command) ([]byte, string, error) {
	m := c.Movement
	if !commandText(c.Tenant, 256) || !commandText(c.Actor, 256) || !commandText(c.Reason, 4096) ||
		!commandText(m.MovementID, 256) || !commandText(m.PortfolioID, 256) || !commandText(m.SourceRef, 1024) ||
		(m.VenueAccountID != "" && !commandText(m.VenueAccountID, 256)) || m.Effective.IsZero() || m.Effective.UTC().Year() < 1 || m.Effective.UTC().Year() > 9999 || m.Effective.Nanosecond()%1000 != 0 || (len(m.Currency) < 2 || len(m.Currency) > 16) {
		return nil, "", ErrCommandInput
	}
	for _, ch := range m.Currency {
		if (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') {
			return nil, "", ErrCommandInput
		}
	}
	if _, _, ok := m.Kind.wire(); !ok || m.Amount == nil || m.Amount.Sign() <= 0 {
		return nil, "", ErrCommandInput
	}
	if _, ok := dec.ToProtoExact(m.Amount); !ok {
		return nil, "", ErrCommandInput
	}
	amount, err := dec.ExactFromRat(m.Amount)
	if err != nil {
		return nil, "", ErrCommandInput
	}
	blob, err := json.Marshal(canonicalCommand{"cash-command-v1", c.Tenant, c.Actor, c.Reason, m.MovementID, m.PortfolioID, m.VenueAccountID, m.Kind, amount, m.Currency, m.Effective.UTC(), m.SourceRef})
	if err != nil {
		return nil, "", err
	}
	h := sha256.Sum256(blob)
	return blob, hex.EncodeToString(h[:]), nil
}

// ReviewDigest binds the exact terms and reviewing actor. It is not a second
// approver; the existing Fund capability remains the authorization policy.
func ReviewDigest(c Command) (string, error) { _, d, e := canonicalize(c); return d, e }

type Commands struct{ pool *pgxpool.Pool }

func NewCommands(pool *pgxpool.Pool) *Commands { return &Commands{pool: pool} }

func (s *Commands) Accept(ctx context.Context, c Command, reviewed string) (Receipt, error) {
	body, digest, err := canonicalize(c)
	if err != nil {
		return Receipt{}, err
	}
	if reviewed != digest {
		return Receipt{}, ErrCommandConflict
	}
	// Encode from the immutable reviewed bytes, not a caller-owned Rat that
	// could change while the transaction waits for the command lock.
	var frozen canonicalCommand
	if err = json.Unmarshal(body, &frozen); err != nil {
		return Receipt{}, err
	}
	c.Movement.Amount, err = frozen.Amount.Rat()
	if err != nil {
		return Receipt{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Receipt{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var tenant string
	if err = tx.QueryRow(ctx, `SELECT app_current_tenant()`).Scan(&tenant); err != nil {
		return Receipt{}, err
	}
	if tenant != c.Tenant {
		return Receipt{}, ErrCommandNotFound
	}
	// Shared with the journal append path: acceptance cannot race a legacy FACT
	// into claiming the same entry ID for different money or a different actor.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(app_current_tenant()||'/cash-entry/'||$1,0))`, "cash:"+c.Movement.MovementID); err != nil {
		return Receipt{}, err
	}
	var previous string
	var saved []byte
	err = tx.QueryRow(ctx, `SELECT digest,receipt FROM cash_commands WHERE movement_id=$1`, c.Movement.MovementID).Scan(&previous, &saved)
	if err == nil {
		if previous != digest {
			return Receipt{}, ErrCommandConflict
		}
		var r Receipt
		if err = json.Unmarshal(saved, &r); err != nil {
			return Receipt{}, err
		}
		return r, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Receipt{}, err
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ledger_entries WHERE entry_id=$1)`, "cash:"+c.Movement.MovementID).Scan(&exists); err != nil {
		return Receipt{}, err
	}
	if exists {
		return Receipt{}, ErrCommandConflict
	}
	// Match the journal's per-portfolio commit order: both producers enqueue
	// onto the same outbox partition. Entry identity is always locked first.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext(app_current_tenant()),hashtext($1))`, c.Movement.PortfolioID); err != nil {
		return Receipt{}, err
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	entry, subject, err := encode(c.Movement, now)
	if err != nil {
		return Receipt{}, err
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(entry)
	if err != nil {
		return Receipt{}, err
	}
	r := Receipt{c.Movement.MovementID, digest, c.Actor, now, "accepted"}
	receipt, err := json.Marshal(r)
	if err != nil {
		return Receipt{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO cash_commands(movement_id,portfolio_id,digest,actor,command,receipt,payload) VALUES($1,$2,$3,$4,$5,$6,$7)`, c.Movement.MovementID, c.Movement.PortfolioID, digest, c.Actor, body, receipt, payload); err != nil {
		return Receipt{}, err
	}
	fact, err := outbox.From(ctx, bus.Event{Subject: subject, EventType: subject, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: schemaVersionCash, Domain: domainAccounting, EventTime: c.Movement.Effective, TenantID: c.Tenant, PartitionKey: c.Movement.PortfolioID, CorrelationID: digest, PayloadSchemaRef: schemaRefLedger, Payload: entry})
	if err != nil {
		return Receipt{}, err
	}
	if err = outbox.Enqueue(ctx, tx, fact); err != nil {
		return Receipt{}, err
	}
	return r, tx.Commit(ctx)
}

// Status distinguishes durable acceptance from journal folding. Neither state
// asserts custody settlement. POST retries always return the original receipt.
func (s *Commands) Status(ctx context.Context, tenant, portfolio, id string) (Receipt, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var blob []byte
	var recorded bool
	err := s.pool.QueryRow(ctx, `SELECT receipt,EXISTS(SELECT 1 FROM ledger_entries WHERE entry_id='cash:'||c.movement_id) FROM cash_commands c WHERE app_current_tenant()=$1 AND portfolio_id=$2 AND movement_id=$3`, tenant, portfolio, id).Scan(&blob, &recorded)
	if errors.Is(err, pgx.ErrNoRows) {
		return Receipt{}, ErrCommandNotFound
	}
	if err != nil {
		return Receipt{}, err
	}
	var r Receipt
	if err = json.Unmarshal(blob, &r); err != nil {
		return Receipt{}, err
	}
	if recorded {
		r.Status = "journal_recorded"
	}
	return r, nil
}
