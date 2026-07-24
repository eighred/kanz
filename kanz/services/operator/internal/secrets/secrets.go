// Package secrets is the operator's write-only venue-credential surface. It puts an
// exchange API key set into the venue's secret location (a Kubernetes Secret in dev,
// Vault KV-v2 in production) so the platform's venue adapters can mount it. It is
// WRITE-ONLY BY DESIGN: no method returns key material — presence is the only read,
// because an exchange key moves real capital and must never be reachable for read
// from a request path.
package secrets

import (
	"context"
	"fmt"
	"sort"
)

// VenueKeys is a candidate exchange key set. It is passed to a backend and never
// returned by any Store method.
type VenueKeys struct {
	APIKey     string
	APISecret  string
	Passphrase string
}

// VenueStatus is the value-blind presence report for one venue.
type VenueStatus struct {
	Venue      string
	Configured bool
}

// Store is the write-only venue-credential surface. It has NO method returning key
// material — ListVenues reports presence only. Both the k8s and Vault backends satisfy it.
type Store interface {
	SetVenueKeys(ctx context.Context, venue string, keys VenueKeys) error
	ListVenues(ctx context.Context) ([]VenueStatus, error)
}

// venueSpec records a known venue and whether the exchange uses an API passphrase
// (OKX does; Binance does not). This map is the single source of truth for both
// validation and the presence listing.
var venueSpecs = map[string]struct{ needsPassphrase bool }{
	"okx":     {needsPassphrase: true},
	"binance": {needsPassphrase: false},
}

// KnownVenues returns the supported venue ids, sorted (stable for the presence list).
func KnownVenues() []string {
	out := make([]string, 0, len(venueSpecs))
	for v := range venueSpecs {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// ValidateVenueKeys rejects an unknown venue, a missing api_key/api_secret, a
// passphrase supplied for a venue that has none, or a missing passphrase for one that
// requires it. It is called at the handler before any backend write, so a malformed
// key set never reaches a store.
func ValidateVenueKeys(venue string, k VenueKeys) error {
	spec, ok := venueSpecs[venue]
	if !ok {
		return fmt.Errorf("unknown venue %q (known: %v)", venue, KnownVenues())
	}
	if k.APIKey == "" || k.APISecret == "" {
		return fmt.Errorf("venue %q: api_key and api_secret are required", venue)
	}
	if spec.needsPassphrase && k.Passphrase == "" {
		return fmt.Errorf("venue %q requires an api passphrase", venue)
	}
	if !spec.needsPassphrase && k.Passphrase != "" {
		return fmt.Errorf("venue %q takes no passphrase", venue)
	}
	return nil
}
