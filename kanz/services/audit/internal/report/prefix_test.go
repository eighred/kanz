package report

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/audit/chain"
	"github.com/eighred/kanz/services/audit/internal/audit"
)

type appendAfterScan struct {
	audit.Store
	append func() error
}

func (s appendAfterScan) Head(context.Context) (audit.Head, error) {
	return audit.Head{}, errors.New("head must come from the scanned prefix")
}
func (s appendAfterScan) Scan(ctx context.Context, yield func(*audit.Record) error) error {
	if err := s.Store.Scan(ctx, yield); err != nil {
		return err
	}
	return s.append()
}

func checkPrefix(t *testing.T, store audit.Store, initiallyEmpty bool) {
	t.Helper()
	ctx := context.Background()
	wantHash, wantSeq, wantCount := chain.Genesis, int64(0), 0
	if !initiallyEmpty {
		r, err := store.Append(ctx, &audit.Record{EventID: "before", TenantID: "test-tenant", Kind: audit.KindEvent})
		if err != nil {
			t.Fatal(err)
		}
		wantHash, wantSeq, wantCount = r.Hash(), r.Seq, 1
	}
	wrapped := appendAfterScan{Store: store, append: func() error {
		_, err := store.Append(ctx, &audit.Record{EventID: "after", TenantID: "test-tenant", Kind: audit.KindEvent})
		return err
	}}
	r, err := Generate(ctx, wrapped, Template{Name: "test", Filter: audit.Filter{Tenant: "test-tenant", Limit: 10}}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Integrity.Verified || r.Integrity.Head != wantHash || r.Integrity.HeadSeq != wantSeq || r.Integrity.Records != wantCount || r.Count != wantCount || !r.Complete {
		t.Fatalf("mixed prefixes: %+v", r)
	}
	for _, row := range r.Records {
		if row.Seq > r.Integrity.HeadSeq {
			t.Fatal("unverified row attached")
		}
	}
}

func TestReportUsesScannedPrefixEvenWhenAnAppendArrives(t *testing.T) {
	for _, empty := range []bool{true, false} {
		checkPrefix(t, audit.NewMemory(), empty)
	}
}
