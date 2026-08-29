package consume

import (
	"context"
	"testing"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// THE TEST SEAM FOR THE FACT'S CONTENT (#804).
//
// The cash announcement used to be a publish the Folder made after the ledger
// write committed, and these tests drove it directly. It is now a RECORD the
// ledger commits with the entry, published by the outbox relay — so the tests
// that assert WHAT GOES ON THE WIRE build the record the same way a fold does
// and publish it here, rather than being rewritten around a relay whose job is
// delivery rather than content.
//
// The ones that care about the DELIVERY — that a failed publish is counted, that
// a record survives a broker outage — drive the real path instead: Append with
// the folder's announcer, then flush.

// foldCtx is the context a real delivery carries.
//
// outbox.From REFUSES A RECORD WITH NO TENANT, deliberately: bus.Validate
// rejects an empty tenant_id on the live path, so a record captured without one
// could never be published and would sit at the head of its key blocking every
// FACT behind it. bus.Consumer stashes the inbound envelope's tenant on ctx;
// these tests supply the same thing rather than reaching for a fallback that
// production does not have.
func foldCtx() context.Context {
	return bus.WithTenantID(context.Background(), "acme")
}

// announceVia builds portfolioID's records the way a fold does and publishes
// them straight away, so an assertion about the published message is unchanged
// by the outbox now carrying it.
func announceVia(t *testing.T, ctx context.Context, a *Announcer, st ledger.Store, portfolioID string) error {
	t.Helper()
	records, err := a.Records(ctx, st, portfolioID)
	if err != nil {
		return err
	}
	for _, r := range records {
		e, eerr := r.Event()
		if eerr != nil {
			return eerr
		}
		if perr := a.publisher.Publish(ctx, e); perr != nil {
			return perr
		}
	}
	return nil
}
