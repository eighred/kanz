package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/eighred/kanz/internal/env"
	"github.com/eighred/kanz/pkg/secret"
)

const defaultCertificationTimeout = 5 * time.Minute

func loadConfig() (config, time.Duration, error) {
	token, err := secret.Read("CAPITALPATH_GATEWAY_TOKEN")
	if err != nil {
		return config{}, 0, err
	}
	signingKey, err := secret.Read("CAPITALPATH_GATEWAY_SIGNING_SECRET")
	if err != nil {
		return config{}, 0, err
	}
	dsn, err := secret.Read("CAPITALPATH_LEDGER_DSN")
	if err != nil {
		return config{}, 0, err
	}
	drPath, ok := env.Lookup("CAPITALPATH_DR_ATTESTATION_FILE")
	if !ok {
		return config{}, 0, errors.New("capitalpath: CAPITALPATH_DR_ATTESTATION_FILE is required")
	}
	drRaw, err := os.ReadFile(drPath)
	if err != nil {
		return config{}, 0, fmt.Errorf("capitalpath: read DR attestation %q: %w", drPath, err)
	}
	var dr drAttestation
	decoder := json.NewDecoder(bytes.NewReader(drRaw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&dr); err != nil {
		return config{}, 0, fmt.Errorf("capitalpath: decode DR attestation: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return config{}, 0, errors.New("capitalpath: DR attestation contains trailing JSON")
	}
	timeout, err := env.Duration("CAPITALPATH_TIMEOUT", defaultCertificationTimeout)
	if err != nil {
		return config{}, 0, err
	}
	if timeout <= 0 {
		return config{}, 0, errors.New("capitalpath: CAPITALPATH_TIMEOUT must be positive")
	}
	return config{
		posture:    venuePosture(env.Or("CAPITALPATH_POSTURE", "")),
		gatewayURL: env.Or("CAPITALPATH_GATEWAY_URL", ""), environment: env.Or("CAPITALPATH_ENVIRONMENT", ""),
		tenant: env.Or("CAPITALPATH_TENANT", ""), portfolio: env.Or("CAPITALPATH_PORTFOLIO", ""),
		instrument: env.Or("CAPITALPATH_INSTRUMENT", ""), venue: env.Or("CAPITALPATH_VENUE", ""),
		account: env.Or("CAPITALPATH_VENUE_ACCOUNT", ""), quantity: env.Or("CAPITALPATH_QUANTITY", ""),
		limitPrice: env.Or("CAPITALPATH_LIMIT_PRICE", ""), maxNotional: env.Or("CAPITALPATH_MAX_NOTIONAL", ""),
		token: token, gatewaySigningKey: signingKey, natsURL: env.Or("CAPITALPATH_NATS_URL", ""),
		spiffeSocket: env.Or("CAPITALPATH_SPIFFE_SOCKET", ""), ledgerDSN: dsn, dr: dr,
	}, timeout, nil
}
