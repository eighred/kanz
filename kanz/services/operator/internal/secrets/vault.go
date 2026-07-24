package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// VaultStore is the production backend: a minimal, dependency-free Vault KV-v2 writer
// (matching the codebase's hand-rolled no-vendor-SDK HTTP convention). It writes each
// venue's key set to kv/data/kanz/venue-<venue> and derives presence from a KV-v2 LIST
// of kv/metadata/kanz, which returns key NAMES, not values — so it is value-blind by
// construction, unlike the k8s backend.
type VaultStore struct {
	addr  string
	token string
	httpc *http.Client
}

func NewVaultStore(addr, token string) (*VaultStore, error) {
	if addr == "" || token == "" {
		return nil, fmt.Errorf("vault store: addr and token are required")
	}
	return &VaultStore{
		addr:  strings.TrimRight(addr, "/"),
		token: token,
		httpc: &http.Client{Timeout: 10 * time.Second},
	}, nil
}

func (s *VaultStore) SetVenueKeys(ctx context.Context, venue string, keys VenueKeys) error {
	data := map[string]string{"api_key": keys.APIKey, "api_secret": keys.APISecret}
	if keys.Passphrase != "" {
		data["api_passphrase"] = keys.Passphrase
	}
	body, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		return err
	}
	url := s.addr + "/v1/kv/data/kanz/venue-" + venue
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", s.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("vault put venue-%s: %w", venue, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The status alone — never any request body carrying key material — is surfaced.
		return fmt.Errorf("vault put venue-%s: status %d", venue, resp.StatusCode)
	}
	return nil
}

func (s *VaultStore) ListVenues(ctx context.Context) ([]VenueStatus, error) {
	url := s.addr + "/v1/kv/metadata/kanz?list=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Vault-Token", s.token)
	resp, err := s.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vault list kanz: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("vault list kanz: status %d", resp.StatusCode)
	}
	var out struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("vault list kanz: decode: %w", err)
	}
	present := map[string]bool{}
	for _, k := range out.Data.Keys {
		present[strings.TrimSuffix(k, "/")] = true
	}
	res := make([]VenueStatus, 0, len(KnownVenues()))
	for _, v := range KnownVenues() {
		res = append(res, VenueStatus{Venue: v, Configured: present["venue-"+v]})
	}
	return res, nil
}
