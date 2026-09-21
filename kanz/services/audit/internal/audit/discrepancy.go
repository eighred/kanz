package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/internal/fillfact"
	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
)

const KindVenueDiscrepancy Kind = "venue_discrepancy"

// These observations lack execution-level economics and account attribution.
// Record evidence for investigation; never interpret their deltas as journal
// commands or reopen a terminal order from a cumulative quantity.
func discrepancy(env *envelopepb.Envelope, payload []byte) (classification, bool) {
	if env.GetEventType() == "order.order.fee_correction_proposed" || env.GetEventType() == "order.order.fee_correction_approved" {
		return feeCorrectionEvidence(env, payload), true
	}
	if env.GetEventType() == "order.order.recovery_recorded" {
		return recoveryDiscrepancy(env, payload), true
	}
	kind := ""
	switch env.GetEventType() {
	case "order.order.healed":
		kind = "order"
	case "accounting.balance.reconciled":
		kind = "balance"
	default:
		return classification{}, false
	}
	digest := sha256.Sum256(payload)
	attrs := map[string]string{"discrepancy_type": kind, "disposition": "investigate", "evidence_status": "invalid", "payload_sha256": hex.EncodeToString(digest[:]), "scope_status": "unverified"}
	c := classification{KindVenueDiscrepancy, "Venue discrepancy requires investigation; no ledger or order correction applied", attrs}
	if len(payload) > 1<<20 || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT {
		return c, true
	}
	if kind == "order" {
		var msg orderpb.StateHealed
		if proto.Unmarshal(payload, &msg) != nil || msg.State == nil || msg.OrderId == "" || msg.OrderId != msg.State.OrderId || msg.Venue == "" || msg.DetectedAt == nil || msg.DetectedAt.CheckValid() != nil {
			return c, true
		}
		if _, ok := dec.InDomainDeep(&msg); !ok {
			return c, true
		}
		filled, ok := exactEvidence(msg.State.FilledQuantity)
		if !ok || strings.HasPrefix(filled, "-") {
			return c, true
		}
		if _, ok = orderpb.OrderStatus_name[int32(msg.State.Status)]; !ok || msg.State.Status == orderpb.OrderStatus_ORDER_STATUS_UNSPECIFIED {
			return c, true
		}
		attrs["order_id"] = msg.OrderId
		attrs["venue"] = msg.Venue
		attrs["reported_portfolio_id"] = msg.State.PortfolioId
		attrs["instrument_id"] = msg.State.InstrumentId
		attrs["order_status"] = strings.TrimPrefix(msg.State.Status.String(), "ORDER_STATUS_")
		attrs["filled_quantity"] = filled
		attrs["detected_at"] = msg.DetectedAt.AsTime().UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
	} else {
		var msg accountingpb.BalanceReconciled
		if proto.Unmarshal(payload, &msg) != nil || msg.Expected == nil || msg.Actual == nil || msg.Delta == nil || msg.Venue == "" || msg.Asset == "" || msg.DetectedAt == nil || msg.DetectedAt.CheckValid() != nil {
			return c, true
		}
		expected, eok := dec.FromProtoChecked(msg.Expected)
		actual, aok := dec.FromProtoChecked(msg.Actual)
		delta, dok := dec.FromProtoChecked(msg.Delta)
		if !eok || !aok || !dok || expected.Add(expected, delta).Cmp(actual) != 0 {
			return c, true
		}
		for key, value := range map[string]*commonpb.Decimal{"expected": msg.Expected, "actual": msg.Actual, "delta": msg.Delta} {
			text, ok := exactEvidence(value)
			if !ok {
				return c, true
			}
			attrs[key] = text
		}
		attrs["venue"] = msg.Venue
		attrs["asset"] = msg.Asset
		// Older publishers put tenant_id here. Preserve what was reported but do
		// not mislabel it as a verified portfolio/account assignment.
		attrs["reported_portfolio_id"] = msg.PortfolioId
		attrs["detected_at"] = msg.DetectedAt.AsTime().UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
	}
	for _, v := range attrs {
		if len(v) > 1024 || strings.ContainsRune(v, '\x00') {
			return classification{KindVenueDiscrepancy, c.summary, map[string]string{"discrepancy_type": kind, "disposition": "investigate", "evidence_status": "invalid", "payload_sha256": hex.EncodeToString(digest[:]), "scope_status": "unverified"}}, true
		}
	}
	attrs["evidence_status"] = "observed"
	return c, true
}

