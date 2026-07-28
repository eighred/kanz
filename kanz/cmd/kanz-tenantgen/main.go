// Command kanz-tenantgen renders a tenant's OMS compute manifest from
// infra/deploy/oms-deploy.yaml (see internal/tenantgen) and writes it to
// infra/deploy/tenants/<tenant>/oms-<tenant>.yaml.
//
// It is the ONLY way a tenant's compute manifest is produced — by hand, via
// provision-tenant.sh's `compute` step, or on regeneration after the base
// changes. Both this tool and test/arch/tenant_compute_test.go's drift guard
// call internal/tenantgen.Render; hand-editing the rendered file instead
// works locally and fails in CI, because the guard re-renders every
// committed tenant manifest from the live base and diffs it byte-for-byte.
//
//	go run ./cmd/kanz-tenantgen -tenant acme
//	go run ./cmd/kanz-tenantgen -tenant acme -base infra/deploy/oms-deploy.yaml -out infra/deploy/tenants/acme/oms-acme.yaml
//
// This tool never applies anything to a cluster — it only writes a file.
// Committing that file to main is what deploys it: infra/deploy/ is
// raw-synced by the ApplicationSet's "workloads" component with
// prune:true/selfHeal:true, so an out-of-band kubectl apply would come up
// and then be pruned within minutes because it does not exist in git.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/eighred/kanz/internal/tenantgen"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "kanz-tenantgen: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("kanz-tenantgen", flag.ContinueOnError)
	tenant := fs.String("tenant", "", "tenant id (lowercase, dns-safe) — required")
	base := fs.String("base", "infra/deploy/oms-deploy.yaml", "path to the OMS base manifest")
	out := fs.String("out", "", "output path (default infra/deploy/tenants/<tenant>/oms-<tenant>.yaml)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tenant == "" {
		return fmt.Errorf("-tenant is required")
	}

	outPath := *out
	if outPath == "" {
		outPath = filepath.Join("infra", "deploy", "tenants", *tenant, "oms-"+*tenant+".yaml")
	}

	baseBytes, err := os.ReadFile(*base)
	if err != nil {
		return fmt.Errorf("read base %s: %w", *base, err)
	}

	rendered, err := tenantgen.Render(baseBytes, *tenant)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(outPath), err)
	}
	if err := os.WriteFile(outPath, rendered, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", outPath, err)
	}
	fmt.Printf("wrote %s\n", outPath)
	return nil
}
