package projection

// THE FOLD'S OWN STATE, AT A KNOWN POINT IN ITS OWN INPUT (#809).
//
// EXEC-M21 made tv-sync replay every FACT it had ever folded on boot, because a
// pod roll used to leave a trader looking at an empty account while real positions
// sat open at the exchanges. That fix is correct and this does not weaken it: what
// it left unbounded is the COST — N facts unmarshalled and folded before the pod
// can report ready, so startup grows with the fund's entire history and the outage
// after each OOM kill is longer than the one before it.
//
// #809 proposed bounding the replay to a recent window. That would re-create
// EXEC-M21's defect with a shorter horizon: a windowed rebuild loses every position
// opened before the window and reports realized P&L since the window start rather
// than since inception. On the surface a trader acts from, that is a wrong number
// rather than a saving.
//
// A checkpoint bounds the boot without losing anything, because
//
//	restore(checkpoint) + fold(facts after it)  ==  fold(all facts)
//
// and that equality is a property a test can hold rather than a claim. One does:
// TestACheckpointedBootReachesTheSameViewAsAFullReplay folds a history twice — once
// straight through, once through a checkpoint and a tail — and compares the served
// DTOs.
//
// # What is NOT here, and why
//
// NO PRUNING. Every exec, every order revision and every seen fill is carried
// across. The in-memory history is what serves the bitemporal reads, so dropping
// any of it is the read-path decision #809 describes as a hot window with
// log-backed as-of reads — a different piece of work with a different failure mode.
// This one is only about not re-deriving what we already derived.
//
// NO TRUNCATION OF seen_fills IN PARTICULAR. It looks like the cheapest thing to
// drop and it is the most dangerous: it is the dedup for the DUAL fill path (the
// synchronous venue response and the asynchronous websocket echo of the same
// fill), so a fill id missing when its echo arrives is a fill folded twice, which
// doubles a position the fund does not hold. The bus-redelivery path is protected
// separately by tv_facts's (tenant_id, event_id) primary key; this set is not that.

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	tvsyncpb "github.com/eighred/kanz/kanz-schemas-go/tvsync/v1"

	"github.com/eighred/kanz/internal/dec"
)

// Checkpoint records the fold's current state so the next boot can resume from it
// rather than re-deriving it.
//
// IT IS A NO-OP WITHOUT A LOG. A memory-only projection has nothing to resume
// from and nothing to write to; returning an error would make the checkpoint
// ticker a source of noise in every test and in the memory-only posture the
// service legitimately runs in.
//
// IT TAKES THE READ LOCK, not the write lock. Serializing does not mutate the
// view, and a checkpoint is not worth pausing the fold for: it runs on a ticker
// against a book that is being written to continuously, and the seq it records is
// the one that matches the state it read.
func (p *Projection) Checkpoint(ctx context.Context) error {
	if p.log == nil {
		return nil
	}
	p.mu.RLock()
	seq := p.seq
	cp := p.snapshotLocked()
	p.mu.RUnlock()

	// NOTHING FOLDED YET, NOTHING TO CHECKPOINT. Writing a seq-0 checkpoint would
	// be harmless but it would also overwrite a real one with an empty view if the
	// watermark were ever reset — and an empty restored view is EXEC-M21's defect.
	if seq == 0 {
		return nil
	}
	blob, err := proto.Marshal(cp)
	if err != nil {
		return fmt.Errorf("tv-sync: marshal fold checkpoint: %w", err)
	}
	return p.log.SaveCheckpoint(ctx, seq, blob)
}

// snapshotLocked serializes every account under this pod's tenant. Callers hold at
// least the read lock.
func (p *Projection) snapshotLocked() *tvsyncpb.Checkpoint {
	cp := &tvsyncpb.Checkpoint{
		TenantId: p.tenant,
		Seq:      p.seq,
		TakenAt:  timestamppb.New(p.now().UTC()),
	}
	for _, a := range p.accounts[p.tenant] {
		cp.Accounts = append(cp.Accounts, snapshotAccount(a))
	}
	return cp
}

func snapshotAccount(a *account) *tvsyncpb.AccountSnapshot {
	out := &tvsyncpb.AccountSnapshot{
		TenantId:  a.tenant,
		AccountId: a.id,
		Orders:    make(map[string]*tvsyncpb.OrderRevisions, len(a.orders)),
	}
	for _, e := range a.execs {
		out.Execs = append(out.Execs, &tvsyncpb.Execution{
			FillId: e.fillID, OrderId: e.orderID, Instrument: e.instrument, Venue: e.venue,
			Side: e.side, Qty: ratToProto(e.qty), Price: ratToProto(e.price), Fee: ratToProto(e.fee),
			Effective: timestamppb.New(e.effective), Knowledge: logTime(e.knowledge),
		})
	}
	for id, revs := range a.orders {
		rs := &tvsyncpb.OrderRevisions{Revisions: make([]*tvsyncpb.OrderRevision, 0, len(revs))}
		for _, r := range revs {
			rs.Revisions = append(rs.Revisions, &tvsyncpb.OrderRevision{
				State: r.state, Status: r.status, Reason: r.reason, Venue: r.venue,
				Effective: timestamppb.New(r.effective), Knowledge: logTime(r.knowledge),
			})
		}
		out.Orders[id] = rs
	}
	for fid := range a.seenFills {
		out.SeenFills = append(out.SeenFills, fid)
	}
	return out
}

