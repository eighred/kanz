package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/eighred/kanz/internal/dec"
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