func feeCorrectionEvidence(env *envelopepb.Envelope, payload []byte) classification {
	digest := sha256.Sum256(payload)
	attrs := map[string]string{"discrepancy_type": "execution_fee", "evidence_status": "invalid", "payload_sha256": hex.EncodeToString(digest[:])}
	c := classification{KindVenueDiscrepancy, "Execution fee correction evidence", attrs}
	if len(payload) > 512<<10 || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT {
		return c
	}
	if env.GetEventType() == "order.order.fee_correction_proposed" {
		var p orderpb.ExecutionFeeCorrectionProposed
		if proto.Unmarshal(payload, &p) != nil || p.CaseId == "" || p.OrderId == "" || p.PortfolioId == "" || len(p.Changes) == 0 {
			return c
		}
		actual, err := fillfact.FeeProposalDigest(env.GetTenantId(), &p)
		if err != nil || actual != p.Digest {
			return c
		}
		attrs["case_id"], attrs["order_id"], attrs["portfolio_id"], attrs["approval_digest"], attrs["proposer"], attrs["disposition"] = p.CaseId, p.OrderId, p.PortfolioId, p.Digest, p.Proposer, "awaiting_approval"
	} else {
		var a orderpb.ExecutionFeeCorrectionApproved
		if proto.Unmarshal(payload, &a) != nil || a.CaseId == "" || a.OrderId == "" || a.PortfolioId == "" || strings.TrimSpace(a.Proposer) == "" || strings.TrimSpace(a.Approver) == "" || dualcontrol.SameSubject(a.Proposer, a.Approver) || a.ApprovedAt == nil || a.ApprovedAt.CheckValid() != nil {
			return c
		}
		d, err := hex.DecodeString(a.Digest)
		if err != nil || len(d) != 32 {
			return c
		}
		attrs["case_id"], attrs["order_id"], attrs["portfolio_id"], attrs["approval_digest"], attrs["proposer"], attrs["approver"], attrs["disposition"] = a.CaseId, a.OrderId, a.PortfolioId, a.Digest, a.Proposer, a.Approver, "approved_pending_book_proof"
	}
	attrs["evidence_status"] = "valid"
	return c
}

func recoveryDiscrepancy(env *envelopepb.Envelope, payload []byte) classification {
	digest := sha256.Sum256(payload)
	attrs := map[string]string{"discrepancy_type": "execution_recovery", "disposition": "investigate", "evidence_status": "invalid", "payload_sha256": hex.EncodeToString(digest[:]), "scope_status": "unverified"}
	c := classification{KindVenueDiscrepancy, "Invalid execution recovery lifecycle evidence requires investigation", attrs}
	if len(payload) > 1<<20 || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT {
		return c
	}
	var fact orderpb.ExecutionRecoveryRecorded
	if proto.Unmarshal(payload, &fact) != nil || fact.GetCaseId() == "" || fact.GetOrderId() == "" || fact.GetRecordedAt() == nil || fact.GetRecordedAt().CheckValid() != nil || fact.GetSourceCursor() == "" {
		return c
	}
	if bytes, err := hex.DecodeString(fact.GetPayloadDigest()); err != nil || len(bytes) != 32 {
		return c
	}
	switch fact.GetStatus() {
	case "observed", "investigating", "blocked":
	case "corrected":
		if fact.GetMappingVersion() == "" || fact.GetPortfolioId() == "" || fact.GetVenueAccountId() == "" {
			return c
		}
	default:
		return c
	}
	for _, value := range []string{fact.CaseId, fact.OrderId, fact.PortfolioId, fact.Venue, fact.VenueAccountId, fact.MappingVersion, fact.SourceCursor, fact.Reason} {
		if len(value) > 2048 || strings.ContainsRune(value, '\x00') {
			return c
		}
	}
	attrs["case_id"], attrs["order_id"], attrs["portfolio_id"] = fact.CaseId, fact.OrderId, fact.PortfolioId
	attrs["venue"], attrs["venue_account_id"], attrs["mapping_version"] = fact.Venue, fact.VenueAccountId, fact.MappingVersion
	attrs["source_cursor"], attrs["source_payload_sha256"] = fact.SourceCursor, fact.PayloadDigest
	attrs["disposition"], attrs["evidence_status"], attrs["reason"] = fact.Status, "observed", fact.Reason
	attrs["checkpoint"] = fmt.Sprint(fact.Checkpoint)
	if fact.MappingVersion != "" {
		attrs["scope_status"] = "verified_mapping"
	}
	c.summary = "Execution recovery " + fact.Status
	return c
}

func exactEvidence(d *commonpb.Decimal) (string, bool) {
	if d == nil {
		return "", false
	}
	r, ok := dec.FromProtoChecked(d)
	if !ok {
		return "", false
	}
	return r.RatString(), true
}
