package openlineage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// HTTPEmitter POSTs RunEvents to an OpenLineage backend (Marquez/DataHub) at
// {baseURL}/api/v1/lineage — the standard OpenLineage receive endpoint. Used
// when an OpenLineage URL is configured; otherwise the LogEmitter is the default.
type HTTPEmitter struct {
	endpoint string
	client   *http.Client
}

func NewHTTPEmitter(baseURL string, client *http.Client) *HTTPEmitter {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &HTTPEmitter{endpoint: baseURL + "/api/v1/lineage", client: client}
}

func (e *HTTPEmitter) Emit(ctx context.Context, ev RunEvent) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("openlineage: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("openlineage: post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("openlineage: backend %s", resp.Status)
	}
	return nil
}
