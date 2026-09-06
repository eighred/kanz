package main

import (
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
)

type certificationEvidence struct {
	SchemaVersion int            `json:"schema_version"`
	Result        string         `json:"result"`
	Environment   string         `json:"environment"`
	Posture       venuePosture   `json:"posture"`
	Tenant        string         `json:"tenant"`
	Portfolio     string         `json:"portfolio"`
	OrderID       string         `json:"order_id"`
	Venue         string         `json:"venue"`
	VenueAccount  string         `json:"venue_account_id"`
	Instrument    string         `json:"instrument_id"`
	StartedAt     time.Time      `json:"started_at"`
	CompletedAt   time.Time      `json:"completed_at"`
	DurationMS    int64          `json:"duration_ms"`
	DR            evidenceDR     `json:"dr_attestation"`
	Checks        []string       `json:"checks"`
	Fills         []evidenceFill `json:"fills"`
}

type evidenceDR struct {
	ClusterUID     string    `json:"cluster_uid"`
	BackupID       string    `json:"backup_id"`
	VerifiedAt     time.Time `json:"verified_at"`
	RPOSeconds     int64     `json:"rpo_seconds"`
	RTOSeconds     int64     `json:"rto_seconds"`
	EvidenceSHA256 string    `json:"evidence_sha256"`
}

type evidenceFill struct {
	FillID           string    `json:"fill_id"`
	VenueExecutionID string    `json:"venue_execution_id,omitempty"`
	Quantity         string    `json:"quantity"`
	Price            string    `json:"price"`
	ExecutedAt       time.Time `json:"executed_at"`
}

func buildEvidence(cfg config, orderID string, fills []*orderpb.Fill, started, completed time.Time) certificationEvidence {
	out := certificationEvidence{
		SchemaVersion: 1, Result: "PASS", Environment: cfg.environment, Posture: cfg.posture,
		Tenant: cfg.tenant, Portfolio: cfg.portfolio, OrderID: orderID, Venue: cfg.venue,
		VenueAccount: cfg.account, Instrument: cfg.instrument,
		StartedAt: started.UTC(), CompletedAt: completed.UTC(), DurationMS: completed.Sub(started).Milliseconds(),
		DR: evidenceDR{
			ClusterUID: cfg.dr.ClusterUID, BackupID: cfg.dr.BackupID, VerifiedAt: cfg.dr.VerifiedAt.UTC(),
			RPOSeconds: cfg.dr.RPOSeconds, RTOSeconds: cfg.dr.RTOSeconds, EvidenceSHA256: cfg.dr.EvidenceSHA256,
		},
		Checks: []string{"gateway_admission", "order_accepted", "venue_routed", "venue_filled", "accounting_commit_causation", "ledger_exact"},
		Fills:  make([]evidenceFill, 0, len(fills)),
	}
	for _, fill := range fills {
		out.Fills = append(out.Fills, evidenceFill{
			FillID: fill.GetFillId(), VenueExecutionID: fill.GetVenueExecutionId(),
			Quantity: dec.FromProto(fill.GetQuantity()).RatString(), Price: dec.FromProto(fill.GetPrice()).RatString(),
			ExecutedAt: fill.GetExecutedAt().AsTime().UTC(),
		})
	}
	return out
}