// restore rebuilds the accounts from a checkpoint blob. Callers hold the write
// lock; Rehydrate does.
//
// IT REPLACES RATHER THAN MERGES. Restore runs before any fact is folded, so
// there is nothing to merge with — and a merge would be the more dangerous
// operation, because a partially-applied checkpoint on top of a partially-folded
// view is a state no sequence of facts could have produced.
func (p *Projection) restore(blob []byte) error {
	var cp tvsyncpb.Checkpoint
	if err := proto.Unmarshal(blob, &cp); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}
	// A CHECKPOINT FROM ANOTHER TENANT IS REFUSED, not ignored. RLS makes this
	// unreachable through the pool, so reaching it means the row was written under
	// a different scope than the one reading it — and restoring another tenant's
	// book into this pod is a cross-tenant disclosure on the surface a trader
	// reads, not merely a wrong number.
	if cp.GetTenantId() != p.tenant {
		return fmt.Errorf("%w: checkpoint is for tenant %q, this pod serves %q",
			ErrCheckpointTenantMismatch, cp.GetTenantId(), p.tenant)
	}
	for _, as := range cp.GetAccounts() {
		a, err := restoreAccount(as)
		if err != nil {
			return err
		}
		if p.accounts[a.tenant] == nil {
			p.accounts[a.tenant] = map[string]*account{}
		}
		p.accounts[a.tenant][a.id] = a
	}
	return nil
}

func restoreAccount(as *tvsyncpb.AccountSnapshot) (*account, error) {
	a := newAccount(as.GetTenantId(), as.GetAccountId())
	for _, e := range as.GetExecs() {
		qty, err := protoToRat(e.GetQty())
		if err != nil {
			return nil, fmt.Errorf("exec %s qty: %w", e.GetFillId(), err)
		}
		price, err := protoToRat(e.GetPrice())
		if err != nil {
			return nil, fmt.Errorf("exec %s price: %w", e.GetFillId(), err)
		}
		fee, err := protoToRat(e.GetFee())
		if err != nil {
			return nil, fmt.Errorf("exec %s fee: %w", e.GetFillId(), err)
		}
		a.execs = append(a.execs, execution{
			fillID: e.GetFillId(), orderID: e.GetOrderId(), instrument: e.GetInstrument(),
			venue: e.GetVenue(), side: e.GetSide(), qty: qty, price: price, fee: fee,
			effective: e.GetEffective().AsTime(), knowledge: e.GetKnowledge().AsTime(),
		})
	}
	for id, rs := range as.GetOrders() {
		revs := make([]orderRev, 0, len(rs.GetRevisions()))
		for _, r := range rs.GetRevisions() {
			revs = append(revs, orderRev{
				state: r.GetState(), status: r.GetStatus(), reason: r.GetReason(), venue: r.GetVenue(),
				effective: r.GetEffective().AsTime(), knowledge: r.GetKnowledge().AsTime(),
			})
		}
		a.orders[id] = revs
	}
	for _, fid := range as.GetSeenFills() {
		a.seenFills[fid] = true
	}
	return a, nil
}

// logTime writes a KNOWLEDGE time at the resolution the fact log gives it back.
//
// THE TWO CLOCKS ROUND-TRIP DIFFERENTLY, and that asymmetry is the whole reason
// this function exists:
//
//   - EFFECTIVE time comes out of the FACT PAYLOAD, which is stored as raw
//     protobuf bytes. A replay decodes the same bytes, so nanoseconds survive and
//     the checkpoint must preserve them too.
//   - KNOWLEDGE time comes out of the knowledge_at COLUMN, and Postgres
//     TIMESTAMPTZ is microseconds. A value that has been through tv_facts has
//     already lost its last three digits; one taken from the in-memory view has
//     not.
//
// Truncating both, or neither, makes a restored pod and a replayed pod disagree —
// in opposite directions, which is how this was found. TestACheckpointedBootReaches
// TheSameViewAsAFullReplay compares the two views field by field and failed each
// way in turn.
//
// TRUNCATION IS THE CORRECT DIRECTION FOR THIS ONE, not a convenience: the log is
// the record a rebuild is defined against, so after any restart the canonical
// knowledge time IS the microsecond one, and a checkpoint that kept more precision
// would be asserting something its own source of truth cannot express.
func logTime(t time.Time) *timestamppb.Timestamp {
	return timestamppb.New(t.Truncate(time.Microsecond))
}

// ratToProto renders a folded quantity for the checkpoint.
//
// NIL STAYS NIL. A fee that was never reported and a fee of zero are different
// facts on a P&L line, and the fold already distinguishes them — collapsing them
// here would make a restored view disagree with the one it replaced.
func ratToProto(r *big.Rat) *commonpb.Decimal {
	if r == nil {
		return nil
	}
	d, _ := dec.ToProtoScaled(r)
	return d
}

// protoToRat reads one back.
//
// IT REFUSES AN OUT-OF-DOMAIN VALUE rather than substituting one, matching
// decodeFact's rule on the fold path: a checkpoint that silently repaired a
// number would restore a book the fold could never have produced.
func protoToRat(d *commonpb.Decimal) (*big.Rat, error) {
	if d == nil {
		return nil, nil
	}
	r, ok := dec.FromProtoChecked(d)
	if !ok {
		return nil, ErrCheckpointValueOutOfDomain
	}
	return r, nil
}

// ErrCheckpointTenantMismatch: a checkpoint row was written under a different
// tenant scope than the pod reading it. RLS makes this unreachable through the
// pool, so it is a structural fault rather than an operational one.
var ErrCheckpointTenantMismatch = errors.New("tv-sync: fold checkpoint belongs to another tenant")

// ErrCheckpointValueOutOfDomain: a stored decimal is not representable. The fold
// path refuses the same shape on the way in (decodeFact), so this is a corrupted
// or hand-edited row, not a schema drift.
var ErrCheckpointValueOutOfDomain = errors.New("tv-sync: fold checkpoint holds an out-of-domain decimal")
