package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"strings"
	"time"
)

type venuePosture string

const (
	postureOKXDemo        venuePosture = "okx-demo"
	postureBinanceTestnet venuePosture = "binance-testnet"
	maxDRAttestationAge                = 24 * time.Hour
	absoluteMaxNotional                = "10"
)

type drAttestation struct {
	Environment    string    `json:"environment"`
	ClusterUID     string    `json:"cluster_uid"`
	BackupID       string    `json:"backup_id"`
	VerifiedAt     time.Time `json:"verified_at"`
	RPOSeconds     int64     `json:"rpo_seconds"`
	RTOSeconds     int64     `json:"rto_seconds"`
	RestoreProven  bool      `json:"restore_proven"`
	AuditVerified  bool      `json:"audit_verified"`
	EvidenceSHA256 string    `json:"evidence_sha256"`
}

type config struct {
	posture           venuePosture
	gatewayURL        string
	environment       string
	tenant            string
	portfolio         string
	instrument        string
	venue             string
	account           string
	quantity          string
	limitPrice        string
	maxNotional       string
	token             string
	gatewaySigningKey string
	natsURL           string
	spiffeSocket      string
	ledgerDSN         string
	dr                drAttestation
}

func validateConfig(c config, now time.Time) error {
	expectedVenue := ""
	switch c.posture {
	case postureOKXDemo:
		expectedVenue = "XOKX"
	case postureBinanceTestnet:
		expectedVenue = "XBIN"
	default:
		return fmt.Errorf("capitalpath: posture %q is not okx-demo or binance-testnet", c.posture)
	}
	if c.venue != expectedVenue {
		return fmt.Errorf("capitalpath: posture %q requires venue %q, got %q", c.posture, expectedVenue, c.venue)
	}
	u, err := url.Parse(c.gatewayURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return errors.New("capitalpath: gateway URL must be an absolute https URL")
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("capitalpath: gateway URL must contain only an https origin")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() || strings.EqualFold(u.Hostname(), "localhost") {
		return errors.New("capitalpath: gateway URL must not target a loopback host")
	}
	host := strings.ToLower(u.Hostname())
	if !isNonProductionHost(host) {
		return fmt.Errorf("capitalpath: gateway host %q does not attest a test/demo/drill environment", host)
	}
	for name, value := range map[string]string{
		"environment": c.environment,
		"tenant":      c.tenant, "portfolio": c.portfolio, "instrument": c.instrument,
		"account": c.account, "token": c.token, "gateway signing secret": c.gatewaySigningKey,
		"NATS URL": c.natsURL, "ledger DSN": c.ledgerDSN,
		"SPIFFE workload API socket": c.spiffeSocket,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("capitalpath: %s is required", name)
		}
	}
	qty, err := positiveDecimal("quantity", c.quantity)
	if err != nil {
		return err
	}
	price, err := positiveDecimal("limit price", c.limitPrice)
	if err != nil {
		return err
	}
	limit, err := positiveDecimal("maximum notional", c.maxNotional)
	if err != nil {
		return err
	}
	hardLimit, _ := new(big.Rat).SetString(absoluteMaxNotional)
	if limit.Cmp(hardLimit) > 0 {
		return fmt.Errorf("capitalpath: configured maximum notional %s exceeds compile-time hard ceiling %s", limit.RatString(), hardLimit.RatString())
	}
	if new(big.Rat).Mul(qty, price).Cmp(limit) > 0 {
		return fmt.Errorf("capitalpath: order notional %s exceeds maximum notional %s",
			new(big.Rat).Mul(qty, price).RatString(), limit.RatString())
	}
	if c.dr.Environment != c.environment {
		return fmt.Errorf("capitalpath: DR environment %q does not match target environment %q", c.dr.Environment, c.environment)
	}
	if c.dr.ClusterUID == "" || c.dr.BackupID == "" {
		return errors.New("capitalpath: DR cluster UID and backup ID are required")
	}
	if c.dr.VerifiedAt.After(now) || now.Sub(c.dr.VerifiedAt) > maxDRAttestationAge {
		return fmt.Errorf("capitalpath: DR attestation is stale or future-dated: verified_at=%s", c.dr.VerifiedAt.UTC())
	}
	if !c.dr.RestoreProven {
		return errors.New("capitalpath: DR restore is not proven")
	}
	if !c.dr.AuditVerified {
		return errors.New("capitalpath: post-restore audit verification is not proven")
	}
	if c.dr.RPOSeconds < 0 || c.dr.RPOSeconds > 60 {
		return fmt.Errorf("capitalpath: DR RPO %ds exceeds 60s", c.dr.RPOSeconds)
	}
	if c.dr.RTOSeconds < 0 || c.dr.RTOSeconds > 900 {
		return fmt.Errorf("capitalpath: DR RTO %ds exceeds 900s", c.dr.RTOSeconds)
	}
	digest, err := hex.DecodeString(c.dr.EvidenceSHA256)
	if err != nil || len(digest) != 32 {
		return errors.New("capitalpath: DR evidence digest must be 64 hexadecimal characters")
	}
	return nil
}

func isNonProductionHost(host string) bool {
	for _, label := range strings.FieldsFunc(host, func(r rune) bool { return r == '.' || r == '-' }) {
		switch label {
		case "test", "testnet", "demo", "drill":
			return true
		}
	}
	return false
}

func positiveDecimal(name, raw string) (*big.Rat, error) {
	v, ok := new(big.Rat).SetString(raw)
	if !ok || v.Sign() <= 0 {
		return nil, fmt.Errorf("capitalpath: %s %q is not a positive exact decimal", name, raw)
	}
	return v, nil
}
