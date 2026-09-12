package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPortfolioBootstrapRefusesUnreviewedScopeBeforeDial(t *testing.T) {
	t.Setenv("CAPITALPATH_TENANT", "someone-else")
	t.Setenv("CAPITALPATH_PORTFOLIO", "PF1")
	t.Setenv("CAPITALPATH_NATS_URL", "nats://unreachable")
	t.Setenv("CAPITALPATH_SPIFFE_SOCKET", "unix:///unreachable")
	err := runPortfolioBootstrap(context.Background(), time.Now)
	if err == nil || !strings.Contains(err.Error(), "must be __system__") {
		t.Fatalf("unreviewed tenant was not refused before network access: %v", err)
	}
}

func TestPortfolioBootstrapRefusesUnreviewedPortfolioBeforeDial(t *testing.T) {
	t.Setenv("CAPITALPATH_TENANT", "__system__")
	t.Setenv("CAPITALPATH_PORTFOLIO", "PF2")
	t.Setenv("CAPITALPATH_NATS_URL", "nats://unreachable")
	t.Setenv("CAPITALPATH_SPIFFE_SOCKET", "unix:///unreachable")
	err := runPortfolioBootstrap(context.Background(), time.Now)
	if err == nil || !strings.Contains(err.Error(), "must be PF1") {
		t.Fatalf("unreviewed portfolio was not refused before network access: %v", err)
	}
}
