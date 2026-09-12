package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/eighred/kanz/internal/orderid"
)

func main() {
	if os.Getenv("CAPITALPATH_MODE") == "portfolio-bootstrap" {
		if err := runPortfolioBootstrap(context.Background(), time.Now); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	evidence, err := run(context.Background(), time.Now)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(evidence); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "capitalpath: encode evidence:", err)
		os.Exit(1)
	}
}

func run(parent context.Context, now func() time.Time) (certificationEvidence, error) {
	if now == nil {
		now = time.Now
	}
	cfg, timeout, err := loadConfig()
	if err != nil {
		return certificationEvidence{}, err
	}
	started := now().UTC()
	if err := validateConfig(cfg, started); err != nil {
		return certificationEvidence{}, err
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	orderID := orderid.Mint()
	collector := newFactCollector(cfg, orderID)
	watch, err := watchCapitalFacts(ctx, cfg, collector)
	if err != nil {
		return certificationEvidence{}, err
	}
	defer watch.close()

	baseTransport := http.DefaultTransport.(*http.Transport).Clone()
	baseTransport.ForceAttemptHTTP2 = true
	baseTransport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13}
	client := newGatewayClient(&http.Client{Transport: baseTransport, Timeout: 30 * time.Second})

	mayBeLive := true
	cleanup := func(cause error) error {
		if !mayBeLive {
			return cause
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if cancelErr := client.cancel(cleanupCtx, cfg, orderID); cancelErr != nil {
			return errors.Join(cause, fmt.Errorf("capitalpath: FAIL-CLOSED cleanup also failed: %w", cancelErr))
		}
		return cause
	}

	if err := client.submitKnown(ctx, cfg, orderID); err != nil {
		return certificationEvidence{}, cleanup(err)
	}
	fills, err := collector.await(ctx)
	if err != nil {
		return certificationEvidence{}, cleanup(fmt.Errorf("capitalpath: await committed FACT lineage for %s: %w", orderID, err))
	}
	// ORDER_FILLED is terminal, and the causally linked accounting FACT proves
	// its ledger transaction committed. A cleanup cancel is no longer necessary.
	mayBeLive = false
	if err := verifyLedger(ctx, cfg, fills); err != nil {
		return certificationEvidence{}, err
	}
	completed := now().UTC()
	return buildEvidence(cfg, orderID, fills, started, completed), nil
}
