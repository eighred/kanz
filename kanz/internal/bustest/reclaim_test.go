package bustest

import (
	"context"
	"errors"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
)

func TestReclaimRejectsNonliteralOrUnboundedSubjects(t *testing.T) {
	for _, subjects := range [][]string{nil, {""}, {"risk.position.>"}, {"risk.position.changed.*.p.i"},
		{"risk.position.changed.t..i"}, {"risk.position.changed.t.p.i "}, {"order.order.submit"},
		{"risk.position.changed.t.p.i\x00"}, {"risk.position.changed.t.p.i\x7f"},
		make([]string, 257)} {
		if err := reclaimSubjectsValid(subjects); err == nil {
			t.Errorf("accepted unsafe reclaim list %q", subjects)
		}
	}
	for _, subj := range []string{"risk.position.changed.t.p.i", "risk.position.venue.changed.t.p.v.i"} {
		if err := reclaimSubjectsValid([]string{subj}); err != nil {
			t.Fatal(err)
		}
	}
}

type refusingPurger struct {
	calls int
	err   error
}

func (p *refusingPurger) Purge(context.Context, ...jetstream.StreamPurgeOpt) error {
	p.calls++
	return p.err
}

func TestReclaimReportsFailureAndStillAttemptsEveryOwnedSubject(t *testing.T) {
	failure := errors.New("fixture purge refused")
	first, second := &refusingPurger{err: failure}, &refusingPurger{}
	err := purgePositionSubjects(context.Background(), map[string]positionPurger{
		"risk.position.changed.t.p.first": first, "risk.position.changed.t.p.second": second,
	})
	if !errors.Is(err, failure) || first.calls != 1 || second.calls != 1 {
		t.Fatalf("cleanup lost failure or abandoned another subject: %v, calls=%d/%d", err, first.calls, second.calls)
	}
}
