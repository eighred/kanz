// Command custody_batch imports one signed nonproduction statement into accounting.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/accounting/internal/config"
	"github.com/eighred/kanz/services/accounting/internal/custody"
)

func readFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("custody batch: input file unavailable")
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("custody batch: bounded regular input file required")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(b)) > limit {
		return nil, errors.New("custody batch: input exceeds size limit")
	}
	return b, err
}

func run(ctx context.Context, policyPath, batchPath, sigPath string) error {
	policy, err := readFile(policyPath, 64<<10)
	if err != nil {
		return err
	}
	var authority custody.BatchAuthority
	reader := json.NewDecoder(bytes.NewReader(policy))
	reader.DisallowUnknownFields()
	if err = reader.Decode(&authority); err != nil {
		return errors.New("custody batch: invalid public authorization policy")
	}
	if reader.Decode(new(json.RawMessage)) != io.EOF {
		return errors.New("custody batch: trailing public authorization data")
	}
	raw, err := readFile(batchPath, custody.BatchLimit)
	if err != nil {
		return err
	}
	sig, err := readFile(sigPath, 64)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if !cfg.CustodySignedBatchOnly {
		return errors.New("custody batch: signed-only accounting tenant required")
	}
	if cfg.DatabaseURL == "" || cfg.NATSURL == "" || os.Getenv("ACCOUNTING_TENANT") == "" {
		return errors.New("custody batch: explicit accounting database, tenant and broker required")
	}
	broker, err := url.Parse(cfg.NATSURL)
	if err != nil || broker.User != nil || broker.Hostname() == "" || strings.Contains(cfg.NATSURL, ",") ||
		(broker.Scheme != "nats" && broker.Scheme != "tls") || broker.Path != "" || broker.RawQuery != "" || broker.Fragment != "" {
		return errors.New("custody batch: use a broker endpoint without embedded credentials")
	}
	if cfg.SPIFFESocket == "" && !net.ParseIP(broker.Hostname()).IsLoopback() {
		return errors.New("custody batch: remote broker requires workload identity")
	}
	mesh, err := transport.NewMesh(ctx, cfg.SPIFFESocket)
	if err != nil {
		return errors.New("custody batch: workload identity unavailable")
	}
	defer mesh.Close()
	pool, err := pg.NewTenantPool(ctx, cfg.DatabaseURL, cfg.Tenant)
	if err != nil {
		return errors.New("custody batch: scoped database unavailable")
	}
	defer pool.Close()
	var restricted bool
	if err = pool.QueryRow(ctx, `SELECT current_database() = 'kanz_custody_pilot' AND NOT rolsuper AND NOT rolbypassrls
		AND pg_has_role(current_user, 'kanz_custody_batch_pilot', 'usage')
		AND NOT has_table_privilege(current_user, 'ledger_entries', 'INSERT')
		AND NOT has_table_privilege(current_user, 'custody_batch_receipts', 'UPDATE')
		FROM pg_roles WHERE rolname=current_user`).Scan(&restricted); err != nil || !restricted {
		return errors.New("custody batch: restricted database role required")
	}
	metrics := bus.NewBusMetrics(prometheus.NewRegistry())
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: custody.BatchProducer,
		InboxPrefix: custody.BatchInbox, TLSConfig: mesh.Client, Metrics: metrics, Logger: slog.Default()})
	if err != nil {
		return errors.New("custody batch: broker unavailable")
	}
	defer func() { _ = client.Close() }()
	pub, err := bus.NewProducer(client, bus.ProducerConfig{Source: custody.BatchProducer, ProducerVersion: version.String(), Tenant: cfg.Tenant, Metrics: metrics})
	if err != nil {
		return err
	}
	importer, err := custody.NewBatchImporter(authority, cfg.Tenant, cfg.CustodyPairs, cfg.CustodyAccounts, pool, pub, slog.Default())
	if err != nil {
		return err
	}
	id, err := importer.Import(bus.WithTenantID(ctx, cfg.Tenant), raw, sig)
	if err != nil {
		if id != "" {
			fmt.Fprintln(os.Stderr, id)
			return errors.New("custody batch: publication pending; retry the identical signed file")
		}
		return errors.New("custody batch: admission failed; check signature, scope, revision, retention and storage")
	}
	fmt.Println(id)
	return nil
}

func main() {
	policy := flag.String("authority", "", "operator-approved public source policy JSON")
	batch := flag.String("batch", "", "signed statement batch")
	signature := flag.String("signature", "", "detached 64-byte Ed25519 signature")
	flag.Parse()
	if *policy == "" || *batch == "" || *signature == "" || flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := run(ctx, *policy, *batch, *signature); err != nil {
		slog.Error("statement import failed", "error", err)
		os.Exit(1)
	}
}
