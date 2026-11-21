package custody

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/jackc/pgx/v5"
)

var ErrUnprovenAssignment = errors.New("custody: independently proven effective account assignment unavailable")

// AccountAssignment is the exact authority snapshot behind a declared book fold.
// Windows are UTC business dates, inclusive from and exclusive until. Recorded
// times are PostgreSQL knowledge times, independent of the issuers' effective dates.
type AccountAssignment struct {
	TenantID            string `json:"tenant_id"`
	PortfolioID         string `json:"portfolio_id"`
	CustodianID         string `json:"custodian_id"`
	AccountID           string `json:"account_id"`
	Venue               string `json:"venue"`
	ExchangeAccountID   string `json:"exchange_account_id"`
	ProofID             string `json:"proof_id"`
	ProofAuthority      string `json:"proof_authority"`
	ProofDigest         string `json:"proof_sha256"`
	ProofFrom           string `json:"proof_from"`
	ProofUntil          string `json:"proof_until"`
	ProofRecordedAt     string `json:"proof_recorded_at"`
	Version             string `json:"version"`
	Revision            int64  `json:"revision"`
	AssignmentAuthority string `json:"assignment_authority"`
	AssignmentDigest    string `json:"assignment_sha256"`
	EffectiveFrom       string `json:"effective_from"`
	EffectiveUntil      string `json:"effective_until"`
	RecordedAt          string `json:"recorded_at"`
}

func (p *Postgres) ProvenAssignments(ctx context.Context, subject Subject, accounts []string) ([]AccountAssignment, error) {
	return p.proveDeclared(ctx, subject, map[string][]string{subject.CustodianID: accounts}, true)
}

func (p *Postgres) ProvenPortfolioAssignments(ctx context.Context, subject Subject, accounts map[string][]string) ([]AccountAssignment, error) {
	return p.proveDeclared(ctx, subject, accounts, false)
}

func (p *Postgres) proveDeclared(ctx context.Context, subject Subject, accounts map[string][]string, pairOnly bool) ([]AccountAssignment, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return assignmentSnapshot(ctx, tx, subject, accounts, nil, pairOnly)
}

