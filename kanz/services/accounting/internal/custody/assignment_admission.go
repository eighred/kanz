package custody

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"
)

// AssignmentReader resolves the declaration against independent tenant-scoped
// venue identities and effective custody approvals in a single snapshot.
type AssignmentReader interface {
	ProvenPortfolioAssignments(context.Context, Subject, map[string][]string) ([]AccountAssignment, error)
}

var _ AssignmentReader = (*Postgres)(nil)

type assignmentAdmission struct{ reader AssignmentReader }

func (s *BookScope) declaredAccounts(portfolio string) map[string][]string {
	accounts := make(map[string][]string, len(s.accounts[portfolio]))
	for custodian, scope := range s.accounts[portfolio] {
		accounts[custodian] = slices.Sorted(maps.Keys(scope))
	}
	return accounts
}

// AdmitAssignments returns the scope used by a running accounting instance.
// A deployment without declared accounts needs no proof store. The caller must
// use the returned scope: the boot snapshot never stands in for a dated check.
func (s *BookScope) AdmitAssignments(ctx context.Context, at time.Time, reader AssignmentReader) (*BookScope, error) {
	admitted := *s
	admitted.admission = &assignmentAdmission{reader: reader}
	if len(s.accounts) == 0 {
		return &admitted, nil
	}
	if reader == nil || at.IsZero() {
		return nil, ErrUnprovenAssignment
	}
	for _, portfolio := range slices.Sorted(maps.Keys(s.accounts)) {
		subject := Subject{PortfolioID: portfolio, CustodianID: s.custodians[portfolio][0], BusinessDate: BusinessDay(at)}
		if _, err := admitted.assignmentsAt(ctx, subject); err != nil {
			return nil, err
		}
	}
	return &admitted, nil
}

func (s *BookScope) assignmentsAt(ctx context.Context, subject Subject) ([]AccountAssignment, error) {
	if s.admission == nil {
		return nil, nil
	}
	accounts := s.declaredAccounts(subject.PortfolioID)
	rows, err := s.admission.reader.ProvenPortfolioAssignments(ctx, subject, accounts)
	if err != nil {
		// Keep storage details out of completion facts and ad-hoc HTTP responses.
		return nil, fmt.Errorf("%w: %s:%s on %s", ErrUnprovenAssignment, subject.PortfolioID, subject.CustodianID, subject.BusinessDate.Format(time.DateOnly))
	}
	if len(rows) != len(s.claimed[subject.PortfolioID]) {
		return nil, ErrUnprovenAssignment
	}
	return rows, nil
}
