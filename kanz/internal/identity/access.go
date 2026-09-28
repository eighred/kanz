package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
)

var ErrAccessConflict = errors.New("identity: account access changed; reload before editing")
var ErrInvalidAccess = errors.New("identity: invalid roles or portfolios")

// UserPageSize bounds database work and the administrative response size.
const UserPageSize = 25

// Access is the credential-free administrative projection. Activity is deliberately
// not inferred from updated_at: credential rehashes also update that timestamp.
type Access struct {
	Subject      string    `json:"subject"`
	Tenant       string    `json:"tenant"`
	Roles        []string  `json:"roles"`
	Portfolios   []string  `json:"portfolios"`
	Status       Status    `json:"status"`
	Revision     int64     `json:"revision"`
	SessionEpoch int64     `json:"session_epoch"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	CreatedBy    string    `json:"created_by"`
}

// ValidateAccess bounds the authority payload and rejects ambiguous entries.
func ValidateAccess(roles, portfolios []string) error {
	if len(roles) == 0 || len(roles) > 128 || len(portfolios) > 128 {
		return ErrInvalidAccess
	}
	for _, values := range [][]string{roles, portfolios} {
		seen := make(map[string]bool, len(values))
		for _, v := range values {
			if v == "" || len(v) > 256 || strings.TrimSpace(v) != v || strings.ContainsAny(v, "\r\n\x00") || seen[v] {
				return ErrInvalidAccess
			}
			seen[v] = true
		}
	}
	return ValidateAdminRoles(roles)
}

const accessColumns = `u.subject, u.tenant_id, u.roles, u.portfolios, u.status, u.access_revision, u.session_epoch, u.created_at, u.updated_at,
 COALESCE((SELECT i.created_by FROM identity_invites i WHERE i.subject=u.subject AND i.tenant_id=u.tenant_id AND i.redeemed_at IS NOT NULL ORDER BY i.redeemed_at LIMIT 1), '')`

func scanAccess(row pgx.Row) (*Access, error) {
	var a Access
	err := row.Scan(a.scanDestinations()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	return &a, err
}

// UsersFor uses a bounded keyset page. The actor is rechecked under the same
// lock as mutations, so a revoked administrator cannot read after waiting.
func (p *Postgres) UsersFor(ctx context.Context, actor Administration, after string) ([]*Access, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = lockAdministrator(ctx, tx, actor); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT `+accessColumns+` FROM identity_users u WHERE u.tenant_id=$1 AND u.subject>$2 ORDER BY u.subject LIMIT $3`, actor.Tenant, after, UserPageSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Access, 0)
	for rows.Next() {
		a, e := scanAccess(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, a)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return out, tx.Commit(ctx)
}

func (p *Postgres) SetAccess(ctx context.Context, actor Administration, subject string, revision int64, roles, portfolios []string, now time.Time) (*Access, error) {
	if err := ValidateAccess(roles, portfolios); err != nil {
		return nil, err
	}
	roles, portfolios = copyOf(roles), copyOf(portfolios)
	slices.Sort(roles)
	slices.Sort(portfolios)
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	actorUser, err := lockAdministrator(ctx, tx, actor)
	if err != nil {
		return nil, err
	}
	before, err := lockAccess(ctx, tx, subject, actor.Tenant)
	if err != nil {
		return nil, err
	}
	if revision != before.Revision {
		return nil, ErrAccessConflict
	}
	if before.Status == StatusActive && slices.Contains(before.Roles, AdminRole) && !slices.Contains(roles, AdminRole) {
		if err = preserveAdministrator(ctx, tx, actor.Tenant, subject); err != nil {
			return nil, err
		}
	}
	after, err := scanAccess(tx.QueryRow(ctx, `UPDATE identity_users u SET roles=$3, portfolios=$4, access_revision=access_revision+1,session_epoch=session_epoch+1,updated_at=$5,tokens_invalid_before=GREATEST(tokens_invalid_before,$5::timestamptz) WHERE subject=$1 AND tenant_id=$2 RETURNING `+accessColumns, subject, actor.Tenant, roles, portfolios, now.UTC()))
	if err != nil {
		return nil, err
	}
	if err = recordAccess(ctx, tx, actor, actorUser, "access", before, after, now); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return after, nil
}

func preserveAdministrator(ctx context.Context, tx pgx.Tx, tenant, subject string) error {
	var remaining int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM identity_users WHERE tenant_id=$1 AND subject<>$2 AND status='active' AND $3=ANY(roles) AND roles <@ ARRAY[$3,'kanz-user']::text[]`, tenant, subject, AdminRole).Scan(&remaining); err != nil {
		return err
	}
	if remaining == 0 {
		return ErrLastAdmin
	}
	return nil
}

func recordAccess(ctx context.Context, tx pgx.Tx, actor Administration, actorUser *User, action string, before, after *Access, now time.Time) error {
	reason := "current administrator authority verified under tenant lock"
	if action == "credential.rotate" {
		reason = "current session and verified credential snapshot checked under tenant lock"
	}
	entry := auth.BuildDecisionLog("operator:"+actor.Subject, auth.Request{Principal: &auth.Principal{Subject: actor.Subject, Tenant: actor.Tenant, Roles: actorUser.Roles, Portfolios: actorUser.Portfolios, SessionEpoch: actor.SessionEpoch, IssuedAt: actor.IssuedAt}, Action: auth.Action("identity.account." + action), Resource: auth.Resource{Type: "account", ID: after.Subject, Tenant: after.Tenant}}, auth.Decision{Allow: true, Reason: reason})
	oldJSON, err := json.Marshal(before)
	if err != nil {
		return err
	}
	newJSON, err := json.Marshal(after)
	if err != nil {
		return err
	}
	entry.Attributes["account.before"] = string(oldJSON)
	entry.Attributes["account.after"] = string(newJSON)
	entry.Attributes["occurred_at"] = now.UTC().Format(time.RFC3339Nano)
	return persistIdentityDecision(ctx, tx, after.Tenant, after.Subject, now, entry)
}

func persistIdentityDecision(ctx context.Context, tx pgx.Tx, tenant, subject string, now time.Time, entry *observationpb.DecisionLog) error {
	payload, err := protojson.Marshal(entry)
	if err != nil {
		return err
	}
	// Transaction-local scope cannot leak into the unscoped login pool.
	if _, err = tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO identity_access_audit(tenant_id,subject,occurred_at,decision) VALUES($1,$2,$3,$4::jsonb)`, tenant, subject, now.UTC(), string(payload))
	if err != nil {
		return fmt.Errorf("identity: persist access audit: %w", err)
	}
	return nil
}

func (a *Access) scanDestinations() []any {
	return []any{&a.Subject, &a.Tenant, &a.Roles, &a.Portfolios, &a.Status, &a.Revision, &a.SessionEpoch, &a.CreatedAt, &a.UpdatedAt, &a.CreatedBy}
}

// lockAccess keeps the lock and scan visibly coupled. Both mutation paths use
// the returned revision/status/roles before writing, with one column mapping.
func lockAccess(ctx context.Context, tx pgx.Tx, subject, tenant string) (*Access, error) {
	var a Access
	err := tx.QueryRow(ctx, `SELECT `+accessColumns+` FROM identity_users u WHERE u.subject=$1 AND u.tenant_id=$2 FOR UPDATE OF u`, subject, tenant).Scan(a.scanDestinations()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	return &a, err
}
