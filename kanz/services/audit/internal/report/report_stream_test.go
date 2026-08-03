package report

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/audit/chain"
	"github.com/eighred/kanz/services/audit/internal/audit"
)

// VERIFY MUST NOT MATERIALISE THE LOG (#229).
//
// GET /v1/audit/verify used to call store.All(ctx) — the entire compliance log
// into a slice, plus a parallel slice of chain.Link over it — with no bound and
// no LIMIT, on the endpoint a regulator's request hits.
//
// This proves the fold is genuinely incremental by verifying against a store
// that HAS NO BACKING SLICE and REUSES ONE RECORD BUFFER, which Store.Scan's
// contract explicitly permits ("the record is only valid for the duration of
// the call").
//
// The reuse is the assertion, and it is why this test cannot be satisfied by
// accident. A Verify that folds each record as it arrives is correct against a
// reused buffer. A Verify that ACCUMULATES ends up holding n aliases of the
// same, final record, so the chain it then walks does not verify — the failure
// is deterministic, immediate, and needs no heap measurement or timing. Against
// the pre-#229 code this store could not have been written at all: All had to
// return the whole log.
type streamStore struct {
	audit.Store
	n         int
	scanCalls int
	buf       audit.Record // the single reused record
}

func (s *streamStore) Scan(_ context.Context, yield func(*audit.Record) error) error {
	s.scanCalls++
	prev := chain.Genesis
	for i := range s.n {
		s.buf = audit.Record{
			EventID:    "e" + strconv.Itoa(i),
			Summary:    "s" + strconv.Itoa(i),
			RecordedAt: time.Unix(int64(1000+i), 0),
			PrevHashV:  prev,
		}
		s.buf.HashV = chain.Next(prev, s.buf.Canonical())
		prev = s.buf.HashV
		if err := yield(&s.buf); err != nil {
			return err
		}
	}
	return nil
}

func (s *streamStore) Head(context.Context) (audit.Head, error) {
	return audit.Head{Seq: int64(s.n), Hash: "head"}, nil
}

func TestVerifyStreamsRatherThanLoadingTheWholeLog(t *testing.T) {
	st := &streamStore{n: 5000}
	att, err := Verify(context.Background(), st)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !att.Verified {
		t.Fatalf("attestation not verified: %+v", att)
	}
	if att.Records != 5000 {
		t.Fatalf("Records = %d, want 5000 — the whole chain must still be walked; only the "+
			"working set is bounded", att.Records)
	}
	if st.scanCalls != 1 {
		t.Fatalf("Scan called %d times, want exactly 1", st.scanCalls)
	}
	// The Verified assertion above IS the memory assertion: it can only hold if
	// every record was folded before the next overwrote the buffer. A Verify
	// that collected the log first would have walked 5000 aliases of the last
	// record and failed the chain at index 0.
}

// A break must still be caught, at the right index, when streaming — and it
// must not truncate the count.
func TestVerifyStreamingCatchesATamper(t *testing.T) {
	st := seedStore(t)
	var all []*audit.Record
	if err := st.Scan(context.Background(), func(r *audit.Record) error {
		all = append(all, r)
		return nil
	}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	all[1].Summary = "FORGED"

	att, err := Verify(context.Background(), st)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if att.Verified {
		t.Fatal("a forged record passed streaming verification")
	}
	if !strings.Contains(att.Detail, "index 1") {
		t.Fatalf("detail = %q, want the first break located at index 1", att.Detail)
	}
	// Records is the LOG's length, not the length of the intact prefix:
	// reporting a broken chain as a short one understates what exists, and a
	// verifier that stopped at the first break would do exactly that.
	if att.Records != 3 {
		t.Fatalf("Records = %d, want 3 — a break must not truncate the count", att.Records)
	}
}
