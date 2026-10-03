package cashview

// CommitRecord preserves the accounting transaction's original protobuf bytes.
// JSON uses strings for int64 cursors and base64 for the opaque payload so a
// client cannot round a revision or reconstruct financial evidence from JSON.
type CommitRecord struct {
	Revision int64  `json:"revision,string"`
	Payload  []byte `json:"payload"`
}

// CommitPage is a contiguous bounded page pinned to one committed source head.
// Reading it neither clears source quarantine nor authorizes an OMS admission.
type CommitPage struct {
	TenantID    string         `json:"tenant_id"`
	PortfolioID string         `json:"portfolio_id"`
	Currency    string         `json:"currency"`
	Through     int64          `json:"through_revision,string"`
	Next        int64          `json:"next_revision,string"`
	HasMore     bool           `json:"has_more"`
	Records     []CommitRecord `json:"records"`
}