// A new revision supersedes earlier versions from its effective start. An
// expired successor cannot resurrect an earlier assignment. Saved receipts
// instead select their exact immutable versions, including after a correction.
func assignmentSnapshot(ctx context.Context, tx pgx.Tx, subject Subject, accounts map[string][]string, versions []string, pairOnly bool) ([]AccountAssignment, error) {
	if subject.Validate() != nil || len(accounts) == 0 {
		return nil, ErrUnprovenAssignment
	}
	expected := make(map[string]string)
	for custodian, ids := range accounts {
		if custodian == "" || len(ids) == 0 {
			return nil, ErrUnprovenAssignment
		}
		for _, id := range ids {
			if id == "" || expected[id] != "" {
				return nil, ErrUnprovenAssignment
			}
			expected[id] = custodian
		}
	}
	var tenant string
	if err := tx.QueryRow(ctx, "SELECT app_current_tenant()").Scan(&tenant); err != nil {
		return nil, err
	}
	if tenant == "" || (bus.TenantIDFromContext(ctx) != "" && bus.TenantIDFromContext(ctx) != tenant) {
		return nil, ErrUnprovenAssignment
	}
	const q = `WITH candidates AS (
   SELECT DISTINCT ON (a.account_id) a.* FROM custody_account_assignments a
   WHERE a.tenant_id=$1 AND a.tenant_id=app_current_tenant() AND a.effective_from <= $2::date
   AND ($3::text[] IS NULL OR a.version=ANY($3))
   AND EXISTS(SELECT FROM custody_assignment_readers r WHERE r.login_role=session_user AND r.tenant_id=a.tenant_id)
   ORDER BY a.account_id,a.revision DESC
 ), active AS (
  SELECT c.*,p.venue,p.exchange_account_id,count(*) FILTER(WHERE NOT c.withdrawn AND c.effective_until>$2::date) OVER(PARTITION BY p.venue,p.exchange_account_id) aliases
  FROM candidates c JOIN custody_account_proofs p USING(tenant_id,proof_id,account_id)
 ) SELECT a.portfolio_id,a.custodian_id,a.account_id,a.version,a.revision,a.authority,a.effective_from,a.effective_until,a.recorded_at,
 a.evidence_sha256,a.evidence_payload,a.signature,ai.public_key,
 p.venue,p.exchange_account_id,p.proof_id,p.authority,p.effective_from,p.effective_until,p.recorded_at,
 p.evidence_sha256,p.evidence_payload,p.signature,pi.public_key,a.aliases,a.withdrawn,
 EXISTS(SELECT FROM custody_proof_scopes s WHERE (s.login_role,s.tenant_id,s.venue,s.account_id,s.exchange_account_id,s.portfolio_id,s.custodian_id)=
    (p.issuer_role,p.tenant_id,p.venue,p.account_id,p.exchange_account_id,'','')),
 EXISTS(SELECT FROM custody_proof_scopes s WHERE (s.login_role,s.tenant_id,s.venue,s.account_id,s.exchange_account_id,s.portfolio_id,s.custodian_id)=
    (a.issuer_role,a.tenant_id,'',a.account_id,'',a.portfolio_id,a.custodian_id))
 FROM active a JOIN custody_account_proofs p USING(tenant_id,account_id,proof_id)
 LEFT JOIN custody_proof_issuers ai ON(ai.login_role=a.issuer_role AND ai.tenant_id=a.tenant_id AND ai.authority=a.authority AND ai.kind='custody_assignment')
 LEFT JOIN custody_proof_issuers pi ON(pi.login_role=p.issuer_role AND pi.tenant_id=p.tenant_id AND pi.authority=p.authority AND pi.kind='venue_identity')
 WHERE EXISTS(SELECT FROM custody_account_assignments seen WHERE seen.tenant_id=a.tenant_id AND seen.account_id=a.account_id
   AND seen.portfolio_id=$4 AND (NOT $5::boolean OR seen.custodian_id=$6) AND seen.effective_from<=$2::date)
 ORDER BY a.custodian_id COLLATE "C",a.account_id COLLATE "C"`
	rows, err := tx.Query(ctx, q, tenant, BusinessDay(subject.BusinessDate), versions, subject.PortfolioID, pairOnly, subject.CustodianID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []AccountAssignment
	for rows.Next() {
		a := AccountAssignment{TenantID: tenant}
		var af, au, ar, pf, pu, pr time.Time
		var ad, ab, as, ak, pd, pb, ps, pk []byte
		var aliases int64
		var withdrawn, identityScope, assignmentScope bool
		if err := rows.Scan(&a.PortfolioID, &a.CustodianID, &a.AccountID, &a.Version, &a.Revision, &a.AssignmentAuthority, &af, &au, &ar, &ad, &ab, &as, &ak,
			&a.Venue, &a.ExchangeAccountID, &a.ProofID, &a.ProofAuthority, &pf, &pu, &pr, &pd, &pb, &ps, &pk, &aliases, &withdrawn, &identityScope, &assignmentScope); err != nil {
			return nil, err
		}
		if !identityScope || !assignmentScope {
			return nil, ErrUnprovenAssignment
		}
		a.EffectiveFrom, a.EffectiveUntil, a.ProofFrom, a.ProofUntil = af.Format(time.DateOnly), au.Format(time.DateOnly), pf.Format(time.DateOnly), pu.Format(time.DateOnly)
		a.AssignmentDigest, a.ProofDigest = hex.EncodeToString(ad), hex.EncodeToString(pd)
		a.RecordedAt, a.ProofRecordedAt = ar.UTC().Format(time.RFC3339Nano), pr.UTC().Format(time.RFC3339Nano)
		identity := AccountEvidence{Kind: "venue_identity", TenantID: tenant, Authority: a.ProofAuthority, Venue: a.Venue, AccountID: a.AccountID, ExchangeAccountID: a.ExchangeAccountID, ProofID: a.ProofID, EffectiveFrom: a.ProofFrom, EffectiveUntil: a.ProofUntil}
		assignment := AccountEvidence{Withdrawn: withdrawn, Kind: "custody_assignment", TenantID: tenant, Authority: a.AssignmentAuthority, PortfolioID: a.PortfolioID, CustodianID: a.CustodianID, AccountID: a.AccountID, ProofID: a.ProofID, Version: a.Version, Revision: a.Revision, EffectiveFrom: a.EffectiveFrom, EffectiveUntil: a.EffectiveUntil}
		if !VerifyAccountEvidence(identity, pb, ps, pk) || !VerifyAccountEvidence(assignment, ab, as, ak) {
			return nil, ErrUnprovenAssignment
		}
		if withdrawn || !BusinessDay(subject.BusinessDate).Before(au) || a.PortfolioID != subject.PortfolioID || (pairOnly && a.CustodianID != subject.CustodianID) {
			continue
		}
		// Identity may be attested for a shorter window than the governing approval.
		// Only their intersection can support the selected accounting date.
		if !coveredAssignmentDay(subject.BusinessDate, pf, pu, af, au) || expected[a.AccountID] != a.CustodianID || aliases != 1 {
			return nil, ErrUnprovenAssignment
		}
		result = append(result, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(result) != len(expected) {
		return nil, ErrUnprovenAssignment
	}
	return result, nil
}

func confirmRunAssignments(ctx context.Context, tx pgx.Tx, subject Subject, b *RunBasis, historical bool) (bool, error) {
	if b == nil || b.Assignments == nil {
		return false, nil
	}
	if b.Mode != BasisDeclared || len(b.Assignments) == 0 {
		return false, ErrUnprovenAssignment
	}
	var versions []string
	if historical {
		for _, a := range b.Assignments {
			versions = append(versions, a.Version)
		}
	}
	actual, err := assignmentSnapshot(ctx, tx, subject, b.Accounts, versions, false)
	if err != nil {
		return false, err
	}
	if !slices.Equal(actual, b.Assignments) {
		return false, ErrUnprovenAssignment
	}
	return true, nil
}

func assignmentsForCustodian(all []AccountAssignment, custodian string) []AccountAssignment {
	var own []AccountAssignment
	for _, a := range all {
		if a.CustodianID == custodian {
			own = append(own, a)
		}
	}
	return own
}

func coveredAssignmentDay(day, identityFrom, identityUntil, approvalFrom, approvalUntil time.Time) bool {
	day = BusinessDay(day)
	return !day.Before(identityFrom) && day.Before(identityUntil) && !day.Before(approvalFrom) && day.Before(approvalUntil)
}
